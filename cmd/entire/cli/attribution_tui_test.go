package cli

import (
	"bytes"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
)

func whyTUIFixture() *fileAttributionResult {
	when := time.Date(2026, 1, 2, 15, 4, 0, 0, time.UTC)
	lines := []attributionLine{
		{
			LineNumber: 1, Authorship: attributionHuman, Tag: "[HU]",
			CommitSHA: "1111111111111111111111111111111111111111", ShortCommitSHA: "1111111",
			Author: "Ada", AuthorTime: &when, Content: "package main",
		},
		{
			LineNumber: 2, Authorship: attributionAI, Tag: "[AI]",
			CommitSHA: "2222222222222222222222222222222222222222", ShortCommitSHA: "2222222",
			Author: "Ada", AuthorTime: &when,
			CheckpointID: "a1b2c3d4e5f6", SessionID: "session-agent-12345678",
			Agent: "Claude Code", Model: "claude-test",
			Prompt:  "Fix the authentication bug in login flow please",
			Intent:  "Fix auth bug",
			Content: "func login() {}",
		},
		{
			LineNumber: 3, Authorship: attributionMixed, Tag: "[MX]",
			CommitSHA: "3333333333333333333333333333333333333333", ShortCommitSHA: "3333333",
			CheckpointID: "b1b2c3d4e5f6", SessionID: "session-mixed-12345678",
			Agent: "Claude Code", Prompt: "Refactor helpers", PromptSessionLevel: true,
			Candidates: []attributionCandidate{
				{CheckpointID: "b1b2c3d4e5f6", Agent: "Claude Code", Prompt: "Refactor helpers"},
				{CheckpointID: "c1b2c3d4e5f6", Agent: "Codex", Prompt: "Tidy up"},
			},
			Content: "func helper() {}",
		},
		{
			LineNumber: 4, Authorship: attributionAI, Tag: "[AI]",
			CheckpointID: "d1b2c3d4e5f6", MetadataMissing: true,
			MetadataMissingReason: "checkpoint metadata was not found locally. Run: git fetch origin entire/checkpoints/v1:entire/checkpoints/v1.",
			Content:               "func missing() {}",
		},
	}
	return &fileAttributionResult{
		File:  "auth.py",
		Lines: lines,
		Summary: attributionSummary{
			TotalLines: 4, AILines: 2, HumanLines: 1, MixedLines: 1,
			AIPercentage: 50, HumanPercentage: 25, MixedPercentage: 25,
		},
	}
}

func updateWhyTUI(t *testing.T, m whyTUIModel, msg tea.Msg) whyTUIModel {
	t.Helper()
	next, _ := m.Update(msg)
	tm, ok := next.(whyTUIModel)
	if !ok {
		t.Fatalf("Update returned %T, want whyTUIModel", next)
	}
	return tm
}

// Color off keeps assertions on raw text; a tall window keeps the detail pane
// fully visible so assertions aren't tripped by viewport scrolling.
func newSizedWhyTUI(t *testing.T, result *fileAttributionResult, startLine int) whyTUIModel {
	t.Helper()
	m := newWhyTUIModel(result, "acme/app", false, startLine)
	return updateWhyTUI(t, m, tea.WindowSizeMsg{Width: 140, Height: 44})
}

func whyTUIViewText(t *testing.T, m whyTUIModel) string {
	t.Helper()
	return m.View().Content
}

func keyPress(k string) tea.KeyPressMsg {
	switch k {
	case "up":
		return tea.KeyPressMsg{Code: tea.KeyUp}
	case "down":
		return tea.KeyPressMsg{Code: tea.KeyDown}
	case "enter":
		return tea.KeyPressMsg{Code: tea.KeyEnter}
	default:
		r := []rune(k)[0]
		return tea.KeyPressMsg{Code: r, Text: k}
	}
}

func TestWhyTUIRendersFileAndSelectedLineDetail(t *testing.T) {
	t.Parallel()
	m := newSizedWhyTUI(t, whyTUIFixture(), 0)
	text := whyTUIViewText(t, m)

	for _, want := range []string{
		"why", "auth.py", "4 lines", "50% AI",
		"package main", "func login() {}", // list content
		"LINE 1", "[HU]", "Human: no agent checkpoint", // detail for initial cursor (line 1)
		whyMarkerLegend, // footer legend
		"quit",          // help line
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("TUI view missing %q:\n%s", want, text)
		}
	}
}

func TestWhyTUINavigationShowsPromptDetail(t *testing.T) {
	t.Parallel()
	m := newSizedWhyTUI(t, whyTUIFixture(), 0)
	m = updateWhyTUI(t, m, keyPress("down")) // to line 2 ([AI])
	text := whyTUIViewText(t, m)

	for _, want := range []string{
		"LINE 2", "[AI]", "fully agent-authored",
		"Agent: Claude Code · claude-test",
		"Session: session-", "Checkpoint: a1b2c3d4e5f6",
		"PROMPT", "Fix the authentication bug",
		"INTENT", "Fix auth bug",
		"Full context: entire checkpoint explain a1b2c3d4e5f6",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("after down, view missing %q:\n%s", want, text)
		}
	}
}

func TestWhyTUIStartLinePositionsCursor(t *testing.T) {
	t.Parallel()
	m := newSizedWhyTUI(t, whyTUIFixture(), 3)
	text := whyTUIViewText(t, m)

	for _, want := range []string{
		"LINE 3", "[MX]", "combined agent work with human edits",
		"SESSION PROMPT", "session-level prompt",
		"CANDIDATE CHECKPOINTS (2)", "c1b2c3d4e5f6", "Codex",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("start-line view missing %q:\n%s", want, text)
		}
	}
}

func TestWhyTUIJumpToNextAgentLine(t *testing.T) {
	t.Parallel()
	m := newSizedWhyTUI(t, whyTUIFixture(), 0) // cursor at line 1 [HU]
	m = updateWhyTUI(t, m, keyPress("n"))      // -> line 2 [AI]
	if got := m.selectedLine().LineNumber; got != 2 {
		t.Fatalf("n jumped to line %d, want 2", got)
	}
	m = updateWhyTUI(t, m, keyPress("n")) // -> line 3 [MX]
	m = updateWhyTUI(t, m, keyPress("n")) // -> line 4 [AI]
	if got := m.selectedLine().LineNumber; got != 4 {
		t.Fatalf("n n jumped to line %d, want 4", got)
	}
	// No further agent lines: cursor stays, status message appears.
	m = updateWhyTUI(t, m, keyPress("n"))
	if got := m.selectedLine().LineNumber; got != 4 {
		t.Fatalf("n at end moved to line %d, want 4", got)
	}
	if !strings.Contains(whyTUIViewText(t, m), "no more agent-attributed lines") {
		t.Fatal("expected status message about no more agent lines")
	}
	// p goes back.
	m = updateWhyTUI(t, m, keyPress("p"))
	if got := m.selectedLine().LineNumber; got != 3 {
		t.Fatalf("p jumped to line %d, want 3", got)
	}
}

func TestWhyTUIExpandTogglesFullPrompt(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("very long prompt text ", 30)
	result := whyTUIFixture()
	result.Lines[1].Prompt = long
	m := newSizedWhyTUI(t, result, 2)

	if !strings.Contains(whyTUIViewText(t, m), "enter to expand") {
		t.Fatal("collapsed view should hint at expansion")
	}
	m = updateWhyTUI(t, m, keyPress("enter"))
	if strings.Contains(whyTUIViewText(t, m), "enter to expand") {
		t.Fatal("expanded view should not truncate the prompt")
	}
}

func TestWhyTUIMissingMetadataShowsReason(t *testing.T) {
	t.Parallel()
	m := newSizedWhyTUI(t, whyTUIFixture(), 4)
	text := whyTUIViewText(t, m)
	if !strings.Contains(text, "checkpoint metadata was not found locally") {
		t.Fatalf("missing-metadata reason absent:\n%s", text)
	}
	if strings.Contains(text, "Full context: entire checkpoint explain d1b2c3d4e5f6") {
		t.Fatal("explain hint must be suppressed when metadata is missing")
	}
}

// g/G map to the shared keymap's Home/End bindings.
func TestWhyTUIHomeEndKeys(t *testing.T) {
	t.Parallel()
	m := newSizedWhyTUI(t, whyTUIFixture(), 3) // start mid-file
	m = updateWhyTUI(t, m, keyPress("g"))
	if got := m.selectedLine().LineNumber; got != 1 {
		t.Fatalf("g moved to line %d, want 1", got)
	}
	m = updateWhyTUI(t, m, keyPress("G"))
	if got := m.selectedLine().LineNumber; got != 4 {
		t.Fatalf("G moved to line %d, want 4", got)
	}
}

func TestWhyTUIQuitKeys(t *testing.T) {
	t.Parallel()
	m := newSizedWhyTUI(t, whyTUIFixture(), 0)
	for _, k := range []string{"q"} {
		_, cmd := m.Update(keyPress(k))
		if cmd == nil {
			t.Fatalf("key %q should quit", k)
		}
	}
}

func TestWhyTUIEmptyFileDoesNotPanic(t *testing.T) {
	t.Parallel()
	empty := &fileAttributionResult{File: "empty.py", Lines: nil}
	m := newWhyTUIModel(empty, "", false, 0)
	m = updateWhyTUI(t, m, tea.WindowSizeMsg{Width: 80, Height: 20})
	m = updateWhyTUI(t, m, keyPress("down"))
	m = updateWhyTUI(t, m, keyPress("n"))
	text := whyTUIViewText(t, m)
	if !strings.Contains(text, "empty.py") {
		t.Fatalf("empty-file view missing filename:\n%s", text)
	}
}

func TestWhyTUITinyWindowDoesNotPanic(t *testing.T) {
	t.Parallel()
	m := newWhyTUIModel(whyTUIFixture(), "", false, 0)
	m = updateWhyTUI(t, m, tea.WindowSizeMsg{Width: 8, Height: 3})
	_ = whyTUIViewText(t, m)
	m = updateWhyTUI(t, m, keyPress("down"))
	_ = whyTUIViewText(t, m)
}

// The agent-safe fallback contract: --tui against a non-TTY writer must fall
// through to the deterministic plain-text output (never start the TUI, never
// block). A bytes.Buffer is the non-TTY case IsTerminalWriter reports false for.
func TestWhyTUIFlagFallsBackToPlainTextWhenNotTTY(t *testing.T) {
	newAttributionRepo(t)

	var buf bytes.Buffer
	err := runAttributionWhy(t.Context(), &buf, "auth.py", attributionWhyOptions{TUI: true})
	if err != nil {
		t.Fatalf("runAttributionWhy --tui non-TTY: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "auth.py") || !strings.Contains(out, "lines") {
		t.Fatalf("expected the plain file summary as fallback, got:\n%s", out)
	}
}

// The interactive viewer must reject an out-of-range start line the same way
// the plain path does, instead of silently opening at line 1.
func TestWhyTUIStartLineOutOfRangeErrors(t *testing.T) {
	t.Parallel()
	result := whyTUIFixture() // lines 1-4

	if _, err := whyTUIStartLine(result, true, 99); err == nil {
		t.Fatal("expected an error for a start line outside the file")
	} else if !strings.Contains(err.Error(), "is outside") {
		t.Fatalf("error %q missing the shared \"is outside\" wording", err)
	}

	start, err := whyTUIStartLine(result, true, 3)
	if err != nil {
		t.Fatalf("in-range start line errored: %v", err)
	}
	if start != 3 {
		t.Fatalf("in-range start line = %d, want 3", start)
	}

	if start, err := whyTUIStartLine(result, false, 0); err != nil || start != 0 {
		t.Fatalf("no explicit line: got (%d, %v), want (0, nil)", start, err)
	}
}

// --json must win over --tui so scripted callers always get JSON.
func TestWhyTUIFlagJSONWins(t *testing.T) {
	newAttributionRepo(t)

	var buf bytes.Buffer
	err := runAttributionWhy(t.Context(), &buf, "auth.py", attributionWhyOptions{TUI: true, JSON: true})
	if err != nil {
		t.Fatalf("runAttributionWhy --tui --json: %v", err)
	}
	if !strings.HasPrefix(strings.TrimSpace(buf.String()), "{") {
		t.Fatalf("expected JSON output, got:\n%s", buf.String())
	}
}
