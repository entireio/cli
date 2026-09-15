package agents

import (
	"slices"
	"strings"
	"testing"
)

// TestModelEnvOverrides covers the per-runner model knobs that have no other
// test. E2E_CLAUDE_MODEL is the reason this file exists: it was documented in
// CLAUDE.md long before anything read it, so setting it changed nothing and
// said nothing.
func TestModelEnvOverrides(t *testing.T) {
	cases := []struct {
		name    string
		env     string
		resolve func() string
		want    string
	}{
		{name: "claude default", env: "", resolve: claudeModel, want: "haiku"},
		{name: "claude override", env: "opus", resolve: claudeModel, want: "opus"},
		{name: "copilot default", env: "", resolve: copilotModel, want: "claude-haiku-4.5"},
		{name: "copilot override", env: "gpt-5", resolve: copilotModel, want: "gpt-5"},

		// Cursor is the one runner with no in-code default: unset means
		// "leave Cursor's own routing alone", not "use a cheap model".
		{name: "cursor default is empty", env: "", resolve: cursorModel, want: ""},
		{name: "cursor override", env: "claude-opus-4-6", resolve: cursorModel, want: "claude-opus-4-6"},

		// A value pasted out of a shell or a workflow file often carries
		// whitespace; it must not reach the CLI as part of the model name.
		{name: "claude trims", env: "  haiku\n", resolve: claudeModel, want: "haiku"},
		{name: "cursor trims to empty", env: "   ", resolve: cursorModel, want: ""},
	}

	envFor := map[string]string{
		"claude":  "E2E_CLAUDE_MODEL",
		"copilot": "E2E_COPILOT_MODEL",
		"cursor":  "E2E_CURSOR_MODEL",
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runner, _, _ := strings.Cut(tc.name, " ")
			key, ok := envFor[runner]
			if !ok {
				t.Fatalf("no env var mapped for runner %q", runner)
			}
			t.Setenv(key, tc.env)
			if got := tc.resolve(); got != tc.want {
				t.Errorf("%s with %s=%q = %q, want %q", runner, key, tc.env, got, tc.want)
			}
		})
	}
}

// TestCursorArgsCarryTheModel pins that the model reaches the CLI, and only
// when there is one. An empty --model would be a flag with no value, which
// swallows the next argument.
func TestCursorArgsCarryTheModel(t *testing.T) {
	t.Parallel()

	bare := cursorArgs("/repo", "")
	if slices.Contains(bare, "--model") {
		t.Errorf("cursorArgs with no model = %v, want no --model flag", bare)
	}
	if !slices.Equal(bare, []string{"--force", "--workspace", "/repo"}) {
		t.Errorf("cursorArgs with no model = %v, want the pre-existing argv", bare)
	}

	pinned := cursorArgs("/repo", "claude-opus-4-6")
	want := []string{"--force", "--workspace", "/repo", "--model", "claude-opus-4-6"}
	if !slices.Equal(pinned, want) {
		t.Errorf("cursorArgs = %v, want %v", pinned, want)
	}
}
