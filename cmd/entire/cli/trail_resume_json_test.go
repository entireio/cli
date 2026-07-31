package cli

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
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

	report := buildTrailResumeActionReport(found, "trail-resume", actions, sessions, "")

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

	report := buildTrailResumeActionReport(found, "feat", trailResumeReportActions{}, sessions, "sess-b")
	if report.Continuation == nil || report.Continuation.SessionID != "sess-b" {
		t.Errorf("continuation = %+v, want preferred session sess-b", report.Continuation)
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
	err := ensurePreferredRestoredSession(sessions, "sess-bogus")
	if err == nil {
		t.Fatal("bogus preferred session: error = nil, want not-found error")
	}
	var notFound *ResumeSessionNotFoundError
	if !errors.As(err, &notFound) || notFound.SessionID != "sess-bogus" {
		t.Errorf("error = %v, want ResumeSessionNotFoundError naming sess-bogus", err)
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

	t.Run("untyped error stays out of JSON", func(t *testing.T) {
		t.Parallel()
		if reportErr := trailResumeReportErrorFrom(errors.New("dial tcp: connection refused")); reportErr != nil {
			t.Errorf("trailResumeReportErrorFrom() = %+v, want nil for untyped error", reportErr)
		}
	})
}
