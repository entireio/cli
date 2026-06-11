package cli

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/entireio/cli/cmd/entire/cli/search"
)

const newQuery = "new query"

func testResults() []search.Result {
	sha1 := "e4f5a6b7c8d9"
	msg1 := "Implement auth middleware"
	user1 := "alicecodes"

	sha2 := "1a2b3c4d5e6f"
	msg2 := "Add JWT token refresh"

	return []search.Result{
		{
			Type: "checkpoint",
			Checkpoint: &search.CheckpointResult{
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
			Checkpoint: &search.CheckpointResult{
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

func testMultiTypeResults() []search.Result {
	results := testResults()
	results = append(results,
		search.Result{
			Type: "commit",
			Commit: &search.CommitResult{
				ID:            "cm1",
				CommitSHA:     "abc1234567890",
				CommitMessage: "fix: auth token validation",
				CommitSubject: "fix: auth token validation",
				Branch:        "main",
				Org:           "entirehq",
				Repo:          "entire.io",
				Author:        "carol",
				CreatedAt:     "2026-03-22T09:00:00Z",
				Additions:     15,
				Deletions:     3,
				FilesChanged:  2,
			},
			Meta: search.Meta{MatchType: "keyword", Score: 0.4},
		},
		search.Result{
			Type: "session",
			Session: &search.SessionResult{
				SessionID:   "ss1",
				DisplayName: "Debug auth flow",
				Org:         "entirehq",
				Repo:        "entire.io",
				CreatedAt:   "2026-03-23T11:00:00Z",
				StepCount:   8,
			},
			Meta: search.Meta{MatchType: "semantic", Score: 0.3},
		},
	)
	return results
}

func testModel() searchModel {
	ss := statusStyles{colorEnabled: false, width: 100}
	cfg := search.Config{ServiceURL: "http://test", Owner: "o", Repo: "r", Limit: 20}
	m := newSearchModel(testResults(), "auth", 2, cfg, ss)
	return initTestViewport(m)
}

func testMultiTypeModel() searchModel {
	ss := statusStyles{colorEnabled: false, width: 120}
	cfg := search.Config{ServiceURL: "http://test", Owner: "o", Repo: "r", Limit: 20}
	results := testMultiTypeResults()
	m := newSearchModel(results, "auth", len(results), cfg, ss)
	return initTestViewport(m)
}

// initTestViewport sets a simulated terminal height and initializes the browse viewport for tests that call View().
func initTestViewport(m searchModel) searchModel {
	m.height = 60
	m.browseVP.SetHeight(59)
	m = m.refreshBrowseContent()
	return m
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

	m = updateModel(t, m, tea.KeyPressMsg{Code: tea.KeyDown})
	if m.cursor != 1 {
		t.Errorf("after down: cursor = %d, want 1", m.cursor)
	}

	m = updateModel(t, m, tea.KeyPressMsg{Code: tea.KeyDown})
	if m.cursor != 1 {
		t.Errorf("after down at bottom: cursor = %d, want 1", m.cursor)
	}

	m = updateModel(t, m, tea.KeyPressMsg{Code: tea.KeyUp})
	if m.cursor != 0 {
		t.Errorf("after up: cursor = %d, want 0", m.cursor)
	}

	m = updateModel(t, m, tea.KeyPressMsg{Code: tea.KeyUp})
	if m.cursor != 0 {
		t.Errorf("after up at top: cursor = %d, want 0", m.cursor)
	}
}

func TestSearchModel_VimNavigationAliases(t *testing.T) {
	t.Parallel()
	m := testModel()

	m = updateModel(t, m, tea.KeyPressMsg{Code: 'j', Text: "j"})
	if m.cursor != 1 {
		t.Errorf("after j: cursor = %d, want 1", m.cursor)
	}

	m = updateModel(t, m, tea.KeyPressMsg{Code: 'k', Text: "k"})
	if m.cursor != 0 {
		t.Errorf("after k: cursor = %d, want 0", m.cursor)
	}
}

func TestSearchModel_TopBottomNavigation(t *testing.T) {
	t.Parallel()

	results := make([]search.Result, 30)
	for i := range results {
		results[i] = search.Result{Type: "checkpoint", Checkpoint: &search.CheckpointResult{ID: fmt.Sprintf("id-%02d", i)}}
	}

	tests := []struct {
		name        string
		key         tea.KeyPressMsg
		startPage   int
		startCursor int
		wantPage    int
		wantCursor  int
	}{
		{
			name:        "home",
			key:         tea.KeyPressMsg{Code: tea.KeyHome},
			startPage:   1,
			startCursor: 4,
			wantPage:    0,
			wantCursor:  0,
		},
		{
			name:        "g",
			key:         tea.KeyPressMsg{Code: 'g', Text: "g"},
			startPage:   1,
			startCursor: 4,
			wantPage:    0,
			wantCursor:  0,
		},
		{
			name:        "end",
			key:         tea.KeyPressMsg{Code: tea.KeyEnd},
			startPage:   0,
			startCursor: 0,
			wantPage:    2,
			wantCursor:  9,
		},
		{
			name:        "G",
			key:         tea.KeyPressMsg{Code: 'G', Text: "G"},
			startPage:   0,
			startCursor: 0,
			wantPage:    2,
			wantCursor:  9,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			ss := statusStyles{colorEnabled: false, width: 100}
			cfg := search.Config{}
			m := initTestViewport(newSearchModel(results, "q", len(results), cfg, ss))
			m.page = tt.startPage
			m.cursor = tt.startCursor
			m = m.refreshBrowseContent()

			m = updateModel(t, m, tt.key)
			if m.page != tt.wantPage {
				t.Errorf("page = %d, want %d", m.page, tt.wantPage)
			}
			if m.cursor != tt.wantCursor {
				t.Errorf("cursor = %d, want %d", m.cursor, tt.wantCursor)
			}
		})
	}
}

func TestSearchModel_Quit(t *testing.T) {
	t.Parallel()
	m := testModel()

	quitKeys := []tea.KeyPressMsg{
		{Code: 'q', Text: "q"},
		{Code: 'c', Mod: tea.ModCtrl},
		{Code: tea.KeyEscape},
		{Code: 'h', Text: "h"},
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

	m = updateModel(t, m, tea.KeyPressMsg{Code: '/', Text: "/"})
	if m.mode != modeSearch {
		t.Errorf("after /: mode = %d, want modeSearch", m.mode)
	}

	m = updateModel(t, m, tea.KeyPressMsg{Code: tea.KeyEscape})
	if m.mode != modeBrowse {
		t.Errorf("after esc: mode = %d, want modeBrowse", m.mode)
	}
}

func TestSearchModel_SearchModeEnter(t *testing.T) {
	t.Parallel()
	m := testModel()

	// Enter search mode
	m = updateModel(t, m, tea.KeyPressMsg{Code: '/', Text: "/"})
	// Type a query
	m.input.SetValue(newQuery)

	// Press enter — should set loading and return to browse mode
	updated, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
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
	m = updateModel(t, m, tea.KeyPressMsg{Code: '/', Text: "/"})
	m.input.SetValue("   ")

	// Press enter — should be a no-op (stay in search mode)
	m = updateModel(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if m.mode != modeSearch {
		t.Errorf("after enter with empty query: mode = %d, want modeSearch", m.mode)
	}
	if m.loading {
		t.Error("after enter with empty query: loading should be false")
	}
}

// TestSearchModel_BrowseNeverExceedsHeight guards the master-detail layout:
// the browse view (header + scrolling list + pinned detail card + footer) must
// never render more rows than the terminal height, or the detail card gets
// clipped at the bottom (the bug this layout fixes).
func TestSearchModel_BrowseNeverExceedsHeight(t *testing.T) {
	t.Parallel()

	results := make([]search.Result, 10)
	for i := range results {
		results[i] = search.Result{Type: "checkpoint", Checkpoint: &search.CheckpointResult{
			ID:           fmt.Sprintf("id-%02d", i),
			Prompt:       "a deliberately long prompt that wraps across the detail card width several times over",
			Branch:       "feature/a-fairly-long-branch-name",
			Org:          "entireio",
			Repo:         "cli",
			Author:       "toothbrush",
			CreatedAt:    "2026-06-03T10:00:00Z",
			FilesTouched: []string{"cmd/entire/cli/auth.go", "cmd/entire/cli/some/deeply/nested/long/path/file.go"},
		}}
	}

	for _, color := range []bool{false, true} {
		for _, w := range []int{40, 80, 120} {
			for _, h := range []int{12, 20, 24, 40, 60} {
				ss := statusStyles{colorEnabled: color, width: w}
				m := initTestViewport(newSearchModel(results, "auth", 47, search.Config{}, ss))
				m.width, m.height = w, h
				m.cursor = 7 // force the list to scroll
				m = m.refreshBrowseContent()

				if got := lipgloss.Height(m.viewBrowse()); got > h {
					t.Errorf("color=%v w=%d h=%d: rendered %d rows, exceeds height", color, w, h, got)
				}
			}
		}
	}
}

// TestSearchModel_ListScrollHint verifies the reserved gap row shows a "more
// results" affordance when the list is cut off, and stays blank when it fits.
func TestSearchModel_ListScrollHint(t *testing.T) {
	t.Parallel()

	mk := func(n int) []search.Result {
		r := make([]search.Result, n)
		for i := range r {
			r[i] = search.Result{Type: "checkpoint", Checkpoint: &search.CheckpointResult{
				ID: fmt.Sprintf("id-%02d", i), Prompt: "p", Branch: "main", Org: "o", Repo: "r",
				Author: "a", CreatedAt: "2026-06-03T10:00:00Z",
			}}
		}
		return r
	}

	// Short terminal + 25 results (multiple pages) → the page's rows can't all fit.
	overflow := newSearchModel(mk(25), "auth", 25, search.Config{}, statusStyles{width: 80})
	overflow.height, overflow.width = 28, 80
	overflow = overflow.refreshBrowseContent()

	overflow.cursor = 0
	if v := overflow.viewBrowse(); !strings.Contains(v, "↓ more results") {
		t.Error("expected a down-scroll hint at the top of an overflowing list")
	}
	// The page / results count renders on the same status row beneath the list.
	if v := overflow.viewBrowse(); !strings.Contains(v, "page 1/3") || !strings.Contains(v, "25 results") {
		t.Errorf("expected page/results count in the list status row: %q", v)
	}
	overflow.cursor = 9
	overflow = overflow.refreshBrowseContent()
	if v := overflow.viewBrowse(); !strings.Contains(v, "↑ more results") {
		t.Error("expected an up-scroll hint at the bottom of an overflowing list")
	}

	// Tall terminal + few results → everything fits, no hint.
	fits := newSearchModel(mk(3), "auth", 3, search.Config{}, statusStyles{width: 80})
	fits.height, fits.width = 50, 80
	fits = fits.refreshBrowseContent()
	if v := fits.viewBrowse(); strings.Contains(v, "more results") {
		t.Error("did not expect a scroll hint when the whole list fits")
	}
}

func TestSearchModel_View(t *testing.T) {
	t.Parallel()
	m := testModel()
	view := m.View().Content

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

	// List meta line shows the result's type word
	if !strings.Contains(view, "checkpoint") {
		t.Error("view missing checkpoint type word on meta line")
	}

	// Selected result's full ID is shown in the detail card
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
	if !strings.Contains(view, "alicecodes (alice)") {
		t.Error("detail missing username")
	}
	if !strings.Contains(view, "semantic") {
		t.Error("detail missing match type")
	}
	// Files may be truncated in the inline card — check for "enter for more" hint
	if !strings.Contains(view, "src/middleware/auth.go") && !strings.Contains(view, "enter for more") {
		t.Error("detail missing files or truncation hint")
	}

	if !strings.Contains(view, "2 results") {
		t.Error("view missing results count in footer")
	}
}

func TestSearchModel_ViewMultiTypes(t *testing.T) {
	t.Parallel()
	m := testMultiTypeModel()
	// Switch to the All tab so every result type renders in the list.
	m.filterType = typeFilterAll
	m = m.refreshBrowseContent()
	view := m.View().Content

	// List meta lines show each result's type word
	if !strings.Contains(view, "checkpoint") {
		t.Error("view missing checkpoint type word")
	}
	if !strings.Contains(view, "commit") {
		t.Error("view missing commit type word")
	}
	if !strings.Contains(view, "session") {
		t.Error("view missing session type word")
	}

	// Type tabs
	if !strings.Contains(view, "Checkpoints") {
		t.Error("view missing Checkpoints tab")
	}
	if !strings.Contains(view, "Sessions") {
		t.Error("view missing Sessions tab")
	}
	if !strings.Contains(view, "Commits") {
		t.Error("view missing Commits tab")
	}
}

func TestSearchModel_TypeFilterKeys(t *testing.T) {
	t.Parallel()
	m := testMultiTypeModel()

	// Press 1 → filter to checkpoints
	m = updateModel(t, m, tea.KeyPressMsg{Code: '1', Text: "1"})
	if m.filterType != typeFilterCheckpoints {
		t.Errorf("after 1: filterType = %q, want %q", m.filterType, typeFilterCheckpoints)
	}
	if len(m.filteredResults()) != 2 {
		t.Errorf("checkpoint filter: got %d results, want 2", len(m.filteredResults()))
	}

	// Press 2 → filter to sessions
	m = updateModel(t, m, tea.KeyPressMsg{Code: '2', Text: "2"})
	if m.filterType != typeFilterSessions {
		t.Errorf("after 2: filterType = %q, want %q", m.filterType, typeFilterSessions)
	}
	if len(m.filteredResults()) != 1 {
		t.Errorf("session filter: got %d results, want 1", len(m.filteredResults()))
	}

	// Press 3 → filter to commits
	m = updateModel(t, m, tea.KeyPressMsg{Code: '3', Text: "3"})
	if m.filterType != typeFilterCommits {
		t.Errorf("after 3: filterType = %q, want %q", m.filterType, typeFilterCommits)
	}
	if len(m.filteredResults()) != 1 {
		t.Errorf("commit filter: got %d results, want 1", len(m.filteredResults()))
	}

	// Press 0 → no-op (the All tab was removed); filter stays on commits
	m = updateModel(t, m, tea.KeyPressMsg{Code: '0', Text: "0"})
	if m.filterType != typeFilterCommits {
		t.Errorf("after 0: filterType = %q, want %q (no-op)", m.filterType, typeFilterCommits)
	}
}

func TestSearchModel_TypeFilterResetsCursorAndPage(t *testing.T) {
	t.Parallel()
	m := testMultiTypeModel()
	m.cursor = 2
	m.page = 1

	m = updateModel(t, m, tea.KeyPressMsg{Code: '1', Text: "1"})
	if m.cursor != 0 {
		t.Errorf("cursor should reset to 0 on type change, got %d", m.cursor)
	}
	if m.page != 0 {
		t.Errorf("page should reset to 0 on type change, got %d", m.page)
	}
}

func TestSearchModel_CommitDetail(t *testing.T) {
	t.Parallel()
	m := testMultiTypeModel()

	// Filter to commits and check detail
	m.filterType = typeFilterCommits
	m.cursor = 0
	m = m.refreshBrowseContent()

	r := m.selectedResult()
	if r == nil {
		t.Fatal("no selected result")
	}
	if r.Type != "commit" {
		t.Fatalf("selected result type = %q, want commit", r.Type)
	}

	content := m.renderDetailContent(*r, 80, true)
	if !strings.Contains(content, "Commit Detail") {
		t.Error("missing Commit Detail title")
	}
	if !strings.Contains(content, "abc1234") {
		t.Error("missing truncated SHA")
	}
	if !strings.Contains(content, "fix: auth token validation") {
		t.Error("missing commit subject")
	}
	if !strings.Contains(content, "+15") {
		t.Error("missing additions")
	}
	if !strings.Contains(content, "-3") {
		t.Error("missing deletions")
	}
}

func TestSearchModel_SessionDetail(t *testing.T) {
	t.Parallel()
	m := testMultiTypeModel()

	// Filter to sessions and check detail
	m.filterType = typeFilterSessions
	m.cursor = 0
	m = m.refreshBrowseContent()

	r := m.selectedResult()
	if r == nil {
		t.Fatal("no selected result")
	}
	if r.Type != "session" {
		t.Fatalf("selected result type = %q, want session", r.Type)
	}

	content := m.renderDetailContent(*r, 80, true)
	if !strings.Contains(content, "Session Detail") {
		t.Error("missing Session Detail title")
	}
	if !strings.Contains(content, "ss1") {
		t.Error("missing session ID")
	}
	if !strings.Contains(content, "Debug auth flow") {
		t.Error("missing display name")
	}
	if !strings.Contains(content, "8") {
		t.Error("missing step count")
	}
}

func TestSearchModel_BrowseFooterHelp(t *testing.T) {
	t.Parallel()
	m := testModel()

	footer := m.viewHelp()
	wantParts := []string{
		"/ search",
		"↑/↓, j/k scroll",
		"home/end, g/G top/bottom",
		"1-3 type",
		"q quit",
	}
	lastIndex := -1
	for _, want := range wantParts {
		idx := strings.Index(footer, want)
		if idx == -1 {
			t.Fatalf("footer missing %q: %q", want, footer)
		}
		if idx < lastIndex {
			t.Fatalf("footer control %q rendered out of order: %q", want, footer)
		}
		lastIndex = idx
	}

	for _, unwanted := range []string{"detail", "open", "back", "navigate"} {
		if strings.Contains(footer, unwanted) {
			t.Fatalf("footer should not mention %q: %q", unwanted, footer)
		}
	}
	if strings.Contains(footer, "n/p page") {
		t.Fatalf("single-page footer should not mention paging: %q", footer)
	}
}

func TestSearchModel_BrowseFooterHelpIncludesPagingForMultiplePages(t *testing.T) {
	t.Parallel()

	results := make([]search.Result, 30)
	for i := range results {
		results[i] = search.Result{Type: "checkpoint", Checkpoint: &search.CheckpointResult{ID: fmt.Sprintf("id-%02d", i)}}
	}

	ss := statusStyles{colorEnabled: false, width: 120}
	m := newSearchModel(results, "q", len(results), search.Config{}, ss)

	footer := m.viewHelp()
	wantParts := []string{
		"/ search",
		"↑/↓, j/k scroll",
		"home/end, g/G top/bottom",
		"n/p page",
		"1-3 type",
		"q quit",
	}
	lastIndex := -1
	for _, want := range wantParts {
		idx := strings.Index(footer, want)
		if idx == -1 {
			t.Fatalf("footer missing %q: %q", want, footer)
		}
		if idx < lastIndex {
			t.Fatalf("footer control %q rendered out of order: %q", want, footer)
		}
		lastIndex = idx
	}
}

func TestSearchModel_ViewSearchModeIncludesRepoHint(t *testing.T) {
	t.Parallel()

	m := testModel()
	m.mode = modeSearch
	m.input.Focus()

	view := m.View().Content
	if !strings.Contains(view, "repo:<owner/name|*>") {
		t.Error("view missing repo filter hint")
	}
	if !strings.Contains(view, "repo:* searches all accessible repos") {
		t.Error("view missing repo:* note")
	}
}

func TestSearchModel_ViewNoResults(t *testing.T) {
	t.Parallel()
	ss := statusStyles{colorEnabled: false, width: 80}
	cfg := search.Config{}
	m := initTestViewport(newSearchModel(nil, "nothing", 0, cfg, ss))
	view := m.View().Content

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

	if view := m.View().Content; view != "" {
		t.Errorf("view with zero width should be empty, got %q", view)
	}
}

func TestSearchModel_ViewNarrowWidth(t *testing.T) {
	t.Parallel()
	ss := statusStyles{colorEnabled: false, width: 1}
	cfg := search.Config{}
	m := newSearchModel(testResults(), "auth", 2, cfg, ss)
	m.width = 1

	// Should not panic on width=1 (contentWidth would be negative without guard)
	_ = m.View()
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

func TestRenderDetailContent_Sections(t *testing.T) {
	t.Parallel()
	m := testModel()
	r := testResults()[0]

	withSections := m.renderDetailContent(r, 80, true)
	if !strings.Contains(withSections, "OVERVIEW") {
		t.Error("showSections=true should contain OVERVIEW header")
	}
	if !strings.Contains(withSections, "SOURCE") {
		t.Error("showSections=true should contain SOURCE header")
	}
	if !strings.Contains(withSections, "FILES") {
		t.Error("showSections=true should contain FILES header")
	}

	withoutSections := m.renderDetailContent(r, 80, false)
	if strings.Contains(withoutSections, "OVERVIEW") {
		t.Error("showSections=false should not contain OVERVIEW header")
	}
	if strings.Contains(withoutSections, "SOURCE") {
		t.Error("showSections=false should not contain SOURCE header")
	}
	if !strings.Contains(withoutSections, "Files:") {
		t.Error("showSections=false should contain Files: label")
	}
}

func TestRenderDetailContent_AuthorEmptyUsername(t *testing.T) {
	t.Parallel()
	m := testModel()
	r := testResults()[1] // bob, no AuthorUsername
	content := m.renderDetailContent(r, 80, false)
	if !strings.Contains(content, "bob") {
		t.Error("author should show display name when username is nil")
	}

	// Empty string username should fall back to display name
	empty := ""
	r.Checkpoint.AuthorUsername = &empty
	content = m.renderDetailContent(r, 80, false)
	if !strings.Contains(content, "bob") {
		t.Error("author should show display name when username is empty string")
	}
}

func TestRenderDetailContent_PromptWrapping(t *testing.T) {
	t.Parallel()
	m := testModel()
	r := testResults()[0]
	r.Checkpoint.Prompt = "line one\nline two\nline three"

	content := m.renderDetailContent(r, 80, false)
	// CollapseWhitespace should merge the newlines into spaces
	if strings.Contains(content, "line one\n") {
		t.Error("prompt should have newlines collapsed")
	}
	if !strings.Contains(content, "line one line two line three") {
		t.Error("prompt should be collapsed to single line")
	}
}

func TestRenderSearchStatic(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	styles := statusStyles{colorEnabled: false, width: 120}
	results := testMultiTypeResults()
	renderSearchStatic(&buf, results, "auth", len(results), styles)
	output := buf.String()

	if !strings.Contains(output, `Found 4 results matching "auth"`) {
		t.Errorf("static output missing header, got:\n%s", output)
	}
	if !strings.Contains(output, "TYPE") {
		t.Error("static output missing TYPE header")
	}
	if !strings.Contains(output, "REPO") {
		t.Error("static output missing REPO header")
	}
	if !strings.Contains(output, "CP") {
		t.Error("static output missing CP badge")
	}
	if !strings.Contains(output, "CM") {
		t.Error("static output missing CM badge")
	}
	if !strings.Contains(output, "SS") {
		t.Error("static output missing SS badge")
	}
}

func TestSearchModel_PageResults(t *testing.T) {
	t.Parallel()

	// With 2 results and 25 per page, everything fits on page 0
	m := testModel()
	page := m.pageResults()
	if len(page) != 2 {
		t.Errorf("pageResults() = %d items, want 2", len(page))
	}

	// Out-of-range page returns nil
	m.page = 5
	if got := m.pageResults(); got != nil {
		t.Errorf("pageResults() on out-of-range page = %v, want nil", got)
	}
}

func TestSearchModel_TotalPages(t *testing.T) {
	t.Parallel()

	// 2 results, total=2 → 1 page
	m := testModel()
	if got := m.totalPages(); got != 1 {
		t.Errorf("totalPages() with total=2 = %d, want 1", got)
	}

	// 0 results = 1 page (empty state)
	ss := statusStyles{colorEnabled: false, width: 100}
	cfg := search.Config{}
	empty := newSearchModel(nil, "", 0, cfg, ss)
	if got := empty.totalPages(); got != 1 {
		t.Errorf("totalPages() with total=0 = %d, want 1", got)
	}

	// 26 loaded results, total=26 → 2 pages
	results := make([]search.Result, 26)
	for i := range results {
		results[i] = search.Result{Type: "checkpoint", Checkpoint: &search.CheckpointResult{ID: fmt.Sprintf("id-%02d", i)}}
	}
	many := newSearchModel(results, "q", 26, cfg, ss)
	if got := many.totalPages(); got != 3 {
		t.Errorf("totalPages() with total=26 = %d, want 3", got)
	}
}

func TestSearchModel_TotalPagesUsesFilteredCount(t *testing.T) {
	t.Parallel()

	m := testMultiTypeModel()

	// Unfiltered: 4 results → 1 page
	if got := m.totalPages(); got != 1 {
		t.Errorf("unfiltered totalPages = %d, want 1", got)
	}

	// Filter to checkpoints: 2 results → 1 page
	m.filterType = typeFilterCheckpoints
	if got := m.totalPages(); got != 1 {
		t.Errorf("checkpoint-filtered totalPages = %d, want 1", got)
	}
}

func TestSearchModel_AppendResults(t *testing.T) {
	t.Parallel()

	ss := statusStyles{colorEnabled: false, width: 100}
	cfg := search.Config{}
	results := make([]search.Result, 25)
	for i := range results {
		results[i] = search.Result{Type: "checkpoint", Checkpoint: &search.CheckpointResult{ID: fmt.Sprintf("id-%02d", i)}}
	}
	m := newSearchModel(results, "q", 50, cfg, ss)

	if m.apiPage != 1 {
		t.Fatalf("initial apiPage = %d, want 1", m.apiPage)
	}

	// Simulate receiving more results
	newResults := make([]search.Result, 25)
	for i := range newResults {
		newResults[i] = search.Result{Type: "checkpoint", Checkpoint: &search.CheckpointResult{ID: fmt.Sprintf("new-%02d", i)}}
	}
	m = updateModel(t, m, searchMoreResultsMsg{results: newResults})

	if len(m.results) != 50 {
		t.Errorf("after append: len(results) = %d, want 50", len(m.results))
	}
	if m.apiPage != 2 {
		t.Errorf("after append: apiPage = %d, want 2", m.apiPage)
	}
	if m.fetchingMore {
		t.Error("fetchingMore should be false after append")
	}
}

func TestSearchModel_FetchMoreOnNavigate(t *testing.T) {
	t.Parallel()

	// 10 loaded results, total=50 → multiple display pages but only 1 page loaded
	ss := statusStyles{colorEnabled: false, width: 100}
	cfg := search.Config{ServiceURL: "http://test", Owner: "o", Repo: "r", Limit: 10}
	results := make([]search.Result, 10)
	for i := range results {
		results[i] = search.Result{Type: "checkpoint", Checkpoint: &search.CheckpointResult{ID: fmt.Sprintf("id-%02d", i)}}
	}
	m := newSearchModel(results, "q", 50, cfg, ss)
	m.filterType = typeFilterAll // fetch-more from the API applies in the All view

	// Navigate to page 2 — should trigger fetch
	updated, cmd := m.Update(tea.KeyPressMsg{Code: 'n', Text: "n"})
	m, ok := updated.(searchModel)
	if !ok {
		t.Fatalf("Update returned %T, want searchModel", updated)
	}

	if m.page != 1 {
		t.Errorf("page = %d, want 1", m.page)
	}
	if !m.fetchingMore {
		t.Error("fetchingMore should be true when navigating past loaded results")
	}
	if cmd == nil {
		t.Error("expected a fetch command")
	}
}

func TestSearchModel_NoFetchWhenResultsLoaded(t *testing.T) {
	t.Parallel()

	// 50 loaded results, total=50 → 2 pages, all loaded
	ss := statusStyles{colorEnabled: false, width: 100}
	cfg := search.Config{ServiceURL: "http://test", Owner: "o", Repo: "r", Limit: 25}
	results := make([]search.Result, 50)
	for i := range results {
		results[i] = search.Result{Type: "checkpoint", Checkpoint: &search.CheckpointResult{ID: fmt.Sprintf("id-%02d", i)}}
	}
	m := newSearchModel(results, "q", 50, cfg, ss)

	// Navigate to page 2 — should NOT trigger fetch (data already loaded)
	updated, cmd := m.Update(tea.KeyPressMsg{Code: 'n', Text: "n"})
	m, ok := updated.(searchModel)
	if !ok {
		t.Fatalf("Update returned %T, want searchModel", updated)
	}

	if m.page != 1 {
		t.Errorf("page = %d, want 1", m.page)
	}
	if m.fetchingMore {
		t.Error("fetchingMore should be false when results are already loaded")
	}
	if cmd != nil {
		t.Error("expected no command when results are loaded")
	}
}

func TestSearchModel_NewSearchResetsApiPage(t *testing.T) {
	t.Parallel()

	m := testModel()
	m.apiPage = 3
	m.fetchingMore = true

	// Simulate receiving fresh search results
	m = updateModel(t, m, searchResultsMsg{results: testResults()[:1], total: 1})

	if m.apiPage != 1 {
		t.Errorf("apiPage after new search = %d, want 1", m.apiPage)
	}
	if m.fetchingMore {
		t.Error("fetchingMore should be false after new search")
	}
}

func TestSearchModel_SelectedResult(t *testing.T) {
	t.Parallel()

	m := testModel()
	r := m.selectedResult()
	if r == nil {
		t.Fatal("selectedResult() = nil, want first result")
		return
	}
	if r.Checkpoint == nil || r.Checkpoint.ID != "a3b2c4d5e6f7" {
		t.Errorf("selectedResult().Checkpoint.ID = %q, want %q", r.ResultID(), "a3b2c4d5e6f7")
	}

	// Move cursor to second result
	m.cursor = 1
	r = m.selectedResult()
	if r == nil {
		t.Fatal("selectedResult() at cursor 1 = nil")
		return
	}
	if r.Checkpoint == nil || r.Checkpoint.ID != "d5e6f789ab01" {
		t.Errorf("selectedResult().Checkpoint.ID = %q, want %q", r.ResultID(), "d5e6f789ab01")
	}

	// Out-of-range cursor returns nil
	m.cursor = 99
	if got := m.selectedResult(); got != nil {
		t.Errorf("selectedResult() at cursor 99 = %v, want nil", got)
	}
}

func TestSearchModel_PageNavigation(t *testing.T) {
	t.Parallel()

	// Create model with 20 results (2 pages at 10/page)
	ss := statusStyles{colorEnabled: false, width: 100}
	cfg := search.Config{ServiceURL: "http://test", Owner: "o", Repo: "r"}
	results := make([]search.Result, 20)
	for i := range results {
		results[i] = search.Result{Type: "checkpoint", Checkpoint: &search.CheckpointResult{ID: fmt.Sprintf("id-%02d", i)}}
	}
	m := newSearchModel(results, "q", 20, cfg, ss)

	if m.page != 0 {
		t.Fatalf("initial page = %d, want 0", m.page)
	}

	// Navigate to next page
	m = updateModel(t, m, tea.KeyPressMsg{Code: 'n', Text: "n"})
	if m.page != 1 {
		t.Errorf("after 'n': page = %d, want 1", m.page)
	}
	if m.cursor != 0 {
		t.Errorf("after 'n': cursor = %d, want 0 (reset)", m.cursor)
	}

	// Can't go past last page
	m = updateModel(t, m, tea.KeyPressMsg{Code: 'n', Text: "n"})
	if m.page != 1 {
		t.Errorf("after 'n' on last page: page = %d, want 1", m.page)
	}

	// Navigate back
	m = updateModel(t, m, tea.KeyPressMsg{Code: 'p', Text: "p"})
	if m.page != 0 {
		t.Errorf("after 'p': page = %d, want 0", m.page)
	}

	// Can't go before first page
	m = updateModel(t, m, tea.KeyPressMsg{Code: 'p', Text: "p"})
	if m.page != 0 {
		t.Errorf("after 'p' on first page: page = %d, want 0", m.page)
	}
}

func TestSearchModel_NewSearchClearsFilters(t *testing.T) {
	t.Parallel()

	// Create model with startup filters
	ss := statusStyles{colorEnabled: false, width: 100}
	cfg := search.Config{
		ServiceURL: "http://test", Owner: "o", Repo: "r", Limit: 25,
		Author: "alice", Date: "week",
	}
	m := newSearchModel(testResults(), "auth", 2, cfg, ss)

	// Enter search mode
	m = updateModel(t, m, tea.KeyPressMsg{Code: '/', Text: "/"})

	// Type a query without filters
	m.input.SetValue(newQuery)

	// Press enter — should trigger search with cleared filters
	updated, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	m, ok := updated.(searchModel)
	if !ok {
		t.Fatalf("Update returned %T, want searchModel", updated)
	}

	if !m.loading {
		t.Fatal("expected loading to be true")
	}
	if cmd == nil {
		t.Fatal("expected a search command")
	}

	// searchCfg should be updated with the new query and cleared filters,
	// so that fetchMoreResults uses the correct config for page 2+.
	if m.searchCfg.Author != "" {
		t.Errorf("searchCfg.Author should be cleared, got %q", m.searchCfg.Author)
	}
	if m.searchCfg.Date != "" {
		t.Errorf("searchCfg.Date should be cleared, got %q", m.searchCfg.Date)
	}
	if got := m.searchCfg.Repos; len(got) != 0 {
		t.Errorf("searchCfg.Repos should be cleared, got %v", got)
	}
	if m.searchCfg.Query != newQuery {
		t.Errorf("searchCfg.Query = %q, want %q", m.searchCfg.Query, newQuery)
	}
}

func TestSearchModel_FetchMoreError(t *testing.T) {
	t.Parallel()

	ss := statusStyles{colorEnabled: false, width: 100}
	cfg := search.Config{}
	results := make([]search.Result, 25)
	for i := range results {
		results[i] = search.Result{Type: "checkpoint", Checkpoint: &search.CheckpointResult{ID: fmt.Sprintf("id-%02d", i)}}
	}
	m := newSearchModel(results, "q", 50, cfg, ss)
	m.fetchingMore = true

	m = updateModel(t, m, searchMoreResultsMsg{err: errTestSearch})

	if m.fetchingMore {
		t.Error("fetchingMore should be false after error")
	}
	if m.searchErr == "" {
		t.Error("searchErr should be set after fetch-more error")
	}
	if len(m.results) != 25 {
		t.Errorf("results should be unchanged, got %d", len(m.results))
	}
}

func TestSearchModel_FetchMoreEmpty_CapsTotal(t *testing.T) {
	t.Parallel()

	ss := statusStyles{colorEnabled: false, width: 100}
	cfg := search.Config{}
	results := make([]search.Result, 10)
	for i := range results {
		results[i] = search.Result{Type: "checkpoint", Checkpoint: &search.CheckpointResult{ID: fmt.Sprintf("id-%02d", i)}}
	}
	m := newSearchModel(results, "q", 100, cfg, ss)
	m.filterType = typeFilterAll // exercise all-types pagination against m.total

	if m.totalPages() != 10 {
		t.Fatalf("initial totalPages = %d, want 10", m.totalPages())
	}

	// Simulate API returning empty results (exhausted)
	m = updateModel(t, m, searchMoreResultsMsg{results: nil})

	if m.total != 10 {
		t.Errorf("total should be capped to loaded results (10), got %d", m.total)
	}
	if m.totalPages() != 1 {
		t.Errorf("totalPages should be 1 after cap, got %d", m.totalPages())
	}
}

func TestSearchModel_ViewFetchingMore(t *testing.T) {
	t.Parallel()

	// Model with 10 loaded results but on page 2 (no data) while fetching
	ss := statusStyles{colorEnabled: false, width: 100}
	cfg := search.Config{}
	results := make([]search.Result, 10)
	for i := range results {
		results[i] = search.Result{Type: "checkpoint", Checkpoint: &search.CheckpointResult{ID: fmt.Sprintf("id-%02d", i)}}
	}
	m := initTestViewport(newSearchModel(results, "q", 50, cfg, ss))
	m.page = 1
	m.fetchingMore = true
	m = m.refreshBrowseContent()

	view := m.View().Content
	if !strings.Contains(view, "Loading more results...") {
		t.Error("view should show loading message when fetchingMore and page has no data")
	}
}

func TestSearchModel_NewSearchPersistsFilters(t *testing.T) {
	t.Parallel()

	ss := statusStyles{colorEnabled: false, width: 100}
	cfg := search.Config{ServiceURL: "http://test", Owner: "o", Repo: "r", Limit: 25}
	m := newSearchModel(testResults(), "old", 2, cfg, ss)

	// Enter search mode and type query with filters
	m = updateModel(t, m, tea.KeyPressMsg{Code: '/', Text: "/"})
	m.input.SetValue(newQuery + " author:bob date:month")

	updated, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	m, ok := updated.(searchModel)
	if !ok {
		t.Fatalf("Update returned %T, want searchModel", updated)
	}

	if m.searchCfg.Query != newQuery {
		t.Errorf("searchCfg.Query = %q, want %q", m.searchCfg.Query, newQuery)
	}
	if m.searchCfg.Author != "bob" {
		t.Errorf("searchCfg.Author = %q, want %q", m.searchCfg.Author, "bob")
	}
	if m.searchCfg.Date != "month" {
		t.Errorf("searchCfg.Date = %q, want %q", m.searchCfg.Date, "month")
	}
}

func TestSearchModel_NewSearchPersistsRepoFilters(t *testing.T) {
	t.Parallel()

	ss := statusStyles{colorEnabled: false, width: 100}
	cfg := search.Config{
		ServiceURL: "http://test",
		Owner:      "default-owner",
		Repo:       "default-repo",
		Limit:      25,
	}
	m := newSearchModel(testResults(), "old", 2, cfg, ss)

	m = updateModel(t, m, tea.KeyPressMsg{Code: '/', Text: "/"})
	m.input.SetValue(newQuery + " repo:entirehq/entire.io")

	updated, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	m, ok := updated.(searchModel)
	if !ok {
		t.Fatalf("Update returned %T, want searchModel", updated)
	}

	if m.searchCfg.Query != newQuery {
		t.Errorf("searchCfg.Query = %q, want %q", m.searchCfg.Query, newQuery)
	}
	if got := m.searchCfg.Repos; len(got) != 1 || got[0] != "entirehq/entire.io" {
		t.Errorf("searchCfg.Repos = %v, want %v", got, []string{"entirehq/entire.io"})
	}
}

func TestSearchModel_NewSearchClearsExplicitRepoFilters(t *testing.T) {
	t.Parallel()

	ss := statusStyles{colorEnabled: false, width: 100}
	cfg := search.Config{
		ServiceURL: "http://test",
		Owner:      "default-owner",
		Repo:       "default-repo",
		Limit:      25,
		Repos:      []string{"entirehq/entire.io"},
	}
	m := newSearchModel(testResults(), "auth", 2, cfg, ss)

	m = updateModel(t, m, tea.KeyPressMsg{Code: '/', Text: "/"})
	m.input.SetValue(newQuery)

	updated, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	m, ok := updated.(searchModel)
	if !ok {
		t.Fatalf("Update returned %T, want searchModel", updated)
	}

	if got := m.searchCfg.Repos; len(got) != 0 {
		t.Errorf("searchCfg.Repos = %v, want empty explicit repo overrides", got)
	}
	if m.searchCfg.Owner != "default-owner" || m.searchCfg.Repo != "default-repo" {
		t.Errorf("default repo scope changed unexpectedly: %s/%s", m.searchCfg.Owner, m.searchCfg.Repo)
	}
}

func TestSearchModel_NewSearchAllReposFilter(t *testing.T) {
	t.Parallel()

	ss := statusStyles{colorEnabled: false, width: 100}
	cfg := search.Config{
		ServiceURL: "http://test",
		Owner:      "default-owner",
		Repo:       "default-repo",
		Limit:      25,
	}
	m := newSearchModel(testResults(), "old", 2, cfg, ss)

	m = updateModel(t, m, tea.KeyPressMsg{Code: '/', Text: "/"})
	m.input.SetValue(newQuery + " repo:*")

	updated, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	m, ok := updated.(searchModel)
	if !ok {
		t.Fatalf("Update returned %T, want searchModel", updated)
	}

	if got := m.searchCfg.Repos; len(got) != 1 || got[0] != search.AllReposFilter {
		t.Errorf("searchCfg.Repos = %v, want %v", got, []string{search.AllReposFilter})
	}
}

func TestSearchModel_NewSearchRejectsMultipleExplicitRepos(t *testing.T) {
	t.Parallel()

	ss := statusStyles{colorEnabled: false, width: 100}
	cfg := search.Config{
		ServiceURL: "http://test",
		Owner:      "default-owner",
		Repo:       "default-repo",
		Limit:      25,
	}
	m := newSearchModel(testResults(), "old", 2, cfg, ss)

	m = updateModel(t, m, tea.KeyPressMsg{Code: '/', Text: "/"})
	m.input.SetValue(newQuery + " repo:entirehq/entire.io,entireio/cli")

	updated, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	m, ok := updated.(searchModel)
	if !ok {
		t.Fatalf("Update returned %T, want searchModel", updated)
	}

	if cmd != nil {
		t.Fatal("expected no search command on invalid multi-repo input")
	}
	if m.mode != modeSearch {
		t.Errorf("mode = %d, want modeSearch", m.mode)
	}
	if m.searchErr != "only one explicit repo filter is currently supported" {
		t.Errorf("searchErr = %q", m.searchErr)
	}
}

func TestSearchModel_ApiPageInitialization(t *testing.T) {
	t.Parallel()

	ss := statusStyles{colorEnabled: false, width: 100}
	cfg := search.Config{}

	// With results: apiPage = 1
	withResults := newSearchModel(testResults(), "q", 2, cfg, ss)
	if withResults.apiPage != 1 {
		t.Errorf("apiPage with results = %d, want 1", withResults.apiPage)
	}

	// Without results: apiPage = 0
	noResults := newSearchModel(nil, "", 0, cfg, ss)
	if noResults.apiPage != 0 {
		t.Errorf("apiPage without results = %d, want 0", noResults.apiPage)
	}
}

func TestComputeColumns(t *testing.T) {
	t.Parallel()

	cols := computeColumns(120)
	if cols.typeCol != 5 {
		t.Errorf("type width = %d, want 5", cols.typeCol)
	}
	if cols.age != 10 {
		t.Errorf("age width = %d, want 10", cols.age)
	}
	if cols.id != 12 {
		t.Errorf("id width = %d, want 12", cols.id)
	}
	if cols.repo < 10 {
		t.Errorf("repo width = %d, want >= 10", cols.repo)
	}
	if cols.author != 14 {
		t.Errorf("author width = %d, want 14", cols.author)
	}

	cols = computeColumns(40)
	if cols.branch < 8 {
		t.Errorf("branch width on narrow terminal = %d, want >= 8", cols.branch)
	}
	if cols.repo < 10 {
		t.Errorf("repo width on narrow terminal = %d, want >= 10", cols.repo)
	}
}

func TestSearchModel_ComputeTypeCounts(t *testing.T) {
	t.Parallel()

	m := testMultiTypeModel()
	cp, cm, ss := m.computeTypeCounts()
	if cp != 2 {
		t.Errorf("checkpoints = %d, want 2", cp)
	}
	if cm != 1 {
		t.Errorf("commits = %d, want 1", cm)
	}
	if ss != 1 {
		t.Errorf("sessions = %d, want 1", ss)
	}
}

func TestSearchModel_ComputeTypeCounts_UsesAPICounts(t *testing.T) {
	t.Parallel()

	m := testMultiTypeModel()
	m.counts = &search.TypeCounts{Checkpoints: 10, Commits: 5, Sessions: 3}
	cp, cm, ss := m.computeTypeCounts()
	if cp != 10 {
		t.Errorf("checkpoints = %d, want 10", cp)
	}
	if cm != 5 {
		t.Errorf("commits = %d, want 5", cm)
	}
	if ss != 3 {
		t.Errorf("sessions = %d, want 3", ss)
	}
}
