package cli

import (
	"bytes"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestLabsCmd_PrintsExperimentalCommandList(t *testing.T) {
	t.Parallel()

	root := NewRootCmd()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"labs"})

	if err := root.Execute(); err != nil {
		t.Fatalf("entire labs failed: %v", err)
	}
	got := out.String()
	for _, want := range []string{
		"Labs",
		"newer Entire workflows",
		"Available experimental commands",
		"entire review",
		"entire review --help",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("entire labs output missing %q:\n%s", want, got)
		}
	}
}

func TestLabsCmd_HelpShowsExperimentalCommandList(t *testing.T) {
	t.Parallel()

	root := NewRootCmd()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"labs", "--help"})

	if err := root.Execute(); err != nil {
		t.Fatalf("entire labs --help failed: %v", err)
	}
	got := out.String()
	for _, want := range []string{"Labs", "entire review"} {
		if !strings.Contains(got, want) {
			t.Fatalf("entire labs --help output missing %q:\n%s", want, got)
		}
	}
}

func TestLabsCmd_RejectsTopicWithoutRunningIt(t *testing.T) {
	t.Parallel()

	root := NewRootCmd()
	var out, errOut bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errOut)
	root.SetArgs([]string{"labs", "review"})

	err := root.Execute()
	if err == nil {
		t.Fatal("entire labs review should return an error")
	}
	if !strings.Contains(err.Error(), "unknown labs topic") {
		t.Fatalf("error should mention unknown labs topic, got: %v", err)
	}
	if !strings.Contains(errOut.String(), "entire review --help") {
		t.Fatalf("stderr should point to canonical review help, got:\n%s", errOut.String())
	}
	if strings.Contains(out.String(), "Run the review skills configured") {
		t.Fatalf("entire labs review should not run or show review help, got stdout:\n%s", out.String())
	}
}

func TestRootHelp_ShowsLabsButHidesReview(t *testing.T) {
	t.Parallel()

	root := NewRootCmd()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"--help"})

	if err := root.Execute(); err != nil {
		t.Fatalf("entire --help failed: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "labs") || !strings.Contains(got, "Explore experimental Entire workflows") {
		t.Fatalf("root help should include labs command, got:\n%s", got)
	}
	if strings.Contains(got, "review") {
		t.Fatalf("root help should not include review while it is listed in labs, got:\n%s", got)
	}
}

// summaryColumns returns, for each non-empty rendered row, the rune offset at
// which the summary begins (i.e. the column after the padded invocation).
func summaryColumns(t *testing.T, commands []experimentalCommandInfo) []int {
	t.Helper()
	var cols []int
	for _, line := range strings.Split(renderExperimentalCommands(commands), "\n") {
		if line == "" {
			continue
		}
		info := indexOfSummary(t, line, commands)
		cols = append(cols, info)
	}
	return cols
}

// indexOfSummary finds the rune offset of a row's summary text within the line.
func indexOfSummary(t *testing.T, line string, commands []experimentalCommandInfo) int {
	t.Helper()
	for _, info := range commands {
		if idx := strings.Index(line, info.Summary); idx >= 0 {
			return utf8.RuneCountInString(line[:idx])
		}
	}
	t.Fatalf("no known summary found in rendered line %q", line)
	return -1
}

func TestRenderExperimentalCommands_SummariesAlign(t *testing.T) {
	t.Parallel()

	cols := summaryColumns(t, experimentalCommands)
	if len(cols) < 2 {
		t.Fatalf("expected multiple experimental commands, got %d", len(cols))
	}
	for i, col := range cols {
		if col != cols[0] {
			t.Fatalf("summary column %d (%d) does not match first column (%d); descriptions are misaligned", i, col, cols[0])
		}
	}
}

func TestRenderExperimentalCommands_ColumnWidthAdjustsToLongest(t *testing.T) {
	t.Parallel()

	short := []experimentalCommandInfo{
		{Name: "a", Invocation: "entire a", Summary: "first"},
		{Name: "b", Invocation: "entire b", Summary: "second"},
	}
	long := []experimentalCommandInfo{
		{Name: "a", Invocation: "entire a", Summary: "first"},
		{Name: "verylongcommand", Invocation: "entire verylongcommand", Summary: "second"},
	}

	shortCol := summaryColumns(t, short)[0]
	longCol := summaryColumns(t, long)[0]

	if longCol <= shortCol {
		t.Fatalf("column should widen for a longer invocation: short=%d long=%d", shortCol, longCol)
	}
	// All rows in the long set must still align despite differing invocation lengths.
	for i, col := range summaryColumns(t, long) {
		if col != longCol {
			t.Fatalf("row %d column %d does not match %d", i, col, longCol)
		}
	}
}

func TestRenderExperimentalCommands_MultiByteInvocationAligns(t *testing.T) {
	t.Parallel()

	// "entire ▶▶" is 9 runes but 13 bytes (each ▶ is 3 bytes). The longest
	// invocation below is 12 runes, so the column width is 12. With byte-based
	// padding, len("entire ▶▶") == 13 >= 12 would skip padding and misalign the
	// row; rune-based padding correctly adds 3 spaces.
	commands := []experimentalCommandInfo{
		{Name: "long", Invocation: "entire aaaaa", Summary: "first"},
		{Name: "multibyte", Invocation: "entire ▶▶", Summary: "second"},
	}

	if got := len("entire ▶▶"); got < 12 {
		t.Fatalf("test precondition broken: byte length %d should exceed column width 12", got)
	}

	cols := summaryColumns(t, commands)
	if cols[0] != cols[1] {
		t.Fatalf("multi-byte invocation summary misaligned: %v", cols)
	}
}

func TestLabsRegistryCommandsExistAtCanonicalPaths(t *testing.T) {
	t.Parallel()

	root := NewRootCmd()
	for _, info := range experimentalCommands {
		cmd, _, err := root.Find([]string{info.Name})
		if err != nil {
			t.Fatalf("labs command %q should exist at canonical path: %v", info.Name, err)
		}
		if cmd == nil {
			t.Fatalf("labs command %q resolved to nil command", info.Name)
		}
	}
}
