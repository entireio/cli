package codex

import (
	"context"
	"io"
	"reflect"
	"testing"
)

// TestCodexSpawner_Name asserts the spawner reports the stable registry name.
func TestCodexSpawner_Name(t *testing.T) {
	t.Parallel()
	if got := NewSpawner().Name(); got != wantCodexAgentName {
		t.Errorf("Name() = %q, want %q", got, wantCodexAgentName)
	}
}

// TestCodexSpawner_Argv pins the argv + stdin contract:
//
//	codex exec --skip-git-repo-check --dangerously-bypass-approvals-and-sandbox -
//
// Prompt is piped on stdin. The bypass-approvals-and-sandbox flag is
// codex's documented way to run autonomously: less aggressive options
// (-s workspace-write, --add-dir) are not sufficient because codex's
// workspace-write policy excludes anything under `.git/` regardless of
// --add-dir, which blocks investigate's per-run dir at
// <git-common-dir>/entire-investigations/<run-id>/.
func TestCodexSpawner_Argv(t *testing.T) {
	t.Parallel()
	env := []string{"FOO=bar", "BAZ=qux"}
	cmd := NewSpawner().BuildCmd(context.Background(), env, "the-prompt")

	wantArgs := []string{
		wantCodexAgentName, "exec",
		"--skip-git-repo-check",
		"--dangerously-bypass-approvals-and-sandbox",
		"-",
	}
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

// TestCodexSpawner_Argv_StableUnderInvestigateEnv pins the contract
// that the argv does NOT change based on env vars. (A previous
// implementation appended --add-dir from ENTIRE_INVESTIGATE_FINDINGS_DOC;
// that approach didn't actually unblock writes under .git/, so we
// dropped it. This test pins the regression.)
func TestCodexSpawner_Argv_StableUnderInvestigateEnv(t *testing.T) {
	t.Parallel()
	env := []string{
		"FOO=bar",
		"ENTIRE_INVESTIGATE_FINDINGS_DOC=/repo/.git/entire-investigations/abcdef012345/findings.md",
	}
	cmd := NewSpawner().BuildCmd(context.Background(), env, "prompt")

	wantArgs := []string{
		wantCodexAgentName, "exec",
		"--skip-git-repo-check",
		"--dangerously-bypass-approvals-and-sandbox",
		"-",
	}
	if !reflect.DeepEqual(cmd.Args, wantArgs) {
		t.Errorf("Args = %v, want %v", cmd.Args, wantArgs)
	}
}
