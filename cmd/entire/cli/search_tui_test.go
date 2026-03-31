package cli

import (
	"bytes"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/entireio/cli/cmd/entire/cli/search"
)

func testResults() []search.Result {
	sha1 := "e4f5a6b7c8d9"
	msg1 := "Implement auth middleware"
	user1 := "alicecodes"

	sha2 := "1a2b3c4d5e6f"
	msg2 := "Add JWT token refresh"

	return []search.Result{
		{
			Type: "checkpoint",
			Data: search.CheckpointResult{
				ID:             "a3b2c4d5e6f7",
				Prompt:         "add auth middleware to protect API routes",
				CommitSHA:      &sha1,
				CommitMessage:  &msg1,
				Branch:         "main",
				Org:            "entirehq",
				Repo:           "entire.io",
				Author:         "alice",
				AuthorUsername: &user1,
				CreatedAt:      "2026-03-24T10:30:00Z",
				FilesTouched:   []string{"src/middleware/auth.go", "src/handlers/login.go"},
			},
			Meta: search.Meta{
				MatchType: "semantic",
				Score:     0.042,
				Snippet:   "added auth middleware for JWT validation",
			},
		},
		{
			Type: "checkpoint",
			Data: search.CheckpointResult{
				ID:            "d5e6f789ab01",
				Prompt:        "fix auth token refresh",
				CommitSHA:     &sha2,
				CommitMessage: &msg2,
				Branch:        "feat/login",
				Org:           "entirehq",
				Repo:          "entire.io",
				Author:        "bob",
				CreatedAt:     "2026-03-20T14:00:00Z",
				FilesTouched:  []string{"src/auth/jwt.go"},
			},
			Meta: search.Meta{
				MatchType: "both",
				Score:     0.035,
			},
		},
	}
}

func testModel() searchModel {
	ss := statusStyles{colorEnabled: false, width: 100}
	cfg := search.Config{ServiceURL: "http://test", Owner: "o", Repo: "r", Limit: 20}
	return newSearchModel(testResults(), "auth", 2, cfg, ss)
}

// updateModel is a test helper that sends a message and returns the updated searchModel.
func updateModel(t *testing.T, m searchModel, msg tea.Msg) searchModel {
	t.Helper()
	updated, _ := m.Update(msg)
	result, ok := updated.(searchModel)
	if !ok {
		t.Fatalf("Update returned %T, want searchModel", updated)
	}
	return result
}

func TestSearchModel_Navigation(t *testing.T) {
	t.Parallel()
	m := testModel()

	if m.cursor != 0 {
		t.Fatalf("initial cursor = %d, want 0", m.cursor)
	}

	m = updateModel(t, m, tea.KeyMsg{Type: tea.KeyDown})
	if m.cursor != 1 {
		t.Errorf("after down: cursor = %d, want 1", m.cursor)
	}

	m = updateModel(t, m, tea.KeyMsg{Type: tea.KeyDown})
	if m.cursor != 1 {
		t.Errorf("after down at bottom: cursor = %d, want 1", m.cursor)
	}

	m = updateModel(t, m, tea.KeyMsg{Type: tea.KeyUp})
	if m.cursor != 0 {
		t.Errorf("after up: cursor = %d, want 0", m.cursor)
	}

	m = updateModel(t, m, tea.KeyMsg{Type: tea.KeyUp})
	if m.cursor != 0 {
		t.Errorf("after up at top: cursor = %d, want 0", m.cursor)
	}
}

func TestSearchModel_Quit(t *testing.T) {
	t.Parallel()
	m := testModel()

	quitKeys := []tea.KeyMsg{
		{Type: tea.KeyRunes, Runes: []rune{'q'}},
		{Type: tea.KeyCtrlC},
	}

	for _, key := range quitKeys {
		_, cmd := m.Update(key)
		if cmd == nil {
			t.Errorf("key %v: expected quit command, got nil", key)
			continue
		}
		msg := cmd()
		if _, ok := msg.(tea.QuitMsg); !ok {
			t.Errorf("key %v: expected QuitMsg, got %T", key, msg)
		}
	}
}

func TestSearchModel_SearchMode(t *testing.T) {
	t.Parallel()
	m := testModel()

	if m.mode != modeBrowse {
		t.Fatalf("initial mode = %d, want modeBrowse", m.mode)
	}

	m = updateModel(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'/'}})
	if m.mode != modeSearch {
		t.Errorf("after /: mode = %d, want modeSearch", m.mode)
	}

	m = updateModel(t, m, tea.KeyMsg{Type: tea.KeyEsc})
	if m.mode != modeBrowse {
		t.Errorf("after esc: mode = %d, want modeBrowse", m.mode)
	}
}

func TestSearchModel_SearchModeEnter(t *testing.T) {
	t.Parallel()
	m := testModel()

	// Enter search mode
	m = updateModel(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'/'}})
	// Type a query
	m.input.SetValue("new query")

	// Press enter — should set loading and return to browse mode
	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m, ok := updated.(searchModel)
	if !ok {
		t.Fatalf("Update returned %T, want searchModel", updated)
	}
	if m.mode != modeBrowse {
		t.Errorf("after enter: mode = %d, want modeBrowse", m.mode)
	}
	if !m.loading {
		t.Error("after enter: loading should be true")
	}
	if cmd == nil {
		t.Error("after enter: expected a command for search")
	}
}

func TestSearchModel_SearchModeEnterEmpty(t *testing.T) {
	t.Parallel()
	m := testModel()

	// Enter search mode with empty query
	m = updateModel(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'/'}})
	m.input.SetValue("   ")

	// Press enter — should be a no-op (stay in search mode)
	m = updateModel(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	if m.mode != modeSearch {
		t.Errorf("after enter with empty query: mode = %d, want modeSearch", m.mode)
	}
	if m.loading {
		t.Error("after enter with empty query: loading should be false")
	}
}

func TestSearchModel_View(t *testing.T) {
	t.Parallel()
	m := testModel()
	view := m.View()

	// Section headers
	if !strings.Contains(view, "SEARCH") {
		t.Error("view missing SEARCH section header")
	}
	if !strings.Contains(view, "RESULTS") {
		t.Error("view missing RESULTS section header")
	}

	// Search bar shows query
	if !strings.Contains(view, "auth") {
		t.Error("view missing query in search bar")
	}

	// Column headers
	for _, col := range []string{"Age", "ID", "Branch", "Prompt", "Author"} {
		if !strings.Contains(view, col) {
			t.Errorf("view missing column header %q", col)
		}
	}

	// Table data
	if !strings.Contains(view, "a3b2c4d5e6f") {
		t.Error("view missing first result ID")
	}

	// Detail card content
	if !strings.Contains(view, "Checkpoint Detail") {
		t.Error("view missing detail card title")
	}
	if !strings.Contains(view, "add auth middleware to protect API routes") {
		t.Error("detail missing full prompt")
	}
	if !strings.Contains(view, "e4f5a6b") {
		t.Error("detail missing commit SHA")
	}
	if !strings.Contains(view, "entirehq/entire.io") {
		t.Error("detail missing repo")
	}
	if !strings.Contains(view, "@alicecodes") {
		t.Error("detail missing username")
	}
	if !strings.Contains(view, "semantic") {
		t.Error("detail missing match type")
	}
	if !strings.Contains(view, "src/middleware/auth.go") {
		t.Error("detail missing files")
	}

	// Footer
	if !strings.Contains(view, "navigate") {
		t.Error("view missing footer help")
	}
	if !strings.Contains(view, "2 results") {
		t.Error("view missing results count in footer")
	}
}

func TestSearchModel_ViewNoResults(t *testing.T) {
	t.Parallel()
	ss := statusStyles{colorEnabled: false, width: 80}
	cfg := search.Config{}
	m := newSearchModel(nil, "nothing", 0, cfg, ss)
	view := m.View()

	if !strings.Contains(view, "No results found") {
		t.Error("view should show no results message")
	}
}

func TestSearchModel_WindowResize(t *testing.T) {
	t.Parallel()
	m := testModel()

	m = updateModel(t, m, tea.WindowSizeMsg{Width: 120, Height: 40})
	if m.width != 120 {
		t.Errorf("after resize: width = %d, want 120", m.width)
	}
}

func TestSearchModel_ViewZeroWidth(t *testing.T) {
	t.Parallel()
	ss := statusStyles{colorEnabled: false, width: 0}
	cfg := search.Config{}
	m := newSearchModel(testResults(), "auth", 2, cfg, ss)
	m.width = 0

	if view := m.View(); view != "" {
		t.Errorf("view with zero width should be empty, got %q", view)
	}
}

func TestSearchModel_SearchResultsMsg(t *testing.T) {
	t.Parallel()
	m := testModel()
	m.loading = true

	newResults := testResults()[:1]
	m = updateModel(t, m, searchResultsMsg{results: newResults, total: 1})

	if m.loading {
		t.Error("loading should be false after results msg")
	}
	if len(m.results) != 1 {
		t.Errorf("results = %d, want 1", len(m.results))
	}
	if m.cursor != 0 {
		t.Errorf("cursor should reset to 0, got %d", m.cursor)
	}
}

func TestSearchModel_SearchResultsMsgError(t *testing.T) {
	t.Parallel()
	m := testModel()
	m.loading = true

	m = updateModel(t, m, searchResultsMsg{err: errTestSearch})

	if m.loading {
		t.Error("loading should be false after error msg")
	}
	if m.searchErr == "" {
		t.Error("searchErr should be set")
	}
}

var errTestSearch = &testError{"search failed"}

type testError struct{ msg string }

func (e *testError) Error() string { return e.msg }

func TestFormatSearchAge(t *testing.T) {
	t.Parallel()

	age := formatSearchAge("2026-03-25T10:00:00Z")
	if age == "2026-03-25T10:00:00Z" {
		t.Error("formatSearchAge returned raw timestamp instead of relative time")
	}

	age = formatSearchAge("not-a-date")
	if age != "not-a-date" {
		t.Errorf("formatSearchAge for invalid date = %q, want %q", age, "not-a-date")
	}
}

func TestFormatCommit(t *testing.T) {
	t.Parallel()

	sha := "e4f5a6b7c8d9e0f1"
	msg := "Fix the login bug"
	got := formatCommit(&sha, &msg)
	if !strings.Contains(got, "e4f5a6b") {
		t.Error("formatCommit missing truncated SHA")
	}
	if !strings.Contains(got, "Fix the login bug") {
		t.Error("formatCommit missing message")
	}

	got = formatCommit(nil, &msg)
	if !strings.Contains(got, "—") {
		t.Error("formatCommit with nil SHA should show dash")
	}

	got = formatCommit(&sha, nil)
	if !strings.HasPrefix(got, "e4f5a6b") {
		t.Errorf("formatCommit with nil message should start with SHA, got %q", got)
	}
}

func TestFormatAuthor(t *testing.T) {
	t.Parallel()

	username := "alicecodes"
	if got := formatAuthor("alice", &username); got != "alice (@alicecodes)" {
		t.Errorf("formatAuthor = %q, want %q", got, "alice (@alicecodes)")
	}

	if got := formatAuthor("bob", nil); got != "bob" {
		t.Errorf("formatAuthor(nil username) = %q, want %q", got, "bob")
	}
}

func TestRenderSearchStatic(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	styles := statusStyles{colorEnabled: false, width: 100}
	renderSearchStatic(&buf, testResults(), "auth", 2, styles)
	output := buf.String()

	if !strings.Contains(output, `Found 2 checkpoints matching "auth"`) {
		t.Error("static output missing header")
	}
	if !strings.Contains(output, "a3b2c4d5e6") {
		t.Error("static output missing first result ID")
	}
	if !strings.Contains(output, "d5e6f789ab") {
		t.Error("static output missing second result ID")
	}
}

func TestComputeColumns(t *testing.T) {
	t.Parallel()

	cols := computeColumns(100)
	if cols.age != 10 {
		t.Errorf("age width = %d, want 10", cols.age)
	}
	if cols.id != 12 {
		t.Errorf("id width = %d, want 12", cols.id)
	}
	if cols.author != 14 {
		t.Errorf("author width = %d, want 14", cols.author)
	}

	cols = computeColumns(40)
	if cols.branch < 8 {
		t.Errorf("branch width on narrow terminal = %d, want >= 8", cols.branch)
	}
}
