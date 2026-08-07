// Package agentimport imports a coding agent's pre-existing local transcripts
// into Entire as read-only, commit-less checkpoints on the v1 metadata branch.
//
// The orchestration (discovery loop, idempotent per-turn IDs, redaction, and
// the checkpoint write) is agent-agnostic and lives here. Each agent plugs in
// an Importer that knows where its transcripts live and how to split one into
// per-user-prompt turns. Claude Code is the only implementation today
// (see claude.go); others register themselves the same way.
package agentimport

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"sort"
	"time"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"

	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	cp "github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/redact"
)

// DefaultLookbackDays bounds how far back import reaches when the caller does
// not set Options.LookbackDays. The effective window applies uniformly to
// transcript discovery, the anchor walk, and the reconcile commit scan — a
// commit outside it can never be linked to a turn outside it.
const DefaultLookbackDays = 30

// SessionFile is one discovered agent transcript for a repo.
type SessionFile struct {
	Path      string // absolute path to the transcript file
	SessionID string // agent session id (used in checkpoint metadata)
}

// Turn is one user-prompt turn extracted from a session transcript. Line
// offsets are in raw-line space (newline-counted), matching transcript slicing
// and the agent token-usage helpers.
type Turn struct {
	LineStart, LineEnd int
	UUID               string
	Prompt, Model      string
	CreatedAt          time.Time
	// Tokens is this turn's token usage. Every field is a per-turn delta:
	// main-agent fields are scoped to the turn's [LineStart, LineEnd) slice by
	// the token helpers, and SubagentTokens is rescoped from the cumulative
	// snapshot those helpers return to a per-turn increment by
	// rescopeSubagentTokensToDeltas (see linesplit.go). That invariant lets
	// callers sum turns freely: writeSessionState sums them for the session
	// total and each imported checkpoint stores its own turn's delta, so a
	// subagent's tokens are counted exactly once rather than re-added on every
	// turn after it is discovered.
	Tokens *types.TokenUsage
	// CommitSHAs are the commit SHAs (possibly abbreviated) this turn's
	// transcript records creating, in transcript order. Extraction is
	// per-importer (see commitSHAsInRange for Claude Code). Callers must
	// resolve them against the repo before use. Nil when the transcript
	// records no commits (including agents that don't extract them); the
	// anchor then falls back to Options.LinkCommitSHA.
	CommitSHAs []string
}

// Importer is the per-agent seam: it locates an agent's transcripts for a repo
// and splits one into per-turn units. Everything else (idempotency, redaction,
// writing) is handled generically by Run.
type Importer interface {
	// Name is the registry key and the provenance source (e.g. "claude-code").
	Name() string
	// AgentType is the display name stored in checkpoint metadata (e.g. "Claude Code").
	AgentType() types.AgentType
	// Discover returns the agent's transcript files for the repo, dropping
	// transcripts last modified before cutoff (the start of the run's lookback
	// window, computed once by Run so every importer bounds identically).
	// overridePath replaces the default transcript dir; sessionFilter, when
	// non-empty, keeps only matching session IDs.
	Discover(repoRoot, overridePath string, cutoff time.Time, sessionFilter []string) ([]SessionFile, error)
	// SplitTurns splits one session's raw transcript bytes into per-turn units.
	SplitTurns(sf SessionFile, full []byte) ([]Turn, error)
}

// importers is the static set of supported agents. Adding an agent is a new
// Importer implementation appended here — no init() / runtime registration.
var importers = []Importer{
	claudeImporter{},
	cursorImporter{},
	piImporter{},
	factoryImporter{},
	codexImporter{},
	copilotImporter{},
	geminiImporter{},
}

// All returns every supported importer, sorted by name.
func All() []Importer {
	out := append([]Importer(nil), importers...)
	sort.Slice(out, func(i, j int) bool { return out[i].Name() < out[j].Name() })
	return out
}

// Options configures an import run.
type Options struct {
	RepoRoot      string
	OverridePath  string
	SessionFilter []string
	Now           time.Time
	DryRun        bool

	// LookbackDays bounds how far back the run reaches — transcript
	// discovery, the anchor ancestor walk, and the reconcile commit scan all
	// derive their cutoff from it. 0 means DefaultLookbackDays.
	LookbackDays int

	// LinkCommitSHA, when non-empty, is the fallback anchor written to each
	// imported checkpoint's metadata as commit_sha — the commit the UI shows
	// imported sessions against. The caller resolves it (default branch head
	// when resolvable; see resolveImportLinkCommitSHA); Run does not. A turn
	// whose transcript records a resolvable commit that is an ancestor of this
	// fallback anchors to that real commit instead (see turnAnchorResolver).
	LinkCommitSHA string

	// Reconcile, when non-nil and Enabled, makes commit linking first-class:
	// Run scans ScanTips for commits with no session data, links the turns it
	// can, backfills links onto already-imported checkpoints, and reports the
	// rest. Nil (the default) is off — Run behaves exactly as before.
	Reconcile *ReconcileOptions

	// ScanTips are the commit tips the reconcile scan walks (typically
	// origin/<default>, the local default branch, and HEAD). The CLI layer
	// resolves them; Run builds the missing-commit map itself so the package
	// stays testable without a ref-resolution seam. Ignored when Reconcile is
	// off.
	ScanTips []plumbing.Hash

	// Progress, when non-nil, receives session/turn progress notifications
	// as Run executes. Nil (the default) reports nothing.
	Progress *Progress
}

// reconciling reports whether this run should scan commits and link them.
func (o Options) reconciling() bool { return o.Reconcile != nil && o.Reconcile.Enabled }

// acceptingHeuristics reports whether time-window matches may be written
// rather than only reported.
func (o Options) acceptingHeuristics() bool {
	return o.reconciling() && o.Reconcile.AcceptHeuristics
}

// lookbackDays returns the effective lookback window in days.
func (o Options) lookbackDays() int {
	if o.LookbackDays <= 0 {
		return DefaultLookbackDays
	}
	return o.LookbackDays
}

// now returns the run's reference time (wall clock when unset).
func (o Options) now() time.Time {
	if o.Now.IsZero() {
		return time.Now()
	}
	return o.Now
}

// Result summarizes an import run.
type Result struct {
	SessionsScanned int
	TurnsImported   int
	TurnsSkipped    int

	// LinksRecorded counts links whose commit the transcript itself recorded.
	LinksRecorded int
	// LinksHeuristic counts accepted time-window links.
	LinksHeuristic int
	// Backfilled counts already-imported checkpoints whose link was upgraded.
	Backfilled int

	// Report is the reconcile pass's full outcome; nil when not reconciling.
	Report *ReconcileReport
}

// Progress reports observable events as Run walks sessions and writes turns.
// It is UI-agnostic: Run never prints or logs on its behalf, so rendering
// (a progress bar, log lines, a TUI) is entirely up to the caller. Every
// field is optional, and a nil *Progress (the default) is a no-op — Run's
// behavior is byte-identical whether or not one is supplied.
//
// Invariant: for every turn Run processes, exactly one of TurnWritten or
// TurnSkipped fires — so summing both callbacks' calls across one session
// always equals that session's turnCount (as reported by SessionStart).
type Progress struct {
	// SessionStart fires once per session, after its transcript has been
	// split into turns and before any of them are written. sessionIndex is
	// 0-based against sessionTotal, the number of sessions Discover
	// returned; turnCount is the number of turns split from this session.
	SessionStart func(sessionIndex, sessionTotal int, agentName, sessionID string, turnCount int)
	// TurnWritten fires once per turn Run actually writes to the checkpoint
	// store — never for a turn skipped as already-imported, nor under
	// DryRun (see TurnSkipped for those). turnIndex is 0-based against
	// turnCount, matching the turnCount reported by this turn's
	// SessionStart call.
	TurnWritten func(sessionIndex, turnIndex, turnCount int)
	// TurnSkipped fires once per turn Run processes without writing: a turn
	// already imported (idempotent re-run) or, under DryRun, every turn
	// (dry runs never write). Index semantics match TurnWritten exactly.
	TurnSkipped func(sessionIndex, turnIndex, turnCount int)
}

func (p *Progress) sessionStart(sessionIndex, sessionTotal int, agentName, sessionID string, turnCount int) {
	if p == nil || p.SessionStart == nil {
		return
	}
	p.SessionStart(sessionIndex, sessionTotal, agentName, sessionID, turnCount)
}

func (p *Progress) turnWritten(sessionIndex, turnIndex, turnCount int) {
	if p == nil || p.TurnWritten == nil {
		return
	}
	p.TurnWritten(sessionIndex, turnIndex, turnCount)
}

func (p *Progress) turnSkipped(sessionIndex, turnIndex, turnCount int) {
	if p == nil || p.TurnSkipped == nil {
		return
	}
	p.TurnSkipped(sessionIndex, turnIndex, turnCount)
}

// DeriveCheckpointID produces a stable 12-hex checkpoint ID for an imported
// turn. Re-importing the same (sessionID, turnUUID) yields the same ID, which
// is how import stays idempotent.
func DeriveCheckpointID(sessionID, turnUUID string) id.CheckpointID {
	sum := sha256.Sum256([]byte(sessionID + "/" + turnUUID))
	return id.MustCheckpointID(hex.EncodeToString(sum[:6])) // 6 bytes = 12 lowercase hex chars
}

// Run imports the given agent's transcripts (within the lookback window) as
// read-only checkpoints on the v1 metadata branch (Kind "imported"). It is
// idempotent: turns whose deterministic ID already exists are skipped.
//
// With Options.Reconcile enabled it additionally scans Options.ScanTips for
// commits carrying no session data, links the turns it can (transcript-recorded
// links always; time-window guesses only under AcceptHeuristics), backfills
// links onto checkpoints imported by an earlier run, and reports the rest in
// Result.Report. Commits are never rewritten — the link lives on the
// checkpoint. Re-running converges: every link becomes "unchanged" and no write
// is issued.
func Run(ctx context.Context, repo *git.Repository, imp Importer, opts Options) (Result, error) {
	var res Result
	cutoff := opts.now().AddDate(0, 0, -opts.lookbackDays())
	files, err := imp.Discover(opts.RepoRoot, opts.OverridePath, cutoff, opts.SessionFilter)
	if err != nil {
		return res, fmt.Errorf("discover %s sessions: %w", imp.Name(), err)
	}

	stores, err := cp.Open(ctx, repo, cp.OpenOptions{})
	if err != nil {
		return res, fmt.Errorf("open checkpoint store: %w", err)
	}
	existing := make(map[string]bool)
	if infos, listErr := stores.Persistent.List(ctx); listErr == nil {
		for _, in := range infos {
			existing[in.CheckpointID.String()] = true
		}
	}

	// One resolver per Run: it lazily walks and memoizes opts.LinkCommitSHA's
	// ancestor set the first time a turn actually carries a candidate, so a
	// whole import run pays for at most one history walk, not one per turn.
	anchorResolver := newTurnAnchorResolver(repo, opts.LinkCommitSHA, opts.now(), opts.lookbackDays())

	rec, err := newReconciler(ctx, repo, opts, cutoff)
	if err != nil {
		return res, err
	}
	// Scanned commits widen the anchor resolver's reachability gate: a commit
	// on an unmerged feature branch is unreachable from the default-branch
	// fallback, yet it IS in the scan set, so a turn that recorded it links
	// exactly instead of falling back to the tip.
	anchorResolver.extraAccept = rec.scanned()

	// Resolve the importer's git identity once per run (not per turn):
	// checkpoint commits otherwise carry an empty author, which the
	// GitHub->mirror ingestion path falls back to when it has no pusher
	// identity for imported sessions, leaving them unattributed.
	authorName, authorEmail := cp.GetGitAuthorFromRepo(repo)

	for sessionIndex, sf := range files {
		res.SessionsScanned++
		full, readErr := os.ReadFile(sf.Path)
		if readErr != nil {
			return res, fmt.Errorf("read %s: %w", sf.Path, readErr)
		}
		turns, splitErr := imp.SplitTurns(sf, full)
		if splitErr != nil {
			return res, fmt.Errorf("split %s session %s: %w", imp.Name(), sf.SessionID, splitErr)
		}
		opts.Progress.sessionStart(sessionIndex, len(files), string(imp.AgentType()), sf.SessionID, len(turns))

		// Heuristic matches are computed once per session, against the commits
		// still unlinked at this point, so an earlier session's link can't be
		// proposed again here.
		heuristics := rec.matchSession(turns)

		// Redact the session transcript once and reuse it for every turn's
		// checkpoint (each turn stores the full session transcript with its own
		// CheckpointTranscriptStart). Redacting per turn would be O(turns).
		// Computed lazily so a fully-skipped or dry-run file pays nothing.
		var red redact.RedactedBytes
		redacted := false
		for turnIndex, turn := range turns {
			cid := DeriveCheckpointID(sf.SessionID, turn.UUID)
			alreadyImported := existing[cid.String()]

			// Anchor resolution is skipped entirely when nothing will consume
			// it (an already-imported or dry-run turn in a non-reconcile run),
			// preserving the pre-reconcile cost profile.
			var anchor, method string
			var heuristicOnly bool
			if rec.enabled() || (!alreadyImported && !opts.DryRun) {
				anchor, method = anchorResolver.resolve(ctx, turn.CommitSHAs)
				if len(turn.CommitSHAs) > 0 && method != cp.CommitSHAMethodRecorded {
					logging.Debug(ctx, "import: turn anchor fell back",
						"sessionID", sf.SessionID, "turnUUID", turn.UUID, "candidates", len(turn.CommitSHAs))
				}
				anchor, method, heuristicOnly = rec.applyHeuristic(heuristics, turn.UUID, anchor, method)
			}
			link := LinkResult{
				CommitSHA: anchor, CheckpointID: cid, SessionID: sf.SessionID,
				TurnUUID: turn.UUID, Method: method,
			}

			if alreadyImported {
				res.TurnsSkipped++
				opts.Progress.turnSkipped(sessionIndex, turnIndex, len(turns))
				rec.recordExisting(ctx, stores, link, heuristicOnly, &res)
				continue
			}
			if opts.DryRun {
				res.TurnsImported++ // counts what would import
				opts.Progress.turnSkipped(sessionIndex, turnIndex, len(turns))
				rec.record(link, heuristicOnly, ActionDryRun, &res)
				continue
			}
			if !redacted {
				// Sanitize before redacting, like every other path that stores a
				// transcript. Import reads raw third-party rollouts, so for Codex
				// sessions this is where the encrypted payloads would otherwise be
				// handed to the redaction layers — which then scan megabytes of
				// base64 ciphertext only for the store to discard it.
				r, rerr := redact.JSONLBytes(cp.SanitizeTranscriptForAgentType(imp.AgentType(), full))
				if rerr != nil {
					return res, fmt.Errorf("redact %s transcript: %w", sf.SessionID, rerr)
				}
				red = r
				redacted = true
			}
			// A heuristic match that wasn't accepted is reported, never
			// written: the checkpoint keeps the fallback anchor.
			writeAnchor, writeMethod := anchor, method
			if heuristicOnly {
				writeAnchor, writeMethod = anchorResolver.fallbackAnchor()
			}
			if err := writeTurn(ctx, stores, imp, cid, sf, red, turn, writeAnchor, writeMethod, authorName, authorEmail); err != nil {
				return res, err
			}
			existing[cid.String()] = true
			res.TurnsImported++
			opts.Progress.turnWritten(sessionIndex, turnIndex, len(turns))
			rec.record(link, heuristicOnly, ActionWritten, &res)
		}

		// Track A: surface this session in `entire session list`. Best-effort —
		// the read-only checkpoints above are the primary artifact, so a
		// state-write failure must not abort the import.
		if !opts.DryRun {
			if serr := writeSessionState(ctx, imp, sf, turns); serr != nil {
				logging.Debug(ctx, "import: failed to write imported session state",
					"sessionID", sf.SessionID, "error", serr.Error())
			}
		}
	}
	res.Report = rec.finish()
	return res, nil
}

// writeSessionState upserts a local session.State so an imported session shows
// up in `entire session list`. It is Kind-gated (KindImported), never sets
// BaseCommit (imports are commit-less and must not be pinned to HEAD), and uses
// the transcript's own timestamps — the forward-compat contract that keeps a
// later commit-SHA link purely additive. It never clobbers a live or
// manually-attached session that happens to share the ID.
func writeSessionState(ctx context.Context, imp Importer, sf SessionFile, turns []Turn) error {
	if len(turns) == 0 {
		return nil
	}
	store, err := session.NewStateStore(ctx)
	if err != nil {
		return fmt.Errorf("open session state store: %w", err)
	}
	if existing, lerr := store.Load(ctx, sf.SessionID); lerr == nil && existing != nil &&
		!existing.Kind.IsImported() {
		return nil // don't overwrite a real (live/attached) session
	}

	var started, ended time.Time
	var tokens *types.TokenUsage
	model := ""
	for _, turn := range turns {
		if !turn.CreatedAt.IsZero() {
			if started.IsZero() || turn.CreatedAt.Before(started) {
				started = turn.CreatedAt
			}
			if turn.CreatedAt.After(ended) {
				ended = turn.CreatedAt
			}
		}
		if turn.Model != "" {
			model = turn.Model
		}
		// turn.Tokens holds per-turn deltas for every field, including
		// SubagentTokens (rescoped from a cumulative snapshot in
		// rescopeSubagentTokensToDeltas — see the Turn.Tokens doc). Summing
		// them therefore yields the correct session total: main-agent fields
		// add up, and the subagent deltas sum back to the final cumulative
		// subagent snapshot exactly once instead of being multiplied by the
		// number of turns after each subagent was first discovered.
		tokens = types.AddTokenUsage(tokens, turn.Tokens)
	}
	if started.IsZero() {
		// No usable per-turn timestamps (some Codex lines): fall back to the
		// transcript file mtime so the row still sorts sensibly. Never "now".
		if fi, statErr := os.Stat(sf.Path); statErr == nil {
			started = fi.ModTime()
			ended = started
		}
	}
	state := &session.State{
		SessionID:           sf.SessionID,
		Kind:                session.KindImported,
		AgentType:           imp.AgentType(),
		ModelName:           model,
		StartedAt:           started,
		EndedAt:             &ended,
		Phase:               session.PhaseEnded,
		LastInteractionTime: &ended,
		StepCount:           len(turns),
		TokenUsage:          tokens,
		LastPrompt:          session.TruncatePromptForStorage(turns[len(turns)-1].Prompt),
		LastCheckpointID:    DeriveCheckpointID(sf.SessionID, turns[len(turns)-1].UUID),
	}
	if err := store.Save(ctx, state); err != nil {
		return fmt.Errorf("save imported session state %s: %w", sf.SessionID, err)
	}
	return nil
}

func writeTurn(ctx context.Context, stores *cp.Stores, imp Importer, cid id.CheckpointID, sf SessionFile, red redact.RedactedBytes, turn Turn, anchorCommitSHA, anchorMethod, authorName, authorEmail string) error {
	if err := stores.Persistent.Write(ctx, cp.Session(cp.WriteOptions{
		CheckpointID:              cid,
		SessionID:                 sf.SessionID,
		CreatedAt:                 turn.CreatedAt,
		Strategy:                  "import",
		Kind:                      string(session.KindImported),
		Agent:                     imp.AgentType(),
		Model:                     turn.Model,
		Transcript:                red,
		Prompts:                   []string{turn.Prompt},
		CheckpointsCount:          1,
		CheckpointTranscriptStart: turn.LineStart,
		TokenUsage:                turn.Tokens,
		CommitSHA:                 anchorCommitSHA,
		CommitSHAMethod:           anchorMethod,
		AuthorName:                authorName,
		AuthorEmail:               authorEmail,
	})); err != nil {
		return fmt.Errorf("write imported checkpoint %s: %w", cid, err)
	}
	return nil
}
