package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/logging"
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
// exit; untyped errors and pre-resume failures (trail lookup, flag
// validation) keep stdout empty with text on stderr.
type trailResumeReportError struct {
	Type         string `json:"type"`
	Message      string `json:"message"`
	Branch       string `json:"branch,omitempty"`
	WorktreePath string `json:"worktree_path,omitempty"`
	CheckpointID string `json:"checkpoint_id,omitempty"`
	SessionID    string `json:"session_id,omitempty"`
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
	var sessionNotFound *ResumeSessionNotFoundError
	if errors.As(err, &sessionNotFound) {
		return &trailResumeReportError{
			Type:      "session_not_found",
			Message:   sessionNotFound.Error(),
			SessionID: sessionNotFound.SessionID,
		}
	}
	var noSessions *ResumeNoSessionsRestoredError
	if errors.As(err, &noSessions) {
		return &trailResumeReportError{
			Type:    "no_sessions_restored",
			Message: noSessions.Error(),
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
) (trailResumeActionReport, error) {
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
	if len(sessions) > 0 {
		continuation, err := trailResumeContinuationFor(sessions, preferredSessionID)
		if err != nil {
			return report, err
		}
		report.Continuation = continuation
	}
	return report, nil
}

// trailResumeContinuationFor builds the continuation for the default (or
// preferred) session. An unresolvable agent is an error, matching the text
// path. Callers guarantee sessions is non-empty.
func trailResumeContinuationFor(sessions []strategy.RestoredSession, preferredSessionID string) (*trailResumeContinuation, error) {
	choice := buildTrailResumeRestoredSessionChoices(sessions)[0].Session
	if preferredSessionID != "" {
		if preferred, ok := findTrailRestoredSession(sessions, preferredSessionID); ok {
			choice = preferred
		}
	}
	sessionAgent, err := strategy.ResolveAgentForRewind(choice.Agent)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve agent for session %s: %w", choice.SessionID, err)
	}
	return &trailResumeContinuation{
		Agent:     string(choice.Agent),
		SessionID: choice.SessionID,
		Command:   sessionAgent.FormatResumeCommand(choice.SessionID),
	}, nil
}

// runTrailResumeJSON drives an actual resume with a JSON action report on
// stdout. Human act-path output is suppressed (warnings stay on stderr);
// typed failures emit the report with an error object and exit non-zero.
func runTrailResumeJSON(ctx context.Context, cmd *cobra.Command, found api.TrailResource, branch string, opts trailResumeOptions) error {
	errW := cmd.ErrOrStderr()

	var actions trailResumeReportActions
	var restored []strategy.RestoredSession
	emit := func(report trailResumeActionReport) error {
		enc := json.NewEncoder(cmd.OutOrStdout())
		enc.SetEscapeHTML(false)
		enc.SetIndent("", "  ")
		if err := enc.Encode(report); err != nil {
			return fmt.Errorf("encode trail resume action report: %w", err)
		}
		return nil
	}
	// fail emits the report carrying any side effects that already happened
	// (branch switch, restored sessions) before returning the typed error.
	fail := func(err error) error {
		reportErr := trailResumeReportErrorFrom(err)
		if reportErr == nil {
			return err
		}
		// The builder's continuation error is irrelevant here: Continuation
		// is nil'd below.
		report, buildErr := buildTrailResumeActionReport(found, branch, actions, restored, "")
		if buildErr != nil {
			logging.Debug(ctx, "trail resume json: continuation unresolvable in failure report (ignored)",
				slog.String("error", buildErr.Error()))
		}
		report.Continuation = nil
		report.Error = reportErr
		if encodeErr := emit(report); encodeErr != nil {
			return fmt.Errorf("%w (also failed to encode the action report: %w)", err, encodeErr)
		}
		return NewSilentError(err)
	}

	// Recorded up front so failure reports carry the requested checkpoint.
	if checkpointID := strings.TrimSpace(opts.CheckpointID); checkpointID != "" {
		actions.CheckpointID = checkpointID
	}

	if err := ensureTrailResumeBranchAvailable(ctx, errW, branch); err != nil {
		return fail(err)
	}

	// Best-effort fact for the report: on detached HEAD (or any read failure)
	// there is no current branch, so a successful checkout below did switch.
	currentBranch, branchErr := GetCurrentBranch(ctx)
	if branchErr != nil {
		logging.Debug(ctx, "trail resume json: current branch unreadable, reporting checkout as a switch",
			slog.String("error", branchErr.Error()))
		currentBranch = ""
	}
	existedLocally, existsErr := BranchExistsLocally(ctx, branch)
	if existsErr != nil {
		logging.Debug(ctx, "trail resume json: local branch existence unreadable, reporting no fetch",
			slog.String("error", existsErr.Error()))
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
	// behind-head describes the branch's LATEST checkpoint; --checkpoint and
	// --session may resume an older one, for which the count would mislead.
	if opts.CheckpointID == "" && opts.SessionID == "" {
		actions.CheckpointBehindHead = trailResumeCheckpointBehindHead(ctx, branch)
	}

	sessions, preferred, err := restoreTrailSessionsForReport(ctx, errW, branch, opts)
	if err != nil {
		return fail(err)
	}
	restored = sessions
	if len(sessions) == 0 {
		// Checkpoint resolved but its restore produced nothing (e.g. session
		// log content unavailable): nothing was resumed.
		return fail(&ResumeNoSessionsRestoredError{})
	}
	if err := ensurePreferredRestoredSession(sessions, preferred); err != nil {
		return fail(err)
	}

	report, err := buildTrailResumeActionReport(found, branch, actions, sessions, preferred)
	if err != nil {
		// Unresolvable agent: untyped, so stdout stays empty like the text path.
		return err
	}
	return emit(report)
}

// ensurePreferredRestoredSession rejects a --session id that is not among
// the restored sessions: the continuation must never point at a different
// session than the one requested.
func ensurePreferredRestoredSession(sessions []strategy.RestoredSession, preferredSessionID string) error {
	if preferredSessionID == "" {
		return nil
	}
	if _, ok := findTrailRestoredSession(sessions, preferredSessionID); ok {
		return nil
	}
	return &ResumeSessionNotFoundError{SessionID: preferredSessionID}
}

// trailResumeCheckpointBehindHead reports how many branch commits are newer
// than the latest checkpointed commit (0 when HEAD has the checkpoint or the
// count cannot be determined; best-effort).
func trailResumeCheckpointBehindHead(ctx context.Context, branch string) int {
	repo, err := openRepository(ctx)
	if err != nil {
		logging.Debug(ctx, "trail resume json: behind-head count unavailable",
			slog.String("error", err.Error()))
		return 0
	}
	defer repo.Close()
	result, err := findBranchCheckpoints(repo, branch)
	if err != nil {
		logging.Debug(ctx, "trail resume json: behind-head count unavailable",
			slog.String("error", err.Error()))
		return 0
	}
	if result == nil || !result.newerCommitsExist {
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
		// Fail before any restore: a fallback restore of the latest
		// checkpoint would overwrite unrelated session logs under --force.
		contexts, _, ctxErr := resolveTrailResumeSessionContexts(ctx, branch)
		if ctxErr != nil {
			return nil, "", fmt.Errorf("cannot resolve --session %s: loading trail checkpoint sessions failed: %w", opts.SessionID, ctxErr)
		}
		sessionCtx, ok := findTrailResumeSession(contexts, opts.SessionID)
		if !ok {
			return nil, "", &ResumeSessionNotFoundError{SessionID: opts.SessionID}
		}
		sessions, err := restoreByCheckpointID(ctx, io.Discard, errW, id.CheckpointID(sessionCtx.CheckpointID), opts.Force)
		return sessions, opts.SessionID, err
	}
	sessions, err := restoreFromCurrentBranch(ctx, io.Discard, errW, branch, opts.Force, false)
	return sessions, "", err
}
