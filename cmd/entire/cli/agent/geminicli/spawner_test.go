package geminicli

import (
	"context"
	"io"
	"reflect"
	"testing"
)

// TestGeminiCLISpawner_Name asserts the spawner reports the stable registry name.
func TestGeminiCLISpawner_Name(t *testing.T) {
	t.Parallel()
	if got := NewSpawner().Name(); got != "gemini-cli" {
		t.Errorf("Name() = %q, want %q", got, "gemini-cli")
	}
}

// TestGeminiCLISpawner_Argv pins the argv + stdin contract:
// gemini -p " " (space placeholder triggers headless mode), prompt via stdin.
func TestGeminiCLISpawner_Argv(t *testing.T) {
	t.Parallel()
	env := []string{"FOO=bar", "BAZ=qux"}
	cmd := NewSpawner().BuildCmd(context.Background(), env, "the-prompt")

	wantArgs := []string{"gemini", "-p", " "}
	if !reflect.DeepEqual(cmd.Args, wantArgs) {
		t.Errorf("Args = %v, want %v", cmd.Args, wantArgs)
	}

	if !reflect.DeepEqual(cmd.Env, env) {
		t.Errorf("Env = %v, want %v", cmd.Env, env)
	}

	if cmd.Stdin == nil {
		t.Fatal("Stdin = nil, want a reader carrying the prompt")
	}
	got, err := io.ReadAll(cmd.Stdin)
	if err != nil {
		t.Fatalf("read stdin: %v", err)
	}
	if string(got) != "the-prompt" {
		t.Errorf("stdin = %q, want %q", string(got), "the-prompt")
	}
}
