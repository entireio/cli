package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/strategy"

	"github.com/spf13/cobra"
)

// trailResumeActionReport is the --json payload for an actual resume: what was
// done, what was restored, and how to continue. Inspection (--no-resume
// --json) keeps its separate, richer context payload.
type trailResumeActionReport struct {
	Trail        trailResumeReportTrail   `json:"trail"`
	Actions      trailResumeReportActions `json:"actions"`
	Continuation *trailResumeContinuation `json:"continuation,omitempty"`
	Error        *trailResumeReportError  `json:"error,omitempty"`
}

type trailResumeReportTrail struct {
	ID     string `json:"id,omitempty"`
	Number int    `json:"number,omitempty"`
	Title  string `json:"title,omitempty"`
	Branch string `json:"branch"`
}

type trailResumeReportActions struct {
	FetchedBranch        bool                       `json:"fetched_branch"`
	SwitchedBranch       bool                       `json:"switched_branch"`
	CheckpointID         string                     `json:"checkpoint_id,omitempty"`
	CheckpointBehindHead int                        `json:"checkpoint_behind_head,omitempty"`
	RestoredSessions     []trailResumeReportSession `json:"restored_sessions"`
}

type trailResumeReportSession struct {
	SessionID string `json:"session_id"`
	Agent     string `json:"agent,omitempty"`
	Kind      string `json:"kind,omitempty"`
}

// trailResumeContinuation names the default session to continue with. Agent
// and session id are separate fields so headless orchestrators can construct
// their own invocation instead of unquoting the command string.
type trailResumeContinuation struct {
	Agent     string `json:"agent,omitempty"`
	SessionID string `json:"session_id"`
	Command   string `json:"command,omitempty"`
}

// trailResumeReportError is the typed-failure half of the JSON contract:
// typed resume errors emit the report with this error object and a non-zero
// exit; untyped/pre-action failures keep stdout empty with text on stderr.
type trailResumeReportError struct {
	Type         string `json:"type"`
	Message      string `json:"message"`
	Branch       string `json:"branch,omitempty"`
	WorktreePath string `json:"worktree_path,omitempty"`
	CheckpointID string `json:"checkpoint_id,omitempty"`
}

// trailResumeReportErrorFrom maps a typed resume error to its JSON object,
// or nil when the error is not part of the typed contract.
func trailResumeReportErrorFrom(err error) *trailResumeReportError {
	var clash *ResumeWorktreeClashError
	if errors.As(err, &clash) {
		return &trailResumeReportError{
			Type:         "worktree_clash",
			Message:      clash.Error(),
			Branch:       clash.Branch,
			WorktreePath: clash.WorktreePath,
		}
	}
	var noCheckpoint *ResumeNoCheckpointError
	if errors.As(err, &noCheckpoint) {
		return &trailResumeReportError{
			Type:    "no_checkpoint",
			Message: noCheckpoint.Error(),
			Branch:  noCheckpoint.Branch,
		}
	}
	var unavailable *ResumeMetadataUnavailableError
	if errors.As(err, &unavailable) {
		return &trailResumeReportError{
			Type:         "metadata_unavailable",
			Message:      unavailable.Error(),
			CheckpointID: unavailable.CheckpointID.String(),
		}
	}
	return nil
}

func buildTrailResumeActionReport(
	found api.TrailResource,
	branch string,
	actions trailResumeReportActions,
	sessions []strategy.RestoredSession,
	preferredSessionID string,
) trailResumeActionReport {
	report := trailResumeActionReport{
		Trail: trailResumeReportTrail{
			ID:     strings.TrimSpace(found.ID),
			Number: found.Number,
			Title:  strings.TrimSpace(found.Title),
			Branch: strings.TrimSpace(branch),
		},
		Actions: actions,
	}
	report.Actions.RestoredSessions = make([]trailResumeReportSession, 0, len(sessions))
	for _, session := range sessions {
		report.Actions.RestoredSessions = append(report.Actions.RestoredSessions, trailResumeReportSession{
			SessionID: session.SessionID,
			Agent:     string(session.Agent),
			Kind:      session.Kind,
		})
	}
	if report.Actions.CheckpointID == "" {
		report.Actions.CheckpointID = restoredSessionsCheckpointID(sessions)
	}
	report.Continuation = trailResumeContinuationFor(sessions, preferredSessionID)
	return report
}

func trailResumeContinuationFor(sessions []strategy.RestoredSession, preferredSessionID string) *trailResumeContinuation {
	if len(sessions) == 0 {
		return nil
	}
	choice := buildTrailResumeRestoredSessionChoices(sessions)[0].Session
	if preferredSessionID != "" {
		if preferred, ok := findTrailRestoredSession(sessions, preferredSessionID); ok {
			choice = preferred
		}
	}
	continuation := &trailResumeContinuation{
		Agent:     string(choice.Agent),
		SessionID: choice.SessionID,
	}
	if sessionAgent, err := strategy.ResolveAgentForRewind(choice.Agent); err == nil {
		continuation.Command = sessionAgent.FormatResumeCommand(choice.SessionID)
	}
	return continuation
}

// runTrailResumeJSON drives an actual resume with a JSON action report on
// stdout. Human act-path output is suppressed (warnings stay on stderr);
// typed failures emit the report with an error object and exit non-zero.
func runTrailResumeJSON(ctx context.Context, cmd *cobra.Command, found api.TrailResource, branch string, opts trailResumeOptions) error {
	errW := cmd.ErrOrStderr()

	var actions trailResumeReportActions
	emit := func(report trailResumeActionReport) error {
		enc := json.NewEncoder(cmd.OutOrStdout())
		enc.SetIndent("", "  ")
		if err := enc.Encode(report); err != nil {
			return fmt.Errorf("encode trail resume action report: %w", err)
		}
		return nil
	}
	fail := func(err error) error {
		reportErr := trailResumeReportErrorFrom(err)
		if reportErr == nil {
			return err
		}
		report := buildTrailResumeActionReport(found, branch, actions, nil, "")
		report.Continuation = nil
		report.Error = reportErr
		if encodeErr := emit(report); encodeErr != nil {
			return encodeErr
		}
		return NewSilentError(err)
	}

	if err := ensureTrailResumeBranchAvailable(ctx, io.Discard, branch); err != nil {
		return fail(err)
	}

	// Both are best-effort facts for the report: if the repo is unreadable the
	// switch below fails and reports the real error.
	currentBranch, branchErr := GetCurrentBranch(ctx)
	if branchErr != nil {
		currentBranch = branch
	}
	existedLocally, existsErr := BranchExistsLocally(ctx, branch)
	if existsErr != nil {
		existedLocally = true
	}
	proceed, err := switchToBranchForResume(ctx, io.Discard, errW, branch, true)
	if err != nil {
		return fail(err)
	}
	if !proceed {
		return errors.New("resume stopped before switching branches")
	}
	actions.SwitchedBranch = currentBranch != branch
	actions.FetchedBranch = !existedLocally
	actions.CheckpointBehindHead = trailResumeCheckpointBehindHead(ctx, branch)

	sessions, preferred, err := restoreTrailSessionsForReport(ctx, errW, branch, opts)
	if err != nil {
		return fail(err)
	}

	return emit(buildTrailResumeActionReport(found, branch, actions, sessions, preferred))
}

// trailResumeCheckpointBehindHead reports how many branch commits are newer
// than the latest checkpointed commit (0 when HEAD has the checkpoint or the
// count cannot be determined; best-effort).
func trailResumeCheckpointBehindHead(ctx context.Context, branch string) int {
	repo, err := openRepository(ctx)
	if err != nil {
		return 0
	}
	defer repo.Close()
	result, err := findBranchCheckpoints(repo, branch)
	if err != nil || result == nil || !result.newerCommitsExist {
		return 0
	}
	return result.newerCommitCount
}

// restoreTrailSessionsForReport performs the restore for the JSON act path,
// applying the same --checkpoint / --session / latest selection as the human
// act path but with human output discarded.
func restoreTrailSessionsForReport(ctx context.Context, errW io.Writer, branch string, opts trailResumeOptions) ([]strategy.RestoredSession, string, error) {
	if opts.CheckpointID != "" {
		sessions, err := restoreByCheckpointID(ctx, io.Discard, errW, id.CheckpointID(opts.CheckpointID), opts.Force)
		return sessions, "", err
	}
	if opts.SessionID != "" {
		contexts, _, ctxErr := resolveTrailResumeSessionContexts(ctx, branch)
		if ctxErr == nil {
			if sessionCtx, ok := findTrailResumeSession(contexts, opts.SessionID); ok {
				sessions, err := restoreByCheckpointID(ctx, io.Discard, errW, id.CheckpointID(sessionCtx.CheckpointID), opts.Force)
				return sessions, opts.SessionID, err
			}
		} else {
			fmt.Fprintf(errW, "Warning: could not load trail checkpoint sessions: %v\n", ctxErr)
		}
		sessions, err := restoreFromCurrentBranch(ctx, io.Discard, errW, branch, opts.Force)
		return sessions, opts.SessionID, err
	}
	sessions, err := restoreFromCurrentBranch(ctx, io.Discard, errW, branch, opts.Force)
	return sessions, "", err
}
