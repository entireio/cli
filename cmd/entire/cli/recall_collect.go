package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing/object"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/stringutil"
	"github.com/entireio/cli/cmd/entire/cli/summarize"
	"github.com/entireio/cli/cmd/entire/cli/trailers"
	"github.com/entireio/cli/redact"
)

// Wire shape consumed by recall/ (src/model.rs). Keep the two in step.
type recallTurn struct {
	Role string `json:"role"`
	Text string `json:"text"`
}

type recallCheckpoint struct {
	CheckpointID  string       `json:"checkpoint_id"`
	CommitSHA     string       `json:"commit_sha"`
	CommitMessage string       `json:"commit_message"`
	Agent         string       `json:"agent"`
	Files         []string     `json:"files"`
	Session       []recallTurn `json:"session"`
	Diff          []string     `json:"diff"`
	// Unavailable names the fields this shim could not provide: redacted,
	// withheld by --no-transcripts, or unreadable. The Rust side treats a
	// check that needs one of them as unverifiable rather than running it on
	// an empty field. Declared here, never inferred there.
	Unavailable []string `json:"unavailable,omitempty"`
}

// Field names shared with recall/src/model.rs.
const (
	recallFieldSession = "session"
	recallFieldDiff    = "diff"
	recallFieldFiles   = "files"
)

type recallIngestInput struct {
	RepoRoot    string             `json:"repo_root"`
	Checkpoints []recallCheckpoint `json:"checkpoints"`
	// Truncated is set when the commit walk hit recallMaxCommits: older
	// checkpoints exist on the branch and were never examined.
	Truncated bool `json:"truncated"`
}

// recallCommit is a commit on the branch that carries an Entire-Checkpoint trailer.
type recallCommit struct {
	SHA          string
	Message      string
	CheckpointID id.CheckpointID
}

const (
	recallMaxCommits        = 200
	recallMaxDiffLines      = 400
	recallMaxAssistantRunes = 600
)

// collectRecallCheckpoints walks HEAD's first-parent history for trailered
// commits and reads each one's checkpoint into the wire shape. A commit whose
// checkpoint cannot be read is never dropped: its ledger record (sha, subject,
// files and diff from git) is still ingested, with the transcript declared
// unavailable. With noTranscripts every checkpoint is treated that way and the
// transcript is never opened.
func collectRecallCheckpoints(ctx context.Context, errW io.Writer, root string, noTranscripts bool) (*recallIngestInput, error) {
	lookup, err := newExplainCheckpointLookup(ctx)
	if err != nil {
		return nil, err
	}
	defer lookup.Close()

	head, err := lookup.repo.Head()
	if err != nil {
		return nil, fmt.Errorf("resolve HEAD: %w", err)
	}
	iter, err := lookup.repo.Log(&git.LogOptions{From: head.Hash(), Order: git.LogOrderCommitterTime})
	if err != nil {
		return nil, fmt.Errorf("walk history: %w", err)
	}
	defer iter.Close()

	input := &recallIngestInput{RepoRoot: root}
	seen := 0
	err = iter.ForEach(func(c *object.Commit) error {
		if seen >= recallMaxCommits {
			input.Truncated = true
			return errRecallWalkDone
		}
		seen++
		cpID, ok := trailers.ParseCheckpoint(c.Message)
		if !ok {
			return nil
		}
		diff, diffErr := recallCommitDiff(ctx, root, c.Hash.String())
		cp := recallCheckpointFromReader(ctx, errW, lookup.store, recallCommit{SHA: c.Hash.String()[:7], Message: c.Message, CheckpointID: cpID}, diff, diffErr, noTranscripts)
		input.Checkpoints = append(input.Checkpoints, *cp)
		return nil
	})
	if err != nil && !errors.Is(err, errRecallWalkDone) {
		return nil, fmt.Errorf("walk history: %w", err)
	}
	return input, nil
}

var errRecallWalkDone = errors.New("recall: walk budget reached")

// recallReader is the slice of the persistent store the shim reads through.
type recallReader interface {
	Read(ctx context.Context, checkpointID id.CheckpointID) (*checkpoint.CheckpointSummary, error)
	ReadSessionContent(ctx context.Context, checkpointID id.CheckpointID, sessionIndex int) (*checkpoint.SessionContent, error)
}

// recallCheckpointFromReader builds the wire checkpoint for one trailered
// commit. It never fails: what git knows (sha, subject, diff, files) is always
// a ledger record, and every field the store could not supply is declared in
// Unavailable so the ranking side says "unverifiable" instead of judging a
// claim against emptiness. Reasons go to errW; they are operational, not
// content.
func recallCheckpointFromReader(ctx context.Context, errW io.Writer, reader recallReader, commit recallCommit, diff []string, diffErr error, noTranscripts bool) *recallCheckpoint {
	subject, _, _ := strings.Cut(strings.TrimSpace(commit.Message), "\n")
	cp := &recallCheckpoint{
		CheckpointID:  commit.CheckpointID.String(),
		CommitSHA:     commit.SHA,
		CommitMessage: subject,
		Diff:          diff,
	}
	if diffErr != nil {
		fmt.Fprintf(errW, "recall: %s: diff unavailable (%v)\n", commit.SHA, diffErr)
		cp.Diff = nil
		cp.Unavailable = append(cp.Unavailable, recallFieldDiff)
	}

	summary, err := reader.Read(ctx, commit.CheckpointID)
	if err == nil && (summary == nil || len(summary.Sessions) == 0) {
		err = checkpoint.ErrCheckpointNotFound
	}
	var content *checkpoint.SessionContent
	switch {
	case err != nil:
		fmt.Fprintf(errW, "recall: %s: transcript unavailable (%v); ingesting ledger record only\n", commit.CheckpointID, err)
	case noTranscripts:
		// Deliberately never opened: the privacy boundary is that the
		// transcript is not read, not that it is read and discarded.
	default:
		content, err = reader.ReadSessionContent(ctx, commit.CheckpointID, len(summary.Sessions)-1)
		if err != nil {
			fmt.Fprintf(errW, "recall: %s: transcript unavailable (%v); ingesting ledger record only\n", commit.CheckpointID, err)
			content = nil
		}
	}

	var files []string
	if summary != nil {
		files = summary.FilesTouched
	}
	if len(files) == 0 && content != nil {
		files = content.Metadata.FilesTouched
	}
	if len(files) == 0 {
		files = recallFilesFromDiff(diff)
	}
	if len(files) == 0 {
		cp.Unavailable = append(cp.Unavailable, recallFieldFiles)
	}
	cp.Files = files

	if content != nil {
		cp.Agent = string(content.Metadata.Agent)
		if entries, condErr := summarize.BuildCondensedTranscriptFromBytes(redact.AlreadyRedacted(content.Transcript), content.Metadata.Agent); condErr == nil {
			cp.Session = recallTurnsFromEntries(entries, recallMaxAssistantRunes)
		}
	} else {
		cp.Unavailable = append(cp.Unavailable, recallFieldSession)
	}
	// Sequences on the wire are arrays, never null: a nil slice would marshal
	// as null and the Rust side rejects null for a sequence.
	if cp.Files == nil {
		cp.Files = []string{}
	}
	if cp.Session == nil {
		cp.Session = []recallTurn{}
	}
	if cp.Diff == nil {
		cp.Diff = []string{}
	}
	return cp
}

// recallFilesFromDiff recovers the touched paths from the `+++ b/` headers
// that recallDiffLines keeps, for a checkpoint whose store entry is unreadable.
func recallFilesFromDiff(diff []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, line := range diff {
		path, ok := strings.CutPrefix(line, "+++ b/")
		if !ok || path == "" || seen[path] {
			continue
		}
		seen[path] = true
		out = append(out, path)
	}
	return out
}

// recallTurnsFromEntries keeps user and assistant text, collapses whitespace,
// and truncates assistant turns (tool-heavy and long) but never user intent.
func recallTurnsFromEntries(entries []summarize.Entry, maxAssistantRunes int) []recallTurn {
	turns := make([]recallTurn, 0, len(entries))
	for _, e := range entries {
		text := stringutil.CollapseWhitespace(e.Content)
		if text == "" {
			continue
		}
		switch e.Type {
		case summarize.EntryTypeUser:
			turns = append(turns, recallTurn{Role: "user", Text: text})
		case summarize.EntryTypeAssistant:
			turns = append(turns, recallTurn{Role: "assistant", Text: stringutil.TruncateRunes(text, maxAssistantRunes, "...")})
		case summarize.EntryTypeTool:
		}
	}
	return turns
}

// recallCommitDiff returns the commit's unified-0 diff as filtered lines.
func recallCommitDiff(ctx context.Context, root, sha string) ([]string, error) {
	cmd := exec.CommandContext(ctx, "git", "-C", root, "show", "--format=", "--no-color", "-U0", sha)
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git show: %w", err)
	}
	return recallDiffLines(string(out), recallMaxDiffLines), nil
}

// recallDiffLines keeps the lines the Rust side reads — `+++` file headers,
// `@@` hunks, and +/- content — and drops index/`---`/no-newline noise.
func recallDiffLines(raw string, maxLines int) []string {
	var out []string
	for line := range strings.SplitSeq(raw, "\n") {
		if len(out) >= maxLines {
			break
		}
		switch {
		case strings.HasPrefix(line, "+++ "), strings.HasPrefix(line, "@@ "):
			out = append(out, line)
		case strings.HasPrefix(line, "--- "), strings.HasPrefix(line, "\\ "), line == "":
		case line[0] == '+' || line[0] == '-':
			out = append(out, line)
		}
	}
	return out
}
