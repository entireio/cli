package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/jsonutil"
	"github.com/entireio/cli/cmd/entire/cli/settings"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
	"github.com/entireio/cli/cmd/entire/cli/stringutil"
	"github.com/entireio/cli/cmd/entire/cli/tuiutil"
)

// outgoingCheckpointsJSON is `entire checkpoint list --outgoing --json`. The
// checkpoint_sync_* and checkpoint_push_disabled fields carry the same values
// as in `entire status --json`. Checkpoints is never null.
type outgoingCheckpointsJSON struct {
	SyncRemote        string                   `json:"checkpoint_sync_remote,omitempty"`
	SyncRemoteSource  string                   `json:"checkpoint_sync_remote_source,omitempty"`
	SyncError         string                   `json:"checkpoint_sync_error,omitempty"`
	PushDisabled      bool                     `json:"checkpoint_push_disabled"`
	PushPolicy        string                   `json:"checkpoint_push_policy,omitempty"`
	PushPolicyError   string                   `json:"checkpoint_push_policy_error,omitempty"`
	StaleQueueEntries int                      `json:"stale_queue_entries,omitempty"`
	Checkpoints       []outgoingCheckpointJSON `json:"checkpoints"`
}

// outgoingCheckpointJSON is one checkpoint the next push sends. A queued ref
// that names no checkpoint, or a checkpoint that cannot be read, is still sent,
// so it is listed with Error set instead of being dropped.
type outgoingCheckpointJSON struct {
	CheckpointID string   `json:"checkpoint_id,omitempty"`
	Ref          string   `json:"ref"`
	Commit       string   `json:"commit,omitempty"`
	SessionIDs   []string `json:"session_ids,omitempty"`
	CreatedAt    string   `json:"created_at,omitempty"`
	Agent        string   `json:"agent,omitempty"`
	SessionCount int      `json:"session_count,omitempty"`
	Prompt       string   `json:"prompt,omitempty"`
	Summary      string   `json:"summary,omitempty"`
	FilesTouched []string `json:"files_touched"`
	Error        string   `json:"error,omitempty"`
}

// outgoingListing is the --outgoing dataset: where checkpoints sync to, what
// the next push sends there, and what each of those checkpoints holds.
type outgoingListing struct {
	sync    checkpointSyncInfo
	result  strategy.OutgoingCheckpoints
	entries []outgoingCheckpointRow
}

type outgoingCheckpointRow struct {
	strategy.OutgoingCheckpoint

	createdAt    time.Time
	agent        string
	sessionCount int
	sessionIDs   []string
	prompt       string
	summary      string
	files        []string
	err          error
}

// outgoingNeedsGitRefsMessage explains the git-branch refusal to the user.
const outgoingNeedsGitRefsMessage = "entire checkpoint list --outgoing needs the git-refs checkpoint backend. " +
	"This repository uses git-branch, whose push sends entire/checkpoints/v1 as one branch rather than one ref per checkpoint."

func loadOutgoingListing(ctx context.Context) (outgoingListing, error) {
	result, err := strategy.ListOutgoingCheckpoints(ctx)
	if err != nil {
		return outgoingListing{}, fmt.Errorf("list outgoing checkpoints: %w", err)
	}
	s, err := LoadEntireSettings(ctx)
	if err != nil {
		return outgoingListing{}, err
	}
	// The listing is the count, so status' counter must not read the queue again.
	listed := func(context.Context, string) int { return len(result.Checkpoints) }
	listing := outgoingListing{sync: computeCheckpointSyncInfoCounting(ctx, s, listed), result: result}
	if len(result.Checkpoints) == 0 {
		return listing, nil
	}

	repo, err := openRepository(ctx)
	if err != nil {
		return outgoingListing{}, fmt.Errorf("not a git repository: %w", err)
	}
	defer repo.Close()
	// No fetchers: every queued checkpoint was written here, and a read must
	// never reach the remote.
	stores, err := checkpoint.Open(ctx, repo, checkpoint.OpenOptions{
		// Deliberately local: remote-tracking tiers hold what the remote
		// already has, not what this push sends.
		ReadRemotes: nil,
	})
	if err != nil {
		return outgoingListing{}, fmt.Errorf("open checkpoint store: %w", err)
	}
	for _, oc := range result.Checkpoints {
		listing.entries = append(listing.entries, readOutgoingCheckpoint(ctx, stores.Persistent, oc))
	}
	// Oldest first; rows without a creation time (unreadable) go last.
	slices.SortStableFunc(listing.entries, func(a, b outgoingCheckpointRow) int {
		switch {
		case a.createdAt.IsZero() != b.createdAt.IsZero():
			if a.createdAt.IsZero() {
				return 1
			}
			return -1
		default:
			return a.createdAt.Compare(b.createdAt)
		}
	})
	return listing, nil
}

// reportOutgoingErr prints the git-branch refusal in place of the raw error.
func reportOutgoingErr(errW io.Writer, err error) error {
	if errors.Is(err, strategy.ErrOutgoingNeedsGitRefs) {
		fmt.Fprintln(errW, outgoingNeedsGitRefsMessage)
		return NewSilentError(err)
	}
	return err
}

// readOutgoingCheckpoint reads what the listing shows for one checkpoint. The
// prompt comes from the stored prompt.txt, which redaction already ran over.
func readOutgoingCheckpoint(ctx context.Context, store checkpoint.PersistentStore, oc strategy.OutgoingCheckpoint) outgoingCheckpointRow {
	row := outgoingCheckpointRow{OutgoingCheckpoint: oc}
	if oc.CheckpointID.IsEmpty() {
		row.err = errors.New("ref does not name a checkpoint")
		return row
	}
	summary, err := checkpoint.ReadCheckpoint(ctx, store, oc.CheckpointID)
	if err != nil {
		row.err = err
		return row
	}
	row.files = summary.FilesTouched
	row.sessionCount = len(summary.Sessions)
	if row.sessionCount == 0 {
		return row
	}
	first, latest, ids, err := readOutgoingSessions(row.sessionCount, func(i int) (*checkpoint.Metadata, error) {
		return store.ReadSessionMetadata(ctx, oc.CheckpointID, i)
	})
	if err != nil {
		row.err = err
		return row
	}
	row.createdAt = first.CreatedAt
	row.sessionIDs = ids
	row.agent = string(latest.Agent)
	if latest.Summary != nil {
		row.summary = latest.Summary.Intent
	}
	row.prompt = readLatestCommittedSessionPrompt(ctx, store, oc.CheckpointID, row.sessionCount)
	return row
}

// readOutgoingSessions reads a checkpoint's sessions for its row. The first
// and last must be readable, since they give the row's date, agent and
// summary; a session between them only adds its ID, so one that cannot be
// read leaves its ID out instead of failing the row.
func readOutgoingSessions(count int, read func(int) (*checkpoint.Metadata, error)) (first, latest *checkpoint.Metadata, ids []string, err error) {
	first, err = read(0)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("read session metadata: %w", err)
	}
	ids = append(ids, first.SessionID)
	latest = first
	for i := 1; i < count; i++ {
		meta, readErr := read(i)
		if readErr != nil {
			if i == count-1 {
				return nil, nil, nil, fmt.Errorf("read session metadata: %w", readErr)
			}
			continue
		}
		ids = append(ids, meta.SessionID)
		latest = meta
	}
	return first, latest, ids, nil
}

func (l outgoingListing) json() outgoingCheckpointsJSON {
	out := outgoingCheckpointsJSON{
		SyncRemote:        l.sync.Remote,
		SyncRemoteSource:  l.sync.Source,
		SyncError:         l.sync.Err,
		PushDisabled:      l.sync.PushDisabled,
		PushPolicy:        string(l.sync.PushPolicy),
		PushPolicyError:   l.sync.PushPolicyErr,
		StaleQueueEntries: l.result.StaleQueueEntries,
		Checkpoints:       make([]outgoingCheckpointJSON, 0, len(l.entries)),
	}
	for _, row := range l.entries {
		entry := outgoingCheckpointJSON{
			CheckpointID: row.CheckpointID.String(),
			Ref:          row.Ref.String(),
			SessionIDs:   row.sessionIDs,
			Agent:        row.agent,
			SessionCount: row.sessionCount,
			Prompt:       row.prompt,
			Summary:      row.summary,
			FilesTouched: row.files,
		}
		if entry.FilesTouched == nil {
			entry.FilesTouched = []string{}
		}
		if !row.createdAt.IsZero() {
			entry.CreatedAt = row.createdAt.UTC().Format(time.RFC3339)
		}
		if !row.Commit.IsZero() {
			entry.Commit = row.Commit.String()
		}
		if row.err != nil {
			entry.Error = row.err.Error()
		}
		out.Checkpoints = append(out.Checkpoints, entry)
	}
	return out
}

func runCheckpointOutgoingListJSON(ctx context.Context, w, errW io.Writer) error {
	listing, err := loadOutgoingListing(ctx)
	if err != nil {
		return reportOutgoingErr(errW, err)
	}
	data, err := jsonutil.MarshalIndentWithNewline(listing.json(), "", "  ")
	if err != nil {
		return fmt.Errorf("encode outgoing checkpoints: %w", err)
	}
	fmt.Fprint(w, string(data))
	return nil
}

func runCheckpointOutgoingListHuman(ctx context.Context, w, errW io.Writer, noPager bool) error {
	listing, err := loadOutgoingListing(ctx)
	if err != nil {
		return reportOutgoingErr(errW, err)
	}
	var out strings.Builder
	renderOutgoingListing(&out, listing)
	outputExplainContent(w, out.String(), noPager)
	return nil
}

// outgoingDestination names where checkpoints sync to, for prose.
func outgoingDestination(info checkpointSyncInfo) string {
	if info.Source == checkpointSyncSourceDedicated {
		return "the dedicated checkpoint remote (" + tuiutil.SanitizeTerminalLabel(info.Remote) + ")"
	}
	return tuiutil.SanitizeTerminalLabel(info.Remote)
}

func renderOutgoingListing(w io.Writer, l outgoingListing) {
	info := l.sync
	n := len(l.entries)
	dest := outgoingDestination(info)

	if info.Err != "" {
		label := "! Checkpoints NOT syncing: "
		if info.PushDisabled {
			label = "! Checkpoint remote configuration: "
		}
		fmt.Fprintln(w, label+tuiutil.SanitizeTerminalLabel(info.Err))
	}
	if info.PushPolicyErr != "" {
		fmt.Fprintln(w, "! Checkpoint push policy unreadable: "+tuiutil.SanitizeTerminalLabel(info.PushPolicyErr))
	}
	if info.PushDisabled {
		if n > 0 {
			fmt.Fprintln(w, "Automatic checkpoint pushing is off (push_sessions=false), so 'git push' sends none of these.")
		} else {
			fmt.Fprintln(w, "Automatic checkpoint pushing is off (push_sessions=false).")
		}
	}
	// An empty queue says nothing is waiting to be sent, not what the remote
	// holds: a newly elected remote can be missing every checkpoint.
	if n == 0 {
		if dest == "" {
			fmt.Fprintln(w, "No checkpoints are queued.")
		} else {
			fmt.Fprintf(w, "No checkpoints are queued for %s.\n", dest)
		}
		renderStaleQueueEntries(w, l.result.StaleQueueEntries)
		return
	}

	fmt.Fprintln(w, outgoingHeadline(info, n, dest))
	fmt.Fprintln(w)
	for _, row := range l.entries {
		renderOutgoingRow(w, row)
	}
	renderStaleQueueEntries(w, l.result.StaleQueueEntries)
	fmt.Fprintln(w, "Details: entire checkpoint explain <id>")
}

func outgoingHeadline(info checkpointSyncInfo, n int, dest string) string {
	count := pluralCount(n, nounCheckpoint, nounCheckpoints)
	switch {
	case info.PushDisabled && dest != "":
		return fmt.Sprintf("%s %s queued for %s:", count, beVerb(n), dest)
	case info.PushDisabled:
		return fmt.Sprintf("%s %s queued:", count, beVerb(n))
	case info.PushPolicyErr != "":
		return fmt.Sprintf("'git push' sends none of these until the checkpoint push policy can be read. %s %s queued:", count, beVerb(n))
	case info.PushPolicy == settings.CheckpointPushPolicyManual && dest != "":
		return fmt.Sprintf("%s %s waiting for 'entire checkpoint push --all', which sends them to %s:", count, beVerb(n), dest)
	case info.PushPolicy == settings.CheckpointPushPolicyManual:
		return fmt.Sprintf("%s %s waiting for 'entire checkpoint push --all':", count, beVerb(n))
	case info.Err != "":
		return fmt.Sprintf("'git push' sends none of these until that is fixed. %s %s queued:", count, beVerb(n))
	case dest == "":
		return fmt.Sprintf("%s %s queued, but no git remote is configured to send them to:", count, beVerb(n))
	case info.Source == checkpointSyncSourceDedicated:
		return fmt.Sprintf("%s %s to %s with your next 'git push':", count, goVerb(n), dest)
	default:
		return fmt.Sprintf("%s %s to %s with your next 'git push %s':", count, goVerb(n), dest, dest)
	}
}

func goVerb(n int) string {
	if n == 1 {
		return "goes"
	}
	return "go"
}

func beVerb(n int) string {
	if n == 1 {
		return "is"
	}
	return "are"
}

func renderOutgoingRow(w io.Writer, row outgoingCheckpointRow) {
	name := row.CheckpointID.String()
	if name == "" {
		name = row.Ref.String()
	}
	header := tuiutil.SanitizeTerminalLabel(name)
	if row.err != nil {
		fmt.Fprintf(w, "%s  cannot read: %s\n\n", header, tuiutil.SanitizeTerminalLabel(row.err.Error()))
		return
	}
	if !row.createdAt.IsZero() {
		header += "  " + timeAgo(row.createdAt)
	}
	if row.agent != "" {
		header += "  " + tuiutil.SanitizeTerminalLabel(row.agent)
	}
	fmt.Fprintln(w, header)
	switch {
	case row.prompt != "":
		fmt.Fprintf(w, "  prompt   %s\n", outgoingLabel(row.prompt))
	case row.summary != "":
		fmt.Fprintf(w, "  summary  %s\n", outgoingLabel(row.summary))
	}
	for _, f := range row.files {
		fmt.Fprintf(w, "  file     %s\n", tuiutil.SanitizeTerminalLabel(f))
	}
	fmt.Fprintln(w)
}

// outgoingLabel shortens free text to one line; --json carries it in full.
func outgoingLabel(s string) string {
	return tuiutil.SanitizeTerminalLabel(stringutil.TruncateRunes(stringutil.CollapseWhitespace(s), previewPromptWidth, "..."))
}

func renderStaleQueueEntries(w io.Writer, n int) {
	switch {
	case n == 1:
		fmt.Fprintln(w, "1 queued checkpoint ref no longer exists locally; the next push drops it from the queue.")
	case n > 1:
		fmt.Fprintf(w, "%d queued checkpoint refs no longer exist locally; the next push drops them from the queue.\n", n)
	}
}
