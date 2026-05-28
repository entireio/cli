//go:build e2e

package tests

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/e2e/entire"
	"github.com/entireio/cli/e2e/testutil"
)

// TestAlternates_RelativeObjectAlternate_CheckpointSync exercises the checkpoint
// push/sync path in a repository whose objects are resolved through a RELATIVE
// objects/info/alternates entry — the layout produced by a shared clone
// (`git clone --shared` / `--reference`).
//
// go-git cannot follow relative alternates on its own (it strips "../" and
// anchors at the filesystem root), so gitrepo.OpenPath rewrites them to absolute
// before go-git reads them. Without that, the pre-push sync's
// collectCommitsSince() fails with "object not found" on alternate-resident
// checkpoint commits, even though git itself resolves them.
func TestAlternates_RelativeObjectAlternate_CheckpointSync(t *testing.T) {
	// Siblings under one parent so the relative alternate path is short and
	// mirrors a real shared-clone layout.
	parent := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(parent); err == nil {
		parent = resolved
	}
	shared := filepath.Join(parent, "shared")
	work := filepath.Join(parent, "work")
	originBare := filepath.Join(parent, "origin.git")

	// Build the shared object store. Its objects become the alternate; the work
	// repo will hold none of these locally.
	testutil.Git(t, parent, "init", "shared")
	testutil.Git(t, shared, "config", "user.name", "Alt Test")
	testutil.Git(t, shared, "config", "user.email", "alt@test.local")
	writeRepoFile(t, shared, "base.txt", "base\n")
	testutil.Git(t, shared, "add", ".")
	testutil.Git(t, shared, "commit", "-m", "B0")
	base := testutil.GitOutput(t, shared, "rev-parse", "HEAD")

	// Checkpoint-branch commits K1<-K2, created in the shared store so they are
	// only reachable from the work repo via the alternate.
	testutil.Git(t, shared, "checkout", "-b", "cp")
	writeRepoFile(t, shared, "k1.txt", "k1\n")
	testutil.Git(t, shared, "add", ".")
	testutil.Git(t, shared, "commit", "-m", "K1")
	writeRepoFile(t, shared, "k2.txt", "k2\n")
	testutil.Git(t, shared, "add", ".")
	testutil.Git(t, shared, "commit", "-m", "K2")
	k2 := testutil.GitOutput(t, shared, "rev-parse", "HEAD")

	// A divergent commit R1 (sibling of the cp chain off B0) for the remote side,
	// so the push is non-fast-forward and triggers the rebase/sync path.
	testutil.Git(t, shared, "checkout", "-b", "remoteside", base)
	writeRepoFile(t, shared, "r1.txt", "r1\n")
	testutil.Git(t, shared, "add", ".")
	testutil.Git(t, shared, "commit", "-m", "R1")
	r1 := testutil.GitOutput(t, shared, "rev-parse", "HEAD")
	testutil.Git(t, shared, "checkout", "cp")

	// Bare remote seeded with R1 as the checkpoint branch tip.
	testutil.Git(t, parent, "init", "--bare", "origin.git")
	testutil.Git(t, shared, "push", originBare, "remoteside:refs/heads/entire/checkpoints/v1")

	// Shared clone: --shared makes work resolve objects from shared via an
	// alternate and copies no objects locally.
	testutil.Git(t, parent, "clone", "--shared", "-o", "base", "shared", "work")
	testutil.Git(t, work, "config", "user.name", "Alt Test")
	testutil.Git(t, work, "config", "user.email", "alt@test.local")

	// Rewrite the alternate to a RELATIVE path (git clone writes it absolute);
	// this is the layout go-git mishandles and the fix repairs.
	rel, err := filepath.Rel(
		filepath.Join(work, ".git", "objects"),
		filepath.Join(shared, ".git", "objects"),
	)
	if err != nil {
		t.Fatalf("compute relative alternate: %v", err)
	}
	if !strings.HasPrefix(rel, "..") {
		t.Fatalf("expected a relative alternate, got %q", rel)
	}
	altFile := filepath.Join(work, ".git", "objects", "info", "alternates")
	if err := os.WriteFile(altFile, []byte(rel+"\n"), 0o644); err != nil {
		t.Fatalf("write relative alternates: %v", err)
	}

	entire.Enable(t, work, "claude-code")

	// Point the local checkpoint branch at K2 (object lives only in the
	// alternate) and aim the checkpoint push at the bare remote.
	testutil.Git(t, work, "update-ref", "refs/heads/entire/checkpoints/v1", k2)
	testutil.Git(t, work, "remote", "add", "origin", originBare)

	// Drive the real pre-push hook: non-ff vs the remote forces the sync/rebase
	// path that reads the alternate-resident checkpoint commits via go-git.
	cmd := exec.Command(entire.BinPath(), "hooks", "git", "pre-push", "origin")
	cmd.Dir = work
	cmd.Env = upsertEnv(os.Environ(), "ENTIRE_TEST_TTY", "0", "GIT_TERMINAL_PROMPT", "0")
	combined, runErr := cmd.CombinedOutput()
	output := string(combined)
	t.Logf("entire pre-push output:\n%s", output)
	if runErr != nil {
		t.Fatalf("pre-push hook exited non-zero: %v\n%s", runErr, output)
	}

	if strings.Contains(output, "object not found") || strings.Contains(output, "couldn't sync") {
		t.Fatalf("checkpoint sync could not resolve alternate-resident objects (relative alternate not followed):\n%s", output)
	}

	// The rebase should have cherry-picked K1,K2 onto R1 and pushed, advancing
	// the remote checkpoint branch past R1.
	got := testutil.GitOutput(t, originBare, "rev-parse", "refs/heads/entire/checkpoints/v1")
	if got == r1 {
		t.Fatalf("origin checkpoint branch did not advance past R1 (%s); sync/push did not complete:\n%s", r1, output)
	}
}

func writeRepoFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

// upsertEnv returns env with each key=value pair set, replacing the first
// occurrence of an existing entry for that key. Avoids the silent shadowing
// that happens when a key is simply appended to os.Environ() but already
// present in the inherited environment.
func upsertEnv(env []string, pairs ...string) []string {
	if len(pairs)%2 != 0 {
		panic("upsertEnv requires an even number of key/value arguments")
	}
	out := make([]string, len(env))
	copy(out, env)
	for i := 0; i < len(pairs); i += 2 {
		key, value := pairs[i], pairs[i+1]
		prefix := key + "="
		entry := prefix + value
		replaced := false
		for j, e := range out {
			if strings.HasPrefix(e, prefix) {
				out[j] = entry
				replaced = true
				break
			}
		}
		if !replaced {
			out = append(out, entry)
		}
	}
	return out
}
