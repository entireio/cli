//go:build integration

package integration

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/execx"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// refFormatReftable is git's rev-parse --show-ref-format value for a repository
// using the reftable ref backend.
const refFormatReftable = "reftable"

// TestReftableRepository_EnableAndFirstCheckpoint exercises the full capture
// flow (enable -> session start -> file changes -> stop -> user commit ->
// checkpoint) against a repository using the reftable ref backend. go-git's
// filesystem storer cannot read reftable refs, so this verifies the git-CLI
// ref routing in gitrepo.reftableStorer works end to end. Regression for #547.
func TestReftableRepository_EnableAndFirstCheckpoint(t *testing.T) {
	t.Parallel()
	requireGitReftableSupport(t)

	env := NewTestEnv(t)

	// Set up the reftable repo and initial commit directly via git CLI, matching
	// the sha256 repo test: integration tests avoid the enable bootstrap path and
	// drive hooks through getTestBinary() so they exercise the binary under test.
	gitOutput(t, "", "init", "--ref-format=reftable", env.RepoDir)
	gitOutput(t, env.RepoDir, "config", "user.name", "Test User")
	gitOutput(t, env.RepoDir, "config", "user.email", "test@example.com")
	gitOutput(t, env.RepoDir, "config", "commit.gpgsign", "false")
	env.WriteFile("README.md", "# reftable repo\n")
	gitOutput(t, env.RepoDir, "add", "README.md")
	gitOutput(t, env.RepoDir, "commit", "-m", "Initial reftable commit")

	if got := gitOutput(t, env.RepoDir, "rev-parse", "--show-ref-format"); got != refFormatReftable {
		t.Fatalf("repository ref format = %q, want reftable", got)
	}

	// Pin the git-branch backend: this test asserts the v1-branch condensation
	// flow in a reftable repo, and first-run enable now defaults new setups to
	// git-refs.
	output := env.RunCLI(
		"enable",
		"--no-github",
		"--agent", "claude-code",
		"--telemetry=false",
		"--checkpoint-backend", "branch",
	)
	if !strings.Contains(output, paths.MetadataBranchName) {
		t.Fatalf("expected enable to create %s branch, got output:\n%s", paths.MetadataBranchName, output)
	}

	// The metadata branch is a real ref; resolving it proves the reftable-backed
	// ref write during enable succeeded.
	initialHead := gitOutput(t, env.RepoDir, "rev-parse", "HEAD")
	initialMetadataHead := gitOutput(t, env.RepoDir, "rev-parse", paths.MetadataBranchName)

	sess := env.NewSession()
	prompt := "Create a file in the reftable repo"
	if err := env.SimulateUserPromptSubmitWithPromptAndTranscriptPath(sess.ID, prompt, sess.TranscriptPath); err != nil {
		t.Fatalf("user-prompt-submit failed: %v", err)
	}

	const mainContent = "package main\n\nfunc main() {}\n"
	env.WriteFile("main.go", mainContent)
	sess.CreateTranscript(prompt, []FileChange{{Path: "main.go", Content: mainContent}})
	if err := env.SimulateStop(sess.ID, sess.TranscriptPath); err != nil {
		t.Fatalf("stop hook failed creating first checkpoint: %v", err)
	}

	state, err := env.GetSessionState(sess.ID)
	if err != nil {
		t.Fatalf("GetSessionState failed: %v", err)
	}
	if state == nil || state.StepCount != 1 {
		t.Fatalf("session StepCount after first checkpoint = %#v, want 1", state)
	}

	// The shadow branch is created and advanced via reftable ref writes.
	shadowBranch := env.GetShadowBranchNameForCommit(initialHead)
	if got := gitOutput(t, env.RepoDir, "rev-parse", shadowBranch); got == "" {
		t.Fatalf("expected shadow branch %s to resolve", shadowBranch)
	}

	env.GitCommitWithShadowHooks("Add reftable main", "main.go")
	userHead := gitOutput(t, env.RepoDir, "rev-parse", "HEAD")
	if userHead == initialHead {
		t.Fatal("expected user commit to advance HEAD")
	}

	// The condensation on user commit advances the metadata branch (a ref write)
	// and links the checkpoint via a commit trailer on the user commit.
	metadataHead := gitOutput(t, env.RepoDir, "rev-parse", paths.MetadataBranchName)
	if metadataHead == initialMetadataHead {
		t.Fatal("expected metadata branch to advance after condensing the first checkpoint")
	}

	subject := gitOutput(t, env.RepoDir, "log", "-1", "--format=%s", paths.MetadataBranchName)
	if !strings.HasPrefix(subject, "Checkpoint: ") {
		t.Fatalf("metadata branch latest subject = %q, want Checkpoint: <id>", subject)
	}
	checkpointID := strings.TrimPrefix(subject, "Checkpoint: ")

	userBody := gitOutput(t, env.RepoDir, "log", "-1", "--format=%B", "HEAD")
	if !strings.Contains(userBody, "Entire-Checkpoint: "+checkpointID) {
		t.Fatalf("user commit body missing Entire-Checkpoint trailer for %s:\n%s", checkpointID, userBody)
	}

	if _, found := env.ReadFileFromBranch(paths.MetadataBranchName, SessionMetadataPath(checkpointID)); !found {
		t.Fatalf("expected session metadata for checkpoint %s on %s", checkpointID, paths.MetadataBranchName)
	}

	// checkpoint list must work against the reftable repo (read path).
	listOut := env.RunCLI("checkpoint", "list")
	if !strings.Contains(listOut, checkpointID) {
		t.Fatalf("checkpoint list missing checkpoint %s:\n%s", checkpointID, listOut)
	}
}

// TestReftableRepository_LinkedWorktree verifies the capture flow works inside a
// linked worktree of a reftable repository, where the shared reftable stack
// lives under the common git dir rather than the worktree git dir.
func TestReftableRepository_LinkedWorktree(t *testing.T) {
	t.Parallel()
	requireGitReftableSupport(t)

	env := NewTestEnv(t)

	gitOutput(t, "", "init", "--ref-format=reftable", env.RepoDir)
	gitOutput(t, env.RepoDir, "config", "user.name", "Test User")
	gitOutput(t, env.RepoDir, "config", "user.email", "test@example.com")
	gitOutput(t, env.RepoDir, "config", "commit.gpgsign", "false")
	env.WriteFile("README.md", "# reftable repo\n")
	gitOutput(t, env.RepoDir, "add", "README.md")
	gitOutput(t, env.RepoDir, "commit", "-m", "Initial reftable commit")

	// Create a linked worktree on a feature branch.
	worktreePath := filepath.Join(t.TempDir(), "wt")
	gitOutput(t, env.RepoDir, "worktree", "add", "-b", "feature/wt", worktreePath)

	// Enable and drive a checkpoint from within the worktree by pointing the CLI
	// at the worktree directory.
	// Pin the git-branch backend (see TestReftableRepository_EnableAndFirstCheckpoint):
	// first-run enable now defaults to git-refs, but this test asserts the
	// v1-branch metadata flow.
	runCLIIn(t, env, worktreePath, "enable", "--no-github", "--agent", "claude-code", "--telemetry=false", "--checkpoint-backend", "branch")

	if got := gitOutput(t, worktreePath, "rev-parse", "--show-ref-format"); got != refFormatReftable {
		t.Fatalf("worktree ref format = %q, want reftable", got)
	}

	// A ref read against the worktree (HEAD resolution through the reftable
	// storer, whose HEAD stub go-git cannot read) must return the real branch.
	branch := gitOutput(t, worktreePath, "rev-parse", "--abbrev-ref", "HEAD")
	if branch != "feature/wt" {
		t.Fatalf("worktree branch = %q, want feature/wt", branch)
	}

	metadataHead := gitOutput(t, worktreePath, "rev-parse", paths.MetadataBranchName)
	if metadataHead == "" {
		t.Fatalf("expected metadata branch to resolve from worktree")
	}
}

// TestReftableRepository_GitRefsBackend exercises the full capture flow against a
// reftable repository using the shipped default checkpoint backend (git-refs),
// where each checkpoint is condensed to its own ref under refs/entire/checkpoints
// rather than to the entire/checkpoints/v1 branch. Every ref write and read goes
// through gitrepo.reftableStorer, so this proves the reftable backend works with
// the default per-checkpoint ref layout, not just the git-branch flow. It runs
// WITHOUT --checkpoint-backend and without an ENTIRE_CHECKPOINTS_PRIMARY override
// so it pins the actual shipped default.
func TestReftableRepository_GitRefsBackend(t *testing.T) {
	t.Parallel()
	requireGitReftableSupport(t)

	env := NewTestEnv(t)
	bootstrapReftableRepo(t, env)

	// No --checkpoint-backend flag: exercise the shipped first-run default, which
	// must write the git-refs primary into settings.json.
	env.RunCLI("enable", "--no-github", "--agent", "claude-code", "--telemetry=false")
	if s := env.ReadFile(".entire/settings.json"); !strings.Contains(s, `"git-refs"`) {
		t.Fatalf("first-run enable on a reftable repo should default to the git-refs backend, settings.json:\n%s", s)
	}

	initialHead := gitOutput(t, env.RepoDir, "rev-parse", "HEAD")

	sess := env.NewSession()
	prompt := "Create a file in the reftable repo"
	if err := env.SimulateUserPromptSubmitWithPromptAndTranscriptPath(sess.ID, prompt, sess.TranscriptPath); err != nil {
		t.Fatalf("user-prompt-submit failed: %v", err)
	}

	const mainContent = "package main\n\nfunc main() {}\n"
	env.WriteFile("main.go", mainContent)
	sess.CreateTranscript(prompt, []FileChange{{Path: "main.go", Content: mainContent}})
	if err := env.SimulateStop(sess.ID, sess.TranscriptPath); err != nil {
		t.Fatalf("stop hook failed creating first checkpoint: %v", err)
	}

	state, err := env.GetSessionState(sess.ID)
	if err != nil {
		t.Fatalf("GetSessionState failed: %v", err)
	}
	if state == nil || state.StepCount != 1 {
		t.Fatalf("session StepCount after first checkpoint = %#v, want 1", state)
	}

	// The shadow branch is created and advanced via reftable ref writes.
	shadowBranch := env.GetShadowBranchNameForCommit(initialHead)
	if got := gitOutput(t, env.RepoDir, "rev-parse", shadowBranch); got == "" {
		t.Fatalf("expected shadow branch %s to resolve", shadowBranch)
	}

	env.GitCommitWithShadowHooks("Add reftable main", "main.go")
	if userHead := gitOutput(t, env.RepoDir, "rev-parse", "HEAD"); userHead == initialHead {
		t.Fatal("expected user commit to advance HEAD")
	}

	// git-refs artifact: the condensed checkpoint lands on a per-checkpoint ref
	// under refs/entire/checkpoints (written through the reftable storer), and the
	// v1 branch is never created under this backend.
	if refs := gitOutput(t, env.RepoDir, "for-each-ref", checkpointRefPrefix); refs == "" {
		t.Fatalf("expected a per-checkpoint ref under %s for the git-refs default", checkpointRefPrefix)
	}
	if env.BranchExists(paths.MetadataBranchName) {
		t.Fatalf("git-refs default must not create the %s branch", paths.MetadataBranchName)
	}

	// The checkpoint's exact ref resolves through the reftable read path, and its
	// ID is recoverable from the code commit's Entire-Checkpoint trailer.
	checkpointID := env.GetLatestCheckpointIDFromHistory()
	if !refExists(t, env.RepoDir, checkpointRefName(checkpointID)) {
		t.Fatalf("expected checkpoint ref %s to resolve", checkpointRefName(checkpointID))
	}

	// checkpoint list must work against the reftable repo (read path).
	if listOut := env.RunCLI("checkpoint", "list"); !strings.Contains(listOut, checkpointID) {
		t.Fatalf("checkpoint list missing checkpoint %s:\n%s", checkpointID, listOut)
	}
}

// TestReftableRepository_GitRefsBackend_LinkedWorktree verifies that first-run
// enable with the default git-refs backend succeeds inside a linked worktree of a
// reftable repository (shared reftable stack under the common git dir) and that
// the reftable read paths work from the worktree. The git-branch variant is
// covered by TestReftableRepository_LinkedWorktree.
func TestReftableRepository_GitRefsBackend_LinkedWorktree(t *testing.T) {
	t.Parallel()
	requireGitReftableSupport(t)

	env := NewTestEnv(t)
	bootstrapReftableRepo(t, env)

	worktreePath := filepath.Join(t.TempDir(), "wt")
	gitOutput(t, env.RepoDir, "worktree", "add", "-b", "feature/wt", worktreePath)

	// Default backend (git-refs): no --checkpoint-backend flag.
	runCLIIn(t, env, worktreePath, "enable", "--no-github", "--agent", "claude-code", "--telemetry=false")

	if s := readWorktreeFile(t, worktreePath, ".entire/settings.json"); !strings.Contains(s, `"git-refs"`) {
		t.Fatalf("enable in a reftable worktree should default to git-refs, settings.json:\n%s", s)
	}
	if got := gitOutput(t, worktreePath, "rev-parse", "--show-ref-format"); got != refFormatReftable {
		t.Fatalf("worktree ref format = %q, want reftable", got)
	}

	// A ref read against the worktree (HEAD resolution through the reftable storer,
	// whose HEAD stub go-git cannot read) must return the real branch.
	if branch := gitOutput(t, worktreePath, "rev-parse", "--abbrev-ref", "HEAD"); branch != "feature/wt" {
		t.Fatalf("worktree branch = %q, want feature/wt", branch)
	}

	// The git-refs default must not bootstrap the v1 branch.
	if got := gitOutput(t, worktreePath, "for-each-ref", checkpointRefPrefix); got != "" {
		t.Fatalf("no checkpoint should exist yet, got refs:\n%s", got)
	}
}

// bootstrapReftableRepo initializes env.RepoDir as a reftable repository with an
// initial commit via the git CLI. Integration tests deliberately avoid the enable
// bootstrap path and drive hooks through getTestBinary(), so the repo is created
// directly here.
func bootstrapReftableRepo(t *testing.T, env *TestEnv) {
	t.Helper()
	gitOutput(t, "", "init", "--ref-format=reftable", env.RepoDir)
	gitOutput(t, env.RepoDir, "config", "user.name", "Test User")
	gitOutput(t, env.RepoDir, "config", "user.email", "test@example.com")
	gitOutput(t, env.RepoDir, "config", "commit.gpgsign", "false")
	env.WriteFile("README.md", "# reftable repo\n")
	gitOutput(t, env.RepoDir, "add", "README.md")
	gitOutput(t, env.RepoDir, "commit", "-m", "Initial reftable commit")
}

// readWorktreeFile reads a file relative to a linked worktree root. env.ReadFile
// is scoped to env.RepoDir, so worktree-local files (e.g. a per-worktree
// .entire/settings.json) need a direct read.
func readWorktreeFile(t *testing.T, worktreePath, rel string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(worktreePath, rel))
	if err != nil {
		t.Fatalf("read %s in worktree: %v", rel, err)
	}
	return string(data)
}

// runCLIIn runs the built entire binary in an arbitrary directory (e.g. a linked
// worktree) with the same isolated environment RunCLI uses, detached from any
// controlling TTY (matching TestEnv.RunCLIWithError) so an interactive prompt
// path can't hang the test.
func runCLIIn(t *testing.T, env *TestEnv, dir string, args ...string) {
	t.Helper()
	cmd := execx.NonInteractive(context.Background(), getTestBinary(), args...)
	cmd.Dir = dir
	cmd.Env = env.cliEnv()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("entire %s (in %s) failed: %v\n%s", strings.Join(args, " "), dir, err, out)
	}
}

func requireGitReftableSupport(t *testing.T) {
	t.Helper()

	dir := t.TempDir()
	cmd := exec.Command("git", "init", "--ref-format=reftable", dir) //nolint:noctx // test capability probe
	cmd.Env = testutil.GitIsolatedEnv()
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("git does not support reftable repositories: %v\n%s", err, output)
	}
	if got := gitOutput(t, dir, "rev-parse", "--show-ref-format"); got != refFormatReftable {
		t.Skipf("git initialized ref format %q, not reftable", got)
	}
}
