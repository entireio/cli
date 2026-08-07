package agentimport

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/object"

	cp "github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/trailers"
)

// heuristicGrace extends a turn's match window past the moment the next turn
// starts, absorbing the gap between an agent finishing a commit and the user
// typing the next prompt. It also bounds the LAST turn's window, which has no
// next turn to close it — deliberately conservative: a commit made long after
// the final prompt is more likely someone else's work than that turn's.
const heuristicGrace = 10 * time.Minute

// Reconcile action labels, reported per LinkResult so a caller can render (and
// a reader can audit) exactly what a run did.
const (
	// ActionWritten: a new imported checkpoint was created carrying this link.
	ActionWritten = "written"
	// ActionBackfilled: an already-imported checkpoint's link was upgraded.
	ActionBackfilled = "backfilled"
	// ActionUnchanged: the checkpoint already holds exactly this link.
	ActionUnchanged = "unchanged"
	// ActionProposed: a heuristic match that --accept-heuristics would apply.
	ActionProposed = "proposed"
	// ActionDryRun: the link a real run would have written.
	ActionDryRun = "dry-run"
)

// ReconcileOptions turns commit reconciliation on for a Run. Nil (on Options)
// means off, which keeps Run byte-identical to a plain import.
type ReconcileOptions struct {
	// Enabled scans for commits with no session data and reports the links
	// import can make to them.
	Enabled bool
	// AcceptHeuristics promotes time-window matches from reported candidates
	// to written links. Off by default: a heuristic link is a guess, and an
	// unattended import must not invent attribution.
	AcceptHeuristics bool
}

// CommitRecord is one scanned commit that carries no session data.
//
// Subject exists purely so the report can name a commit a human will
// recognize. It is user content: render it, never log it.
//
// (The plan for this feature also listed a Branch field. Scan tips arrive as
// bare hashes, so there is no branch name to attribute a commit to; the field
// would have been permanently empty and is omitted rather than shipped dead.)
type CommitRecord struct {
	SHA     string
	Subject string
	When    time.Time
}

// LinkResult is one commit↔checkpoint link a reconcile run made or proposes.
type LinkResult struct {
	CommitSHA    string
	CheckpointID id.CheckpointID
	SessionID    string
	TurnUUID     string
	// Method is the link's provenance (cp.CommitSHAMethodRecorded or
	// cp.CommitSHAMethodHeuristic) — what lands in commit_sha_method.
	Method string
	// Action is what the run did about it; one of the Action* constants.
	Action string
}

// ReconcileReport is the full outcome of a reconcile pass: what was scanned,
// what got linked, what could be linked with --accept-heuristics, and what is
// still without session data.
type ReconcileReport struct {
	CommitsScanned   int
	Links            []LinkResult
	Candidates       []LinkResult
	UnmatchedCommits []CommitRecord
}

// collectCommitsMissingSessionData walks each tip newest-first and returns the
// commits within the window that carry NO Entire-Checkpoint trailer, keyed by
// hash. Multiple tips are deduped, so a commit reachable from both origin/main
// and HEAD is scanned once.
//
// A trailered commit already links to its checkpoint and is skipped outright.
// The second half of "no session data" — a commit some checkpoint already
// anchors to — is handled by the caller instead: it drops each commit it links
// (or proposes a link for) from the returned map, so an already-anchored commit
// is re-matched to the same turn and reported as unchanged rather than as
// unmatched. Deciding it here would mean trusting List-hydrated fields, which
// may be stubs.
//
// A walk failure on one tip is logged and skipped rather than failing the run:
// the remaining tips still produce a usable (smaller) scan set, and a smaller
// set can only cost links, never invent them.
func collectCommitsMissingSessionData(ctx context.Context, repo *git.Repository, tips []plumbing.Hash, cutoff time.Time, maxWalk int) (map[plumbing.Hash]*CommitRecord, error) {
	if err := ctx.Err(); err != nil {
		return nil, err //nolint:wrapcheck // Propagating context cancellation
	}
	missing := make(map[plumbing.Hash]*CommitRecord)
	seen := make(map[plumbing.Hash]struct{})
	for _, tip := range tips {
		if tip.IsZero() {
			continue
		}
		err := walkRecentCommits(repo, tip, cutoff, maxWalk, func(c *object.Commit) {
			if _, dup := seen[c.Hash]; dup {
				return
			}
			seen[c.Hash] = struct{}{}
			if len(trailers.ParseAllCheckpoints(c.Message)) > 0 {
				return // already linked by a trailer
			}
			missing[c.Hash] = &CommitRecord{
				SHA:     c.Hash.String(),
				Subject: commitSubject(c.Message),
				When:    c.Committer.When,
			}
		})
		if err != nil {
			logging.Debug(ctx, "import: reconcile scan walk truncated for a tip",
				"tip", tip.String(), "scanned", len(seen), "error", err.Error())
		}
	}
	return missing, nil
}

// commitSubject returns a commit message's first line. Report-only — never log
// the result.
func commitSubject(message string) string {
	for i := range len(message) {
		if message[i] == '\n' {
			return message[:i]
		}
	}
	return message
}

// matchHeuristic proposes conservative 1:1 turn↔commit links by time window,
// for commits no transcript recorded. A turn's window runs from its own
// timestamp to the next turn's plus heuristicGrace (the last turn's window is
// its timestamp plus the grace).
//
// A link is proposed only when it is unambiguous in BOTH directions: exactly
// one turn window contains the commit, AND exactly one unlinked commit falls in
// that turn's window. Windows overlap by design (the grace runs past the next
// turn's start), so one-directional uniqueness is not enough — two turns
// claiming one commit, or one turn claiming two commits, both leave everything
// unmatched. Turns with no usable timestamp are skipped entirely.
//
// Results are ordered by commit SHA so a report is deterministic. Every result
// carries Method cp.CommitSHAMethodHeuristic and an empty Action: the caller
// decides whether it becomes a written link or a reported candidate.
//
// Matching on FilesTouched (intersecting a turn's edited files with the
// commit's changed paths) would disambiguate many of the cases this rejects.
// That is future work — it needs per-turn file extraction the importers don't
// do yet.
func matchHeuristic(turns []Turn, missing map[plumbing.Hash]*CommitRecord) []LinkResult {
	if len(turns) == 0 || len(missing) == 0 {
		return nil
	}
	starts, ends := turnWindows(turns)

	// commitTurn maps each commit to the unique turn whose window contains it
	// (absent = none, or ambiguous because two windows claim it); turnCommits
	// counts, per turn, how many commits its window holds.
	type turnMatch struct {
		index int
		uuid  string
	}
	commitTurn := make(map[plumbing.Hash]turnMatch, len(missing))
	turnCommits := make([]int, len(turns))
	for hash, rec := range missing {
		match := -1
		for i := range turns {
			if starts[i].IsZero() || rec.When.Before(starts[i]) || rec.When.After(ends[i]) {
				continue
			}
			if match >= 0 {
				match = -1 // a second turn claims it: ambiguous
				break
			}
			match = i
		}
		if match < 0 {
			continue
		}
		commitTurn[hash] = turnMatch{index: match, uuid: turns[match].UUID}
		turnCommits[match]++
	}

	var out []LinkResult
	for hash, rec := range missing {
		match, ok := commitTurn[hash]
		if !ok || turnCommits[match.index] != 1 {
			continue
		}
		out = append(out, LinkResult{
			CommitSHA: rec.SHA,
			TurnUUID:  match.uuid,
			Method:    cp.CommitSHAMethodHeuristic,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CommitSHA < out[j].CommitSHA })
	return out
}

// turnWindows returns each turn's [start, end] match window. A turn with no
// timestamp gets a zero start, which callers treat as "never matches".
func turnWindows(turns []Turn) (starts, ends []time.Time) {
	starts = make([]time.Time, len(turns))
	ends = make([]time.Time, len(turns))
	for i, turn := range turns {
		if turn.CreatedAt.IsZero() {
			continue
		}
		starts[i] = turn.CreatedAt
		end := turn.CreatedAt
		if i+1 < len(turns) && !turns[i+1].CreatedAt.IsZero() {
			end = turns[i+1].CreatedAt
		}
		ends[i] = end.Add(heuristicGrace)
	}
	return starts, ends
}

// reconciler carries one Run's reconcile state: the scanned commit set, which
// of those commits are still unlinked, and the report being assembled. A
// disabled reconciler (opts.Reconcile off) is a live object whose methods are
// all no-ops, so Run needs no `if reconciling` branches around them.
type reconciler struct {
	opts Options
	// scan is the immutable set of commits found without session data. It
	// doubles as the anchor resolver's reachability gate, so it is never
	// mutated — unlinked tracks progress instead.
	scan map[plumbing.Hash]*CommitRecord
	// unlinked starts as a copy of scan and loses every commit this run links
	// or proposes; the remainder is the report's UnmatchedCommits.
	unlinked map[plumbing.Hash]*CommitRecord
	report   *ReconcileReport
}

// newReconciler scans for commits missing session data, or returns a disabled
// no-op reconciler when reconciliation is off.
func newReconciler(ctx context.Context, repo *git.Repository, opts Options, cutoff time.Time) (*reconciler, error) {
	r := &reconciler{opts: opts}
	if !opts.reconciling() {
		return r, nil
	}
	scan, err := collectCommitsMissingSessionData(ctx, repo, opts.ScanTips, cutoff, ancestorWalkMaxCommits)
	if err != nil {
		return nil, fmt.Errorf("scan commits without session data: %w", err)
	}
	r.scan = scan
	r.unlinked = make(map[plumbing.Hash]*CommitRecord, len(scan))
	for hash, record := range scan {
		r.unlinked[hash] = record
	}
	r.report = &ReconcileReport{CommitsScanned: len(scan)}
	return r, nil
}

func (r *reconciler) enabled() bool { return r.report != nil }

// scanned returns the immutable scan set for the anchor resolver's gate; nil
// when disabled, which leaves the resolver's behavior unchanged.
func (r *reconciler) scanned() map[plumbing.Hash]*CommitRecord { return r.scan }

// matchSession returns this session's heuristic proposals keyed by turn UUID.
func (r *reconciler) matchSession(turns []Turn) map[string]string {
	if !r.enabled() {
		return nil
	}
	matches := matchHeuristic(turns, r.unlinked)
	out := make(map[string]string, len(matches))
	for _, m := range matches {
		out[m.TurnUUID] = m.CommitSHA
	}
	return out
}

// applyHeuristic upgrades a turn's fallback anchor with its heuristic match,
// if it has one. A transcript-recorded anchor always wins and is returned
// untouched. heuristicOnly reports a match the run is NOT allowed to write
// (no --accept-heuristics): the caller reports it as a candidate and writes
// the fallback anchor instead.
func (r *reconciler) applyHeuristic(heuristics map[string]string, turnUUID, anchor, method string) (string, string, bool) {
	if !r.enabled() || method == cp.CommitSHAMethodRecorded {
		return anchor, method, false
	}
	sha, ok := heuristics[turnUUID]
	if !ok {
		return anchor, method, false
	}
	// Proposals are computed once per session, before the turn loop, so an
	// earlier turn in the SAME session may have claimed this commit with a
	// recorded link since. Re-check membership rather than reporting the same
	// commit linked twice.
	if _, stillUnlinked := r.unlinked[plumbing.NewHash(sha)]; !stillUnlinked {
		return anchor, method, false
	}
	return sha, cp.CommitSHAMethodHeuristic, !r.opts.acceptingHeuristics()
}

// record files a link the run made (or would make) for a NEW checkpoint, and
// drops its commit from the unlinked set. Fallback anchors are not links:
// they carry no claim about which commit a turn produced, so they are neither
// reported nor removed from the unlinked set.
func (r *reconciler) record(link LinkResult, heuristicOnly bool, action string, res *Result) {
	if !r.enabled() || !isRealLink(link.Method) {
		return
	}
	link.Action = action
	if heuristicOnly {
		link.Action = ActionProposed
		r.report.Candidates = append(r.report.Candidates, link)
	} else {
		r.report.Links = append(r.report.Links, link)
		r.countLink(link, action, res)
	}
	delete(r.unlinked, plumbing.NewHash(link.CommitSHA))
}

// recordExisting handles a turn whose checkpoint an earlier run already
// imported: it reads that checkpoint's stored link and backfills it only when
// this run's link is an improvement (never a downgrade), so a re-run converges
// on "unchanged" with zero writes.
//
// The read is targeted (one Read per matched turn) and deliberately not taken
// from List: List results may be stubs, and a link decision must never depend
// on a field that might not be hydrated.
func (r *reconciler) recordExisting(ctx context.Context, stores *cp.Stores, link LinkResult, heuristicOnly bool, res *Result) {
	if !r.enabled() || !isRealLink(link.Method) {
		return
	}
	if heuristicOnly {
		link.Action = ActionProposed
		r.report.Candidates = append(r.report.Candidates, link)
		delete(r.unlinked, plumbing.NewHash(link.CommitSHA))
		return
	}

	summary, err := stores.Persistent.Read(ctx, link.CheckpointID)
	if err != nil || summary == nil {
		// Best-effort: an unreadable checkpoint costs this one link, not the
		// import. Operational metadata only — never the commit subject.
		logging.Debug(ctx, "import: reconcile could not read imported checkpoint, link skipped",
			"checkpointID", link.CheckpointID.String(), "error", errText(err))
		return
	}
	delete(r.unlinked, plumbing.NewHash(link.CommitSHA))

	if !linkImproves(summary.CommitSHA, summary.CommitSHAMethod, link.CommitSHA, link.Method) {
		link.Action = ActionUnchanged
		r.report.Links = append(r.report.Links, link)
		return
	}
	if r.opts.DryRun {
		link.Action = ActionDryRun
		r.report.Links = append(r.report.Links, link)
		r.countLink(link, ActionDryRun, res)
		return
	}
	if err := stores.Persistent.Write(ctx, cp.CheckpointCommitSHA{
		CheckpointID: link.CheckpointID,
		SessionID:    link.SessionID,
		CommitSHA:    link.CommitSHA,
		Method:       link.Method,
	}); err != nil {
		logging.Debug(ctx, "import: reconcile commit-link backfill failed",
			"checkpointID", link.CheckpointID.String(), "error", err.Error())
		return
	}
	link.Action = ActionBackfilled
	r.report.Links = append(r.report.Links, link)
	res.Backfilled++
	r.countLink(link, ActionBackfilled, res)
}

// countLink tallies a link into the per-method Result counters. Unchanged
// links are already stored and are not counted as work this run did.
func (r *reconciler) countLink(link LinkResult, action string, res *Result) {
	if action == ActionUnchanged {
		return
	}
	switch link.Method {
	case cp.CommitSHAMethodRecorded:
		res.LinksRecorded++
	case cp.CommitSHAMethodHeuristic:
		res.LinksHeuristic++
	}
}

// finish returns the assembled report with the leftover commits attached, in
// newest-first order. Nil when reconciliation was off.
func (r *reconciler) finish() *ReconcileReport {
	if !r.enabled() {
		return nil
	}
	remaining := make([]CommitRecord, 0, len(r.unlinked))
	for _, record := range r.unlinked {
		remaining = append(remaining, *record)
	}
	sort.Slice(remaining, func(i, j int) bool {
		if !remaining[i].When.Equal(remaining[j].When) {
			return remaining[i].When.After(remaining[j].When)
		}
		return remaining[i].SHA < remaining[j].SHA
	})
	r.report.UnmatchedCommits = remaining
	return r.report
}

// isRealLink reports whether a method makes a claim about which commit a turn
// produced. The fallback display anchor does not.
func isRealLink(method string) bool {
	return method == cp.CommitSHAMethodRecorded || method == cp.CommitSHAMethodHeuristic
}

// errText renders an error for a log attribute, tolerating nil (a nil-summary
// read reports no error of its own).
func errText(err error) string {
	if err == nil {
		return "checkpoint not found"
	}
	return err.Error()
}

// linkImproves reports whether writing (sha, method) over a checkpoint's stored
// link is worth a write. A recorded link may replace anything but an identical
// one; a heuristic link may only fill a gap (no link, or the fallback display
// anchor / an unlabeled legacy one). This mirrors the store's downgrade guard,
// so a run that would be refused never issues the write in the first place.
func linkImproves(storedSHA, storedMethod, sha, method string) bool {
	if sha == "" || method == "" {
		return false
	}
	if storedSHA == sha && storedMethod == method {
		return false
	}
	switch method {
	case cp.CommitSHAMethodRecorded:
		return true
	case cp.CommitSHAMethodHeuristic:
		return storedMethod == "" || storedMethod == cp.CommitSHAMethodFallback
	default:
		return false
	}
}
