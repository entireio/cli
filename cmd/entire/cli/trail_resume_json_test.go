package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/strategy"

	"github.com/spf13/cobra"
)

// TestBuildTrailResumeActionReport_Success pins the --json act-path payload:
// trail identity, actions taken, restored sessions, and a structured
// continuation (agent + session id + command) for headless orchestrators.
func TestBuildTrailResumeActionReport_Success(t *testing.T) {
	t.Parallel()

	found := api.TrailResource{ID: "tr_1", Number: 42, Title: "Improve resume", Branch: "trail-resume"}
	actions := trailResumeReportActions{
		FetchedBranch:        false,
		SwitchedBranch:       true,
		CheckpointBehindHead: 2,
	}
	sessions := []strategy.RestoredSession{
		{
			SessionID:    "sess-work",
			Agent:        "Claude Code",
			CheckpointID: "beefc0ffee12",
			CreatedAt:    time.Date(2026, 2, 2, 12, 0, 0, 0, time.UTC),
		},
		{
			SessionID:    "sess-review",
			Agent:        "Claude Code",
			CheckpointID: "beefc0ffee12",
			Prompt:       "Review the code changes on this branch",
			CreatedAt:    time.Date(2026, 2, 2, 13, 0, 0, 0, time.UTC),
			Kind:         "agent_review",
		},
	}

	report, buildErr := buildTrailResumeActionReport(found, "trail-resume", actions, sessions, "")
	if buildErr != nil {
		t.Fatalf("buildTrailResumeActionReport() error = %v", buildErr)
	}

	data, err := json.Marshal(report)
	if err != nil {
		t.Fatalf("marshal report: %v", err)
	}
	payload := string(data)

	for _, want := range []string{
		`"number":42`,
		`"branch":"trail-resume"`,
		`"switched_branch":true`,
		`"checkpoint_behind_head":2`,
		`"checkpoint_id":"beefc0ffee12"`,
		`"session_id":"sess-work"`,
		`"session_id":"sess-review"`,
		`"kind":"agent_review"`,
		`"command":"claude -r sess-work"`,
	} {
		if !strings.Contains(payload, want) {
			t.Errorf("payload missing %s:\n%s", want, payload)
		}
	}
	if report.Continuation == nil || report.Continuation.SessionID != "sess-work" {
		t.Errorf("continuation = %+v, want default work session sess-work", report.Continuation)
	}
	if strings.Contains(payload, `"error"`) {
		t.Errorf("success payload must omit error, got:\n%s", payload)
	}
}

func TestBuildTrailResumeActionReport_PreferredSessionWins(t *testing.T) {
	t.Parallel()

	found := api.TrailResource{Number: 7, Branch: "feat"}
	sessions := []strategy.RestoredSession{
		{SessionID: "sess-a", Agent: "Claude Code", CreatedAt: time.Date(2026, 2, 2, 12, 0, 0, 0, time.UTC)},
		{SessionID: "sess-b", Agent: "Claude Code", CreatedAt: time.Date(2026, 2, 1, 12, 0, 0, 0, time.UTC)},
	}

	report, err := buildTrailResumeActionReport(found, "feat", trailResumeReportActions{}, sessions, "sess-b")
	if err != nil {
		t.Fatalf("buildTrailResumeActionReport() error = %v", err)
	}
	if report.Continuation == nil || report.Continuation.SessionID != "sess-b" {
		t.Errorf("continuation = %+v, want preferred session sess-b", report.Continuation)
	}
}

// TestBuildTrailResumeActionReport_UnresolvableAgentErrors pins the JSON/text
// parity: an agent that cannot be resolved must error (as the text path
// does), never emit a success report whose continuation has no command.
func TestBuildTrailResumeActionReport_UnresolvableAgentErrors(t *testing.T) {
	t.Parallel()

	sessions := []strategy.RestoredSession{
		{SessionID: "sess-x", Agent: "definitely-not-a-real-agent", CreatedAt: time.Date(2026, 2, 2, 12, 0, 0, 0, time.UTC)},
	}
	_, err := buildTrailResumeActionReport(api.TrailResource{Number: 3, Branch: "feat"}, "feat", trailResumeReportActions{}, sessions, "")
	if err == nil {
		t.Fatal("buildTrailResumeActionReport() = nil error for unresolvable agent, want error")
	}
}

// TestEnsurePreferredRestoredSession pins the --session contract on the JSON
// path: a session id that is not among the restored sessions must error (as
// the human path does via continueRestoredSessions), never silently fall back
// to a different session with exit 0.
func TestEnsurePreferredRestoredSession(t *testing.T) {
	t.Parallel()

	sessions := []strategy.RestoredSession{{SessionID: "sess-a", Agent: "Claude Code"}}

	if err := ensurePreferredRestoredSession(sessions, ""); err != nil {
		t.Errorf("no preferred session: error = %v, want nil", err)
	}
	if err := ensurePreferredRestoredSession(sessions, "sess-a"); err != nil {
		t.Errorf("restored preferred session: error = %v, want nil", err)
	}
	bogusID := "sess-missing"
	err := ensurePreferredRestoredSession(sessions, bogusID)
	if err == nil {
		t.Fatal("bogus preferred session: error = nil, want not-found error")
	}
	var notFound *ResumeSessionNotFoundError
	if !errors.As(err, &notFound) || notFound.SessionID != bogusID {
		t.Errorf("error = %v, want ResumeSessionNotFoundError naming %s", err, bogusID)
	}
}

// TestEnsureSessionsRestored pins the shared zero-sessions contract: nothing
// restored is a failure for scripted callers (non-interactive or --force),
// while an interactive zero is treated as a prompt decline and stays exit 0.
func TestEnsureSessionsRestored(t *testing.T) {
	t.Parallel()

	sessions := []strategy.RestoredSession{{SessionID: "sess-a"}}
	if err := ensureSessionsRestored(sessions, false); err != nil {
		t.Errorf("restored sessions: error = %v, want nil", err)
	}
	// go test is non-interactive: zero sessions must fail.
	err := ensureSessionsRestored(nil, false)
	var noSessions *ResumeNoSessionsRestoredError
	if !errors.As(err, &noSessions) {
		t.Errorf("zero sessions non-interactive: error = %v, want ResumeNoSessionsRestoredError", err)
	}
	// --force always disables prompts, so zero sessions must fail even where
	// a terminal could prompt — agents pass --force.
	err = ensureSessionsRestored(nil, true)
	if !errors.As(err, &noSessions) {
		t.Errorf("zero sessions with --force: error = %v, want ResumeNoSessionsRestoredError", err)
	}
}

// TestTrailResumeReportErrorFrom pins the typed-error JSON contract: the enum
// values and per-type fields, plus nil for untyped errors (which keep the
// default empty-stdout stderr-text behavior).
func TestTrailResumeReportErrorFrom(t *testing.T) {
	t.Parallel()

	t.Run("worktree clash", func(t *testing.T) {
		t.Parallel()
		branch, wtPath := "feat-clash", "/wt"
		reportErr := trailResumeReportErrorFrom(NewSilentError(&ResumeWorktreeClashError{Branch: branch, WorktreePath: wtPath}))
		if reportErr == nil {
			t.Fatal("trailResumeReportErrorFrom() = nil, want worktree_clash")
		}
		if reportErr.Type != "worktree_clash" || reportErr.Branch != branch || reportErr.WorktreePath != wtPath {
			t.Errorf("report error = %+v, want worktree_clash with branch and path", reportErr)
		}
	})

	t.Run("no checkpoint", func(t *testing.T) {
		t.Parallel()
		branch := "feat-nocp"
		reportErr := trailResumeReportErrorFrom(&ResumeNoCheckpointError{Branch: branch})
		if reportErr == nil || reportErr.Type != "no_checkpoint" || reportErr.Branch != branch {
			t.Errorf("report error = %+v, want no_checkpoint with branch", reportErr)
		}
	})

	t.Run("metadata unavailable", func(t *testing.T) {
		t.Parallel()
		cpID := id.MustCheckpointID("1122aabb33cc")
		reportErr := trailResumeReportErrorFrom(NewSilentError(&ResumeMetadataUnavailableError{CheckpointID: cpID}))
		if reportErr == nil || reportErr.Type != "metadata_unavailable" || reportErr.CheckpointID != cpID.String() {
			t.Errorf("report error = %+v, want metadata_unavailable with checkpoint id", reportErr)
		}
	})

	t.Run("session not found", func(t *testing.T) {
		t.Parallel()
		reportErr := trailResumeReportErrorFrom(&ResumeSessionNotFoundError{SessionID: "sess-x"})
		if reportErr == nil || reportErr.Type != "session_not_found" || reportErr.SessionID != "sess-x" {
			t.Errorf("report error = %+v, want session_not_found with session id", reportErr)
		}
	})

	t.Run("no sessions restored", func(t *testing.T) {
		t.Parallel()
		reportErr := trailResumeReportErrorFrom(NewSilentError(&ResumeNoSessionsRestoredError{}))
		if reportErr == nil || reportErr.Type != "no_sessions_restored" {
			t.Errorf("report error = %+v, want no_sessions_restored", reportErr)
		}
	})

	t.Run("untyped error stays out of JSON", func(t *testing.T) {
		t.Parallel()
		if reportErr := trailResumeReportErrorFrom(errors.New("dial tcp: connection refused")); reportErr != nil {
			t.Errorf("trailResumeReportErrorFrom() = %+v, want nil for untyped error", reportErr)
		}
	})
}

// TestRunTrailResumeJSON_DrivesSuccessAndTypedFailure executes the JSON act
// path end to end against a real repo fixture — the orchestration the whole
// --json contract hangs on. Success: stdout is a single parseable JSON
// document whose continuation names the restored session. Typed failure
// (--session with an unknown id): the report is still emitted on stdout with
// an error object, and the command exits non-zero with the typed error
// recoverable via errors.As.
func TestRunTrailResumeJSON_DrivesSuccessAndTypedFailure(t *testing.T) {
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)
	t.Setenv("ENTIRE_TEST_CLAUDE_PROJECT_DIR", filepath.Join(tmpDir, "claude-projects"))

	repo, w, _ := setupResumeTestRepo(t, tmpDir, false)
	cpID := id.MustCheckpointID("abba0110abba")
	writeCommittedResumeCheckpoint(t, repo, cpID, "session-json-drive", time.Date(2025, 1, 1, 10, 0, 0, 0, time.UTC))
	writeResumeTestCommit(t, tmpDir, w, "checkpointed.txt", "Add feature\n\nEntire-Checkpoint: "+cpID.String())

	found := api.TrailResource{Number: 12, Title: "Drive the JSON path", Branch: "master"}

	newCmd := func() (*cobra.Command, *bytes.Buffer) {
		cmd := &cobra.Command{}
		var stdout, stderr bytes.Buffer
		cmd.SetOut(&stdout)
		cmd.SetErr(&stderr)
		return cmd, &stdout
	}

	// Success.
	cmd, stdout := newCmd()
	if err := runTrailResumeJSON(context.Background(), cmd, found, "master", trailResumeOptions{JSON: true}); err != nil {
		t.Fatalf("runTrailResumeJSON() error = %v\nstdout: %s", err, stdout.String())
	}
	var report map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatalf("success stdout is not a single JSON document: %v\n%s", err, stdout.String())
	}
	trail, ok := report["trail"].(map[string]any)
	if !ok || trail["number"] != float64(12) {
		t.Errorf("trail = %v, want number 12", report["trail"])
	}
	actionsPayload, ok := report["actions"].(map[string]any)
	if !ok {
		t.Fatalf("actions = %v, want an object", report["actions"])
	}
	if actionsPayload["switched_branch"] != false {
		t.Errorf("switched_branch = %v, want false (already on master)", actionsPayload["switched_branch"])
	}
	if restoredSessions, ok := actionsPayload["restored_sessions"].([]any); !ok || len(restoredSessions) == 0 {
		t.Errorf("restored_sessions = %v, want the fixture session", actionsPayload["restored_sessions"])
	}
	continuation, ok := report["continuation"].(map[string]any)
	if !ok || continuation["command"] != "claude -r session-json-drive" {
		t.Errorf("continuation = %v, want claude -r session-json-drive", report["continuation"])
	}
	if _, hasErr := report["error"]; hasErr {
		t.Errorf("success report carries an error object: %s", stdout.String())
	}

	// Typed failure: unknown --session id fails fast, report still on stdout.
	bogusID := "sess-bogus"
	cmd, stdout = newCmd()
	err := runTrailResumeJSON(context.Background(), cmd, found, "master", trailResumeOptions{JSON: true, SessionID: bogusID})
	var notFound *ResumeSessionNotFoundError
	if !errors.As(err, &notFound) || notFound.SessionID != bogusID {
		t.Fatalf("runTrailResumeJSON() error = %v, want ResumeSessionNotFoundError for %s", err, bogusID)
	}
	report = map[string]any{}
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatalf("failure stdout is not a single JSON document: %v\n%s", err, stdout.String())
	}
	reportErr, ok := report["error"].(map[string]any)
	if !ok || reportErr["type"] != "session_not_found" || reportErr["session_id"] != bogusID {
		t.Errorf("report error = %v, want session_not_found for %s", report["error"], bogusID)
	}
	if _, hasContinuation := report["continuation"]; hasContinuation {
		t.Errorf("failure report must not offer a continuation: %s", stdout.String())
	}
}
