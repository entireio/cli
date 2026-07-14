package agents

import (
	"strings"
	"testing"
)

// TestOpenCodePromptEnv_OverridesPWD verifies the env handed to the opencode
// child process points PWD at the run directory and contains exactly one PWD
// entry. opencode (Node) resolves its project/worktree root from
// process.env.PWD, and Go's cmd.Dir chdirs without updating PWD — so without
// this override, file operations land in the go-test package dir instead of the
// test repo.
func TestOpenCodePromptEnv_OverridesPWD(t *testing.T) {
	t.Parallel()
	base := []string{
		"PWD=/some/stale/go-test/dir",
		"ENTIRE_TEST_TTY=1",
		"HOME=/home/runner",
	}

	env := openCodePromptEnv(base, "/tmp/e2e-repo-123")

	got, ok := envValue(env, "PWD")
	if !ok {
		t.Fatal("PWD not present in child env")
	}
	if got != "/tmp/e2e-repo-123" {
		t.Fatalf("PWD = %q, want %q", got, "/tmp/e2e-repo-123")
	}

	// The stale PWD must not survive: filterEnv strips it before we re-add.
	count := 0
	for _, e := range env {
		if strings.HasPrefix(e, "PWD=") {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("expected exactly one PWD entry, got %d", count)
	}

	// ENTIRE_TEST_TTY is still stripped so the agent exercises real TTY detection.
	if got, ok := envValue(env, "ENTIRE_TEST_TTY"); ok {
		t.Fatalf("ENTIRE_TEST_TTY = %q, want stripped", got)
	}

	// Unrelated env is preserved.
	if got, _ := envValue(env, "HOME"); got != "/home/runner" {
		t.Fatalf("HOME = %q, want preserved", got)
	}
}
