package cli

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/api"
)

func TestGroupSessionsByDay_SortsNewestFirst(t *testing.T) {
	t.Parallel()

	at := func(year int, month time.Month, day int) string {
		return time.Date(year, month, day, 12, 0, 0, 0, time.Local).Format(time.RFC3339)
	}

	sessions := []userSession{
		{SessionID: "aaa", LastActivityAt: at(2026, time.January, 10)},
		{SessionID: "bbb", LastActivityAt: at(2026, time.January, 12)},
		{SessionID: "ccc", LastActivityAt: at(2026, time.January, 11)},
	}
	days := groupSessionsByDay(sessions)

	if len(days) != 3 {
		t.Fatalf("got %d day groups, want 3", len(days))
	}
	// Newest first: 2026-01-12, 2026-01-11, 2026-01-10.
	if days[0].Sessions[0].SessionID != "bbb" {
		t.Errorf("first day should contain session bbb (2026-01-12), got %q", days[0].Sessions[0].SessionID)
	}
	if days[1].Sessions[0].SessionID != "ccc" {
		t.Errorf("second day should contain session ccc (2026-01-11), got %q", days[1].Sessions[0].SessionID)
	}
	if days[2].Sessions[0].SessionID != "aaa" {
		t.Errorf("third day should contain session aaa (2026-01-10), got %q", days[2].Sessions[0].SessionID)
	}
}

func TestGroupSessionsByDay_UnknownDatesLast(t *testing.T) {
	t.Parallel()
	sessions := []userSession{
		{SessionID: "bad", LastActivityAt: ""},
		{SessionID: "good", LastActivityAt: "2026-01-15T10:00:00Z"},
	}
	days := groupSessionsByDay(sessions)

	if len(days) != 2 {
		t.Fatalf("got %d day groups, want 2", len(days))
	}
	if days[0].Date == dateUnknown {
		t.Errorf("unknown-date sessions should sort last, but appeared first")
	}
	if days[1].Date != dateUnknown {
		t.Errorf("unknown-date sessions should be the last group, got %q", days[1].Date)
	}
}

func TestFetchSessions_ParsesEnvelopeAndSendsWindow(t *testing.T) {
	t.Parallel()

	var gotPath, gotTimeframe, gotLimit string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotTimeframe = r.URL.Query().Get("timeframe")
		gotLimit = r.URL.Query().Get("limit")
		w.Header().Set("Content-Type", "application/json")
		if _, err := w.Write([]byte(`{
			"sessions": [
				{"sessionId":"s1","displayName":"Add sessions list","isPublic":false,
				 "agent":"claude","model":"claude-opus-4-6","lastActivityAt":"2026-01-15T10:00:00Z",
				 "checkpointCount":3,"repo_full_name":"org/repo","is_private":true}
			],
			"timeframe":"last-month",
			"updated_at":"2026-01-15T10:00:00.000Z"
		}`)); err != nil {
			t.Errorf("write response: %v", err)
		}
	}))
	defer srv.Close()

	client := api.NewClientWithBaseURL("tok", srv.URL)
	sessions, err := fetchSessions(t.Context(), client)
	if err != nil {
		t.Fatalf("fetchSessions: %v", err)
	}

	if gotPath != "/api/v1/me/sessions" {
		t.Errorf("path = %q, want /api/v1/me/sessions", gotPath)
	}
	if gotTimeframe != activityTimeframe {
		t.Errorf("timeframe = %q, want %q", gotTimeframe, activityTimeframe)
	}
	if gotLimit != "50" {
		t.Errorf("limit = %q, want 50", gotLimit)
	}

	if len(sessions) != 1 {
		t.Fatalf("got %d sessions, want 1", len(sessions))
	}
	s := sessions[0]
	if s.SessionID != "s1" || s.DisplayName != "Add sessions list" {
		t.Errorf("unexpected session identity: %+v", s)
	}
	if s.Agent == nil || *s.Agent != activityTestAgentClaude {
		t.Errorf("agent = %v, want claude", s.Agent)
	}
	if s.Model == nil || *s.Model != "claude-opus-4-6" {
		t.Errorf("model = %v, want claude-opus-4-6", s.Model)
	}
	if s.CheckpointCount != 3 || s.RepoFullName != "org/repo" {
		t.Errorf("checkpointCount/repo = %d/%q, want 3/org/repo", s.CheckpointCount, s.RepoFullName)
	}
}

func TestRenderSessionList_ShowsRowFields(t *testing.T) {
	t.Parallel()
	agent := activityTestAgentClaude
	model := "claude-opus-4-6"
	days := []sessionDay{
		{Date: "2026-01-15", Sessions: []userSession{
			{
				SessionID:       "s1",
				DisplayName:     "Add sessions list to activity",
				Agent:           &agent,
				Model:           &model,
				LastActivityAt:  "2026-01-15T10:00:00Z",
				CheckpointCount: 3,
				RepoFullName:    "org/repo",
			},
		}},
	}

	var buf strings.Builder
	sty := activityStyles{width: 100}
	renderSessionList(&buf, sty, days)
	out := buf.String()

	for _, want := range []string{
		"Add sessions list to activity", // title
		"org/repo",                      // repo
		"Claude Code",                   // agent label
		"Opus 4.6",                      // formatted model
		"3 checkpoints",                 // checkpoint count
		"1 session",                     // day header count (singular)
	} {
		if !strings.Contains(out, want) {
			t.Errorf("session row output missing %q\n---\n%s", want, out)
		}
	}
}

func TestRenderSessionList_PluralAndUntitled(t *testing.T) {
	t.Parallel()
	days := []sessionDay{
		{Date: "2026-01-15", Sessions: []userSession{
			{DisplayName: "", CheckpointCount: 1, RepoFullName: "org/repo"},
			{DisplayName: "second", CheckpointCount: 0, RepoFullName: "org/repo"},
		}},
	}

	var buf strings.Builder
	sty := activityStyles{width: 100}
	renderSessionList(&buf, sty, days)
	out := buf.String()

	if !strings.Contains(out, "2 sessions") {
		t.Error("day header should say '2 sessions' (plural)")
	}
	if !strings.Contains(out, "(untitled session)") {
		t.Error("empty display name should render as '(untitled session)'")
	}
	if !strings.Contains(out, "1 checkpoint") || strings.Contains(out, "1 checkpoints") {
		t.Error("checkpoint count should be singular '1 checkpoint'")
	}
}
