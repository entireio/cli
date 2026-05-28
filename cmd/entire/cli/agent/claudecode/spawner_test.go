package claudecode

import (
	"context"
	"reflect"
	"testing"
)

// TestClaudeCodeSpawner_Name asserts the spawner reports the stable
// registry name used by both review and investigate callers.
func TestClaudeCodeSpawner_Name(t *testing.T) {
	t.Parallel()
	if got := NewSpawner().Name(); got != "claude-code" {
		t.Errorf("Name() = %q, want %q", got, "claude-code")
	}
}

// TestClaudeCodeSpawner_Argv pins the argv contract:
//
//	claude -p --permission-mode bypassPermissions <prompt>
//
// The prompt is the last positional. --permission-mode bypassPermissions is
// required so file writes succeed in non-interactive mode (see
// spawner.go); stdin is unused.
func TestClaudeCodeSpawner_Argv(t *testing.T) {
	t.Parallel()
	env := []string{"FOO=bar", "BAZ=qux"}
	cmd := NewSpawner().BuildCmd(context.Background(), env, "the-prompt")

	wantArgs := []string{"claude", "-p", "--permission-mode", "bypassPermissions", "the-prompt"}
	if !reflect.DeepEqual(cmd.Args, wantArgs) {
		t.Errorf("Args = %v, want %v", cmd.Args, wantArgs)
	}

	if !reflect.DeepEqual(cmd.Env, env) {
		t.Errorf("Env = %v, want %v", cmd.Env, env)
	}

	if cmd.Stdin != nil {
		t.Errorf("Stdin = %v, want nil (claude uses argv, not stdin)", cmd.Stdin)
	}
}
