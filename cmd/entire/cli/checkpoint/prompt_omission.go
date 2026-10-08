package checkpoint

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/redact"
)

// TaskTranscriptReasonOmittedBySettings is the task.json
// transcript_unavailable_reason recorded when strategy_options.sync_prompts is
// false. Like the other reasons it is a stable category string.
const TaskTranscriptReasonOmittedBySettings = "transcript omitted by settings"

// promptOmittingStore enforces strategy_options.sync_prompts=false at the
// persistent-write boundary. Every write that reaches the persistent store
// (condensation, finalize backfill, attach, import, summary backfill) passes
// through Write, which removes prompt-bearing content before delegating:
//
//   - prompt.txt (Prompts)
//   - full.jsonl / transcript.jsonl and their image assets (Transcript,
//     TranscriptPath, Assets)
//   - subagent transcripts and task descriptions (Tasks)
//   - AI summaries, whose intent restates the prompt (Summary, SessionSummary)
//   - review prompts and investigate topics (user-supplied free text)
//   - skill-event native fields and transcript anchors (Pi records the full
//     slash-command invocation, arguments included)
//
// Checkpoint metadata, attribution, files touched, token usage, and metrics are
// kept. Reads are untouched, so checkpoints written before the setting was
// enabled stay readable. Local shadow-branch (ephemeral) capture is not
// affected; it never leaves the machine.
type promptOmittingStore struct {
	PersistentStore
}

// withPromptOmission wraps store when omit is true. It preserves the optional
// AuthorReader capability (explain relies on it) when the wrapped store has it.
func withPromptOmission(store PersistentStore, omit bool) PersistentStore {
	if !omit {
		return store
	}
	base := &promptOmittingStore{PersistentStore: store}
	if author, ok := store.(AuthorReader); ok {
		return &promptOmittingStoreWithAuthor{promptOmittingStore: base, author: author}
	}
	return base
}

func (s *promptOmittingStore) Write(ctx context.Context, req WriteRequest) error {
	stripped, ok := omitPromptContent(req)
	if !ok {
		logging.Debug(ctx, "checkpoint: write skipped; sync_prompts is disabled",
			slog.String("request_type", fmt.Sprintf("%T", req)))
		return nil
	}
	return s.PersistentStore.Write(ctx, stripped) //nolint:wrapcheck // pure delegation
}

type promptOmittingStoreWithAuthor struct {
	*promptOmittingStore

	author AuthorReader
}

func (s *promptOmittingStoreWithAuthor) GetCheckpointAuthor(ctx context.Context, checkpointID id.CheckpointID) (Author, error) {
	return s.author.GetCheckpointAuthor(ctx, checkpointID) //nolint:wrapcheck // pure delegation
}

// omitPromptContent returns req with prompt-bearing content removed. ok is
// false when nothing would remain to write (a summary-only backfill), in which
// case the caller drops the request.
func omitPromptContent(req WriteRequest) (WriteRequest, bool) {
	switch r := req.(type) {
	case Session:
		return Session(omitFromWriteOptions(WriteOptions(r))), true
	case ReservedSession:
		return ReservedSession(omitFromWriteOptions(WriteOptions(r))), true
	case SessionTranscript:
		opts := UpdateOptions(r)
		opts.Transcript = redact.RedactedBytes{}
		opts.Assets = nil
		opts.PrecomputedBlobs = nil
		opts.Prompts = nil
		opts.SkillEvents = omitFromSkillEvents(opts.SkillEvents)
		return SessionTranscript(opts), true
	case SessionSummary:
		return nil, false
	default:
		return req, true
	}
}

func omitFromWriteOptions(opts WriteOptions) WriteOptions {
	opts.Transcript = redact.RedactedBytes{}
	opts.TranscriptPath = ""
	opts.Assets = nil
	opts.Prompts = nil
	opts.Summary = nil
	opts.ReviewPrompt = ""
	opts.InvestigateTopic = ""
	opts.SkillEvents = omitFromSkillEvents(opts.SkillEvents)
	if len(opts.Tasks) > 0 {
		tasks := make([]TaskPayload, len(opts.Tasks))
		for i, task := range opts.Tasks {
			task.Transcript = redact.RedactedBytes{}
			task.TaskDescription = ""
			task.TranscriptUnavailableReason = TaskTranscriptReasonOmittedBySettings
			tasks[i] = task
		}
		opts.Tasks = tasks
	}
	return opts
}

func omitFromSkillEvents(events []types.SkillEvent) []types.SkillEvent {
	if len(events) == 0 {
		return events
	}
	out := make([]types.SkillEvent, len(events))
	for i, ev := range events {
		ev.Native = nil
		ev.TranscriptAnchor = nil
		ev.Collapse.Label = ev.Skill.Name
		out[i] = ev
	}
	return out
}
