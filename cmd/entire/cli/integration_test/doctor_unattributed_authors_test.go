//go:build integration

package integration

import (
	"errors"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// runDoctor spawns the real binary's `doctor` in env's repo, logged out: a
// per-test ENTIRE_CONFIG_DIR (no saved context, overriding the shared one
// TestMain sets process-wide) so the unattributed-authors check always
// renders its "not logged in" branch. TestMain already unsets ENTIRE_TOKEN
// process-wide (setup_test.go:89-97), so stripping it here again is
// defensive, not load-bearing.
func runDoctor(t *testing.T, env *TestEnv) (exitCode int, output string) {
	t.Helper()

	childEnv := slices.DeleteFunc(env.cliEnv(), func(kv string) bool {
		return strings.HasPrefix(kv, "ENTIRE_TOKEN=")
	})
	childEnv = append(childEnv, "ENTIRE_CONFIG_DIR="+t.TempDir())

	stdout, stderr, err := runEntire(t, childEnv, env.RepoDir, "doctor")
	output = stdout + stderr
	if err == nil {
		return 0, output
	}
	// Exit-code derivation mirrors runEntireInRepo (entiredir_guard_test.go).
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("run doctor: %v\n%s", err, output)
	}
	return exitErr.ExitCode(), output
}

// osUserLocalPart returns the current OS user, normalized the same way
// unattributed_authors.go's normalizeOSUsername does: lowercase, then
// everything after the last backslash (stripping a Windows "DOMAIN\"
// prefix). Reimplemented here (normalizeOSUsername is unexported in package
// cli) rather than exported solely for this test.
func osUserLocalPart(t *testing.T) string {
	t.Helper()
	u, err := user.Current()
	if err != nil {
		t.Skipf("user.Current: %v (production offers nothing without an OS user)", err)
	}
	name := strings.ToLower(strings.TrimSpace(u.Username))
	if i := strings.LastIndexByte(name, '\\'); i >= 0 {
		name = name[i+1:]
	}
	return name
}

// TestDoctor_UnattributedAuthors_LoggedOutMentionsOwnAddressOnly verifies the
// logged-out branch of doctor's COR-1289 check: it names only the address(es)
// whose local part matches the current OS user, never an unrelated
// reserved-host author also present in history, and it never touches the
// network (runDoctor strips ENTIRE_TOKEN and points ENTIRE_CONFIG_DIR at an
// empty per-test directory with no active login context).
func TestDoctor_UnattributedAuthors_LoggedOutMentionsOwnAddressOnly(t *testing.T) {
	t.Parallel()
	ForEachBackend(t, func(t *testing.T, backend string) {
		env := NewFeatureBranchEnv(t)
		env.CheckpointStore = backend

		checkpointID := createCheckpointedCommit(t, env, "Add gate module", "gate.go", "package gate", "Add gate module")
		if checkpointID == "" {
			t.Fatal("expected a checkpoint ID from createCheckpointedCommit")
		}
		if !env.CheckpointsPresentLocally() {
			t.Fatal("expected checkpoints to be present locally")
		}

		me := osUserLocalPart(t)
		myAddr := me + "@test-box.local"
		testutil.RunGit(t, env.RepoDir, "-c", "user.email="+myAddr, "-c", "user.name=Me",
			"commit", "--allow-empty", "--no-gpg-sign", "-m", "empty commit as me (1)")
		testutil.RunGit(t, env.RepoDir, "-c", "user.email="+myAddr, "-c", "user.name=Me",
			"commit", "--allow-empty", "--no-gpg-sign", "-m", "empty commit as me (2)")
		testutil.RunGit(t, env.RepoDir, "-c", "user.email=someoneelse@test-box.local", "-c", "user.name=Someone Else",
			"commit", "--allow-empty", "--no-gpg-sign", "-m", "empty commit as someone else")

		exitCode, output := runDoctor(t, env)
		if exitCode != 0 {
			t.Fatalf("doctor exited %d:\n%s", exitCode, output)
		}
		if !strings.Contains(output, "Unattributed authors: NOT CHECKED (not logged in)") {
			t.Errorf("output missing the NOT CHECKED header, got:\n%s", output)
		}
		if !strings.Contains(output, myAddr) {
			t.Errorf("output missing own address %q, got:\n%s", myAddr, output)
		}
		if !strings.Contains(output, "entire login") {
			t.Errorf("output missing the `entire login` fix hint, got:\n%s", output)
		}
		if strings.Contains(output, "someoneelse@") {
			t.Errorf("output must not mention an address that isn't the OS user's, got:\n%s", output)
		}
	})
}

// TestDoctor_UnattributedAuthors_ExcludesCheckpointRefsUnderBothStores makes
// the three --exclude flags in localCandidateAuthors load-bearing: checkpoint
// commits are written with the user's own git identity
// (checkpoint.GetGitAuthorFromRepo, repo-then-global config), so without the
// excludes a repo's own checkpoint history would make every user with
// user.email set look "unattributed" to themselves. gitCommitWithShadowHooks
// hardcodes the fixture author for the code commit itself, so setting
// user.email in repo-local config only reaches the checkpoint/condensation
// commits the post-commit hook creates from it.
//
//   - refs/heads/entire/* (+ refs/remotes/*/entire/*) protects the
//     git-branch store's entire/checkpoints/v1 branch and per-session shadow
//     branches (entire/<hash>-<hash>).
//   - refs/entire/* protects the git-refs store's per-checkpoint refs
//     (refs/entire/checkpoints/<shard>/<id>, refs/entire/policies/*).
func TestDoctor_UnattributedAuthors_ExcludesCheckpointRefsUnderBothStores(t *testing.T) {
	t.Parallel()
	ForEachBackend(t, func(t *testing.T, backend string) {
		env := NewFeatureBranchEnv(t)
		env.CheckpointStore = backend

		me := osUserLocalPart(t)
		myAddr := me + "@test-box.local"
		testutil.RunGit(t, env.RepoDir, "config", "user.email", myAddr)
		testutil.RunGit(t, env.RepoDir, "config", "user.name", "Me")
		// This test intentionally rewrites repo-local user.name/user.email
		// (createCheckpointedCommit's checkpoint/condensation commits read it
		// via checkpoint.GetGitAuthorFromRepo), which trips the harness's
		// .git/config drift guard; tell it the change was deliberate.
		configData, err := os.ReadFile(filepath.Join(env.RepoDir, ".git", "config"))
		if err != nil {
			t.Fatalf("read .git/config: %v", err)
		}
		env.AcceptGitConfigChanges(string(configData))

		createCheckpointedCommit(t, env, "Add gate module", "gate.go", "package gate", "Add gate module")
		createCheckpointedCommit(t, env, "Add router module", "router.go", "package router", "Add router module")

		if !env.CheckpointsPresentLocally() {
			t.Fatal("expected checkpoints to be present locally")
		}

		// Sanity-check the premise: the address really is out there as the
		// tip author of the refs the excludes are meant to hide, so a
		// passing assertion below is because of the excludes and not because
		// the address never landed anywhere under this backend's store.
		// Bare prefixes (no trailing "/*"): for-each-ref's glob treats "*" as
		// FNM_PATHNAME (it never crosses a "/"), so "refs/heads/entire/*"
		// would miss two-segment-deep refs like
		// refs/heads/entire/checkpoints/v1. An unglobbed prefix matches
		// recursively instead.
		tips := testutil.RunGit(t, env.RepoDir, "for-each-ref", "--format=%(refname) %(authoremail)",
			"refs/entire", "refs/heads/entire")
		if !strings.Contains(tips, "<"+myAddr+">") {
			t.Fatalf("premise: expected %q as tip author on Entire refs under backend %s, got:\n%s", myAddr, backend, tips)
		}
		allAuthors := testutil.RunGit(t, env.RepoDir, "log", "--all", "--format=%ae")
		if !strings.Contains(allAuthors, myAddr) {
			t.Fatalf("premise check failed: expected %q among all authors via `git log --all`, got:\n%s", myAddr, allAuthors)
		}

		exitCode, output := runDoctor(t, env)
		if exitCode != 0 {
			t.Fatalf("doctor exited %d:\n%s", exitCode, output)
		}
		if strings.Contains(output, "Unattributed authors") {
			t.Errorf("expected no Unattributed authors section (checkpoint refs must be excluded from the scan), got:\n%s", output)
		}
	})
}

// TestDoctor_UnattributedAuthors_IgnoresEntireRefsDirectly is a
// hooks-free, belt-and-braces version of the exclusion test above: it plants
// a detached commit authored by the OS user's reserved-host address directly
// under Entire's own ref namespaces (no shadow branches, no condensation, no
// TestEnv checkpoint machinery at all), then proves doctor genuinely ignores
// only those refs by pointing an ordinary branch at the same commit and
// checking the address then appears.
func TestDoctor_UnattributedAuthors_IgnoresEntireRefsDirectly(t *testing.T) {
	t.Parallel()

	env := NewTestEnv(t)
	dir := env.RepoDir
	testutil.InitRepo(t, dir)
	testutil.WriteFile(t, dir, "README.md", "# Test Repository")
	testutil.GitAdd(t, dir, "README.md")
	testutil.GitCommit(t, dir, "Initial commit")

	me := osUserLocalPart(t)
	myAddr := me + "@test-box.local"

	headSHA := strings.TrimSpace(testutil.RunGit(t, dir, "rev-parse", "HEAD"))
	headTree := strings.TrimSpace(testutil.RunGit(t, dir, "rev-parse", "HEAD^{tree}"))
	detachedSHA := strings.TrimSpace(testutil.RunGit(t, dir,
		"-c", "user.name=Me", "-c", "user.email="+myAddr,
		"commit-tree", headTree, "-p", headSHA, "-m", "detached checkpoint-shaped commit"))

	entireRefs := []string{
		"refs/entire/checkpoints/aa/test",
		"refs/entire/policies/checkpoint",
		"refs/heads/entire/checkpoints/v1",
		"refs/heads/entire/abcdef1-123456",
		"refs/remotes/origin/entire/checkpoints/v1",
	}
	for _, ref := range entireRefs {
		testutil.RunGit(t, dir, "update-ref", ref, detachedSHA)
	}

	// Phase 1: only Entire's own refs point at the commit. Doctor may
	// complain about other things (not enabled, no git hooks installed) in
	// this bare repo; assert only on the unattributed-authors substrings.
	_, output := runDoctor(t, env)
	if strings.Contains(output, "Unattributed authors") {
		t.Errorf("expected no Unattributed authors section while the commit is reachable only via Entire's own refs, got:\n%s", output)
	}

	// Phase 2 (positive control): the same commit reachable from an
	// ordinary branch must be picked up, proving the check actually ran and
	// phase 1 wasn't a false negative from some other cause.
	testutil.RunGit(t, dir, "update-ref", "refs/heads/other", detachedSHA)
	_, output = runDoctor(t, env)
	if !strings.Contains(output, myAddr) {
		t.Errorf("expected %q to be reported once reachable from an ordinary branch, got:\n%s", myAddr, output)
	}
	if !strings.Contains(output, "Unattributed authors: NOT CHECKED (not logged in)") {
		t.Errorf("output missing the NOT CHECKED header, got:\n%s", output)
	}
}
