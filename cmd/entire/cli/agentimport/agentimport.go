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

	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	cp "github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/redact"
)

// LookbackDays bounds how far back import reaches. Fixed this pass (no flag).
const LookbackDays = 30

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
	Tokens             *types.TokenUsage
}

// Importer is the per-agent seam: it locates an agent's transcripts for a repo
// and splits one into per-turn units. Everything else (idempotency, redaction,
// writing) is handled generically by Run.
type Importer interface {
	// Name is the registry key and the provenance source (e.g. "claude-code").
	Name() string
	// AgentType is the display name stored in checkpoint metadata (e.g. "Claude Code").
	AgentType() types.AgentType
	// Discover returns the agent's transcript files for the repo within the
	// lookback window. overridePath replaces the default transcript dir;
	// sessionFilter, when non-empty, keeps only matching session IDs.
	Discover(repoRoot, overridePath string, now time.Time, sessionFilter []string) ([]SessionFile, error)
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
}

// Result summarizes an import run.
type Result struct {
	SessionsScanned int
	TurnsImported   int
	TurnsSkipped    int
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
func Run(ctx context.Context, repo *git.Repository, imp Importer, opts Options) (Result, error) {
	var res Result
	files, err := imp.Discover(opts.RepoRoot, opts.OverridePath, opts.Now, opts.SessionFilter)
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

	for _, sf := range files {
		res.SessionsScanned++
		full, readErr := os.ReadFile(sf.Path)
		if readErr != nil {
			return res, fmt.Errorf("read %s: %w", sf.Path, readErr)
		}
		turns, splitErr := imp.SplitTurns(sf, full)
		if splitErr != nil {
			return res, fmt.Errorf("split %s session %s: %w", imp.Name(), sf.SessionID, splitErr)
		}
		// Redact the session transcript once and reuse it for every turn's
		// checkpoint (each turn stores the full session transcript with its own
		// CheckpointTranscriptStart). Redacting per turn would be O(turns).
		// Computed lazily so a fully-skipped or dry-run file pays nothing.
		var red redact.RedactedBytes
		redacted := false
		for _, turn := range turns {
			cid := DeriveCheckpointID(sf.SessionID, turn.UUID)
			if existing[cid.String()] {
				res.TurnsSkipped++
				continue
			}
			if opts.DryRun {
				res.TurnsImported++ // counts what would import
				continue
			}
			if !redacted {
				r, rerr := redact.JSONLBytes(full)
				if rerr != nil {
					return res, fmt.Errorf("redact %s transcript: %w", sf.SessionID, rerr)
				}
				red = r
				redacted = true
			}
			if err := writeTurn(ctx, stores, imp, cid, sf, red, turn); err != nil {
				return res, err
			}
			existing[cid.String()] = true
			res.TurnsImported++
		}
	}
	return res, nil
}

func writeTurn(ctx context.Context, stores *cp.Stores, imp Importer, cid id.CheckpointID, sf SessionFile, red redact.RedactedBytes, turn Turn) error {
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
	})); err != nil {
		return fmt.Errorf("write imported checkpoint %s: %w", cid, err)
	}
	return nil
}
