//go:build integration

package integration

import (
	"crypto/x509"
	"encoding/pem"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-git/go-billy/v6/osfs"
	"github.com/go-git/go-git/v6/backend"
	"github.com/go-git/go-git/v6/plumbing/transport"

	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// httpGitServer is an in-process HTTPS git server backed by bare repos on disk.
// It uses go-git's backend.Backend to serve smart HTTP protocol, including
// git-receive-pack (push) which requires a non-empty Authorization header.
type httpGitServer struct {
	URL        string            // e.g., "https://127.0.0.1:PORT"
	CACertFile string            // PEM file path for GIT_SSL_CAINFO
	BareDirs   map[string]string // repo path (e.g., "testorg/main-repo") -> bare dir on filesystem
}

// tokenEnv returns env vars for authenticated HTTPS git operations.
func (s *httpGitServer) tokenEnv(token string) []string {
	return []string{
		"ENTIRE_CHECKPOINT_TOKEN=" + token,
		"GIT_SSL_CAINFO=" + s.CACertFile,
	}
}

// sslEnv returns env vars for HTTPS git operations without token auth.
func (s *httpGitServer) sslEnv() []string {
	return []string{
		"GIT_SSL_CAINFO=" + s.CACertFile,
	}
}

// startGitHTTPSServer creates bare git repos and starts an HTTPS server that
// serves them via go-git's smart HTTP backend. Each repoName becomes a bare
// repo at <tempDir>/<repoName>.git/ on disk and is accessible at
// <serverURL>/<repoName>.git over HTTPS.
//
// The server's TLS certificate is exported to a PEM file so git can trust it
// via GIT_SSL_CAINFO. The server is automatically shut down when the test ends.
func startGitHTTPSServer(t *testing.T, repoNames ...string) *httpGitServer {
	t.Helper()

	baseDir := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(baseDir); err == nil {
		baseDir = resolved
	}

	bareDirs := make(map[string]string, len(repoNames))
	for _, name := range repoNames {
		bareDir := filepath.Join(baseDir, name+".git")
		if err := os.MkdirAll(bareDir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", bareDir, err)
		}
		cmd := exec.CommandContext(t.Context(), "git", "init", "--bare")
		cmd.Dir = bareDir
		cmd.Env = testutil.GitIsolatedEnv()
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git init --bare %s: %v\n%s", bareDir, err, output)
		}
		bareDirs[name] = bareDir
	}

	loader := transport.NewFilesystemLoader(osfs.New(baseDir), false)
	b := backend.New(loader)

	srv := httptest.NewTLSServer(b)
	t.Cleanup(srv.Close)

	// Export the TLS certificate so git can trust it via GIT_SSL_CAINFO.
	certPEM := pem.EncodeToMemory(&pem.Block{
		Type:  "CERTIFICATE",
		Bytes: srv.TLS.Certificates[0].Certificate[0],
	})
	certFile := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(certFile, certPEM, 0o644); err != nil {
		t.Fatalf("write CA cert: %v", err)
	}

	// Sanity-check: verify the cert is parseable.
	if _, err := x509.ParseCertificate(srv.TLS.Certificates[0].Certificate[0]); err != nil {
		t.Fatalf("parse TLS cert: %v", err)
	}

	return &httpGitServer{
		URL:        srv.URL,
		CACertFile: certFile,
		BareDirs:   bareDirs,
	}
}

// seedBareRepo adds "origin" pointing at a bare repo, pushes the current HEAD
// via local file path, then switches origin to the HTTPS URL. This seeds the
// remote with initial content so subsequent HTTPS operations have a base.
func seedBareRepo(t *testing.T, env *TestEnv, bareDir, httpsOriginURL string) {
	t.Helper()
	ctx := t.Context()

	// Add origin pointing to the bare repo on disk (no auth needed).
	cmd := exec.CommandContext(ctx, "git", "remote", "add", "origin", bareDir)
	cmd.Dir = env.RepoDir
	cmd.Env = testutil.GitIsolatedEnv()
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("remote add origin: %v\n%s", err, output)
	}

	cmd = exec.CommandContext(ctx, "git", "push", "--no-verify", "-u", "origin", "HEAD")
	cmd.Dir = env.RepoDir
	cmd.Env = testutil.GitIsolatedEnv()
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("push to bare: %v\n%s", err, output)
	}

	// Switch origin to the HTTPS URL for subsequent operations.
	cmd = exec.CommandContext(ctx, "git", "remote", "set-url", "origin", httpsOriginURL)
	cmd.Dir = env.RepoDir
	cmd.Env = testutil.GitIsolatedEnv()
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("set-url to https: %v\n%s", err, output)
	}

	// Re-baseline the git config guard so the URL change isn't flagged.
	env.setGitConfigBaseline()
}

// cloneFromBareWithHTTPS clones from a bare dir (local path), initializes
// Entire, then switches origin to the HTTPS URL.
func cloneFromBareWithHTTPS(t *testing.T, env *TestEnv, bareDir, httpsOriginURL string) *TestEnv {
	t.Helper()
	clone := env.CloneFrom(bareDir)

	cmd := exec.CommandContext(t.Context(), "git", "remote", "set-url", "origin", httpsOriginURL)
	cmd.Dir = clone.RepoDir
	cmd.Env = testutil.GitIsolatedEnv()
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("set-url to https in clone: %v\n%s", err, output)
	}

	clone.setGitConfigBaseline()
	return clone
}

// listRemoteMetadataCommits returns the subject lines of all commits on the
// metadata branch of a bare repo, newest first. Uses git log directly on the
// bare directory to avoid testing the production code with itself.
func listRemoteMetadataCommits(t *testing.T, bareDir string) []string {
	t.Helper()

	cmd := exec.CommandContext(t.Context(), "git", "log", "--format=%s", "refs/heads/"+paths.MetadataBranchName)
	cmd.Dir = bareDir
	cmd.Env = testutil.GitIsolatedEnv()
	output, err := cmd.Output()
	if err != nil {
		t.Fatalf("git log on bare remote failed: %v", err)
	}

	var subjects []string
	for _, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		if line != "" {
			subjects = append(subjects, line)
		}
	}
	return subjects
}

// assertRemoteHasCheckpointCommit checks that the metadata branch on the bare
// repo contains a commit whose subject starts with "Checkpoint: <id>".
func assertRemoteHasCheckpointCommit(t *testing.T, bareDir, checkpointID string) {
	t.Helper()

	want := "Checkpoint: " + checkpointID
	for _, subj := range listRemoteMetadataCommits(t, bareDir) {
		if subj == want {
			return
		}
	}
	t.Errorf("remote metadata branch should have commit %q", want)
}

// =============================================================================
// HTTPS Remote Tests
// =============================================================================

// TestHTTPS_PushCheckpointBranchToRemote verifies that PrePush pushes the
// checkpoint branch to an HTTPS remote when ENTIRE_CHECKPOINT_TOKEN is set.
// This exercises:
//   - remote.newCommand HTTPS protocol detection and token injection
//   - tryPushRefCommon over HTTPS with Authorization header
//   - go-git backend.requireReceivePackAuth validates the header
func TestHTTPS_PushCheckpointBranchToRemote(t *testing.T) {
	t.Parallel()

	ForEachBackend(t, func(t *testing.T, backend string) {
		srv := startGitHTTPSServer(t, "testorg/main-repo")
		env := NewFeatureBranchEnv(t)
		env.CheckpointStore = backend

		httpsURL := srv.URL + "/testorg/main-repo.git"
		seedBareRepo(t, env, srv.BareDirs["testorg/main-repo"], httpsURL)

		env.ExtraEnv = srv.tokenEnv("test-push-token")

		checkpointID := createCheckpointedCommit(t, env, "Add feature", "feature.go", "package feature", "Add feature")

		if !env.CheckpointsPresentLocally() {
			t.Fatal("checkpoints should exist locally after condensation")
		}

		env.RunPrePush("origin")

		bareDir := srv.BareDirs["testorg/main-repo"]
		if !env.CheckpointsPresentOnRemote(bareDir) {
			t.Fatal("checkpoints should exist on HTTPS remote after PrePush")
		}

		if checkpointID == "" {
			t.Fatal("should have a checkpoint ID after condensation")
		}
		if !env.CheckpointExistsOnRemote(bareDir, checkpointID) {
			t.Errorf("checkpoint %s should exist on HTTPS remote", checkpointID)
		}

		if !env.usingGitRefs() {
			// 2 commits: "Initialize metadata branch" + "Checkpoint: <id>"
			commits := listRemoteMetadataCommits(t, bareDir)
			if len(commits) != 2 {
				t.Fatalf("expected 2 commits on remote metadata branch, got %d: %v", len(commits), commits)
			}
			assertRemoteHasCheckpointCommit(t, bareDir, checkpointID)
		}
	})
}

// TestHTTPS_CheckpointRemoteRoutesToSeparateRepo verifies that when
// checkpoint_remote is configured, both push AND fetch of the checkpoint
// branch route to the configured repo (not origin).
//
// The test uses two clones that push independently. The second clone triggers
// a non-fast-forward, which forces a fetch+rebase from the checkpoint remote
// before retrying the push. If either fetch or push were misrouted to origin,
// the test would fail because origin never has the checkpoint branch.
//
// Code paths exercised:
//   - resolvePushSettings -> PushURL -> deriveCheckpointURLFromInfo (push routing)
//   - fetchAndRebaseRefCommon with checkpoint URL target (fetch routing)
//   - tryPushRefCommon retry after rebase (push retry)
//
// git-branch only: asserts on v1 commit counts/subjects and the rebased tip's
// parent count. checkpoint_remote routing and non-FF rebase for git-refs
// per-checkpoint refs are separate future work (test plan B5/D2, git-refs only).
func TestHTTPS_CheckpointRemoteRoutesToSeparateRepo(t *testing.T) {
	t.Parallel()

	srv := startGitHTTPSServer(t, "testorg/main-repo", "testorg/checkpoints")
	env := NewFeatureBranchEnv(t)

	mainBare := srv.BareDirs["testorg/main-repo"]
	checkpointBare := srv.BareDirs["testorg/checkpoints"]
	httpsURL := srv.URL + "/testorg/main-repo.git"
	seedBareRepo(t, env, mainBare, httpsURL)

	checkpointRemoteSettings := map[string]any{
		"strategy_options": map[string]any{
			"checkpoint_remote": map[string]any{
				"provider": "github",
				"repo":     "testorg/checkpoints",
			},
		},
	}

	// Clone A
	cloneA := cloneFromBareWithHTTPS(t, env, mainBare, httpsURL)
	cloneA.ExtraEnv = srv.tokenEnv("clone-a-token")
	cloneA.GitCheckoutNewBranch("feature/clone-a")
	cloneA.PatchSettings(checkpointRemoteSettings)

	// Clone B
	cloneB := cloneFromBareWithHTTPS(t, env, mainBare, httpsURL)
	cloneB.ExtraEnv = srv.tokenEnv("clone-b-token")
	cloneB.GitCheckoutNewBranch("feature/clone-b")
	cloneB.PatchSettings(checkpointRemoteSettings)

	// Both create checkpoints independently.
	checkpointA := createCheckpointedCommit(t, cloneA, "Work in clone A", "a.go", "package a", "Work from A")
	t.Logf("Clone A checkpoint: %s", checkpointA)

	checkpointB := createCheckpointedCommit(t, cloneB, "Work in clone B", "b.go", "package b", "Work from B")
	t.Logf("Clone B checkpoint: %s", checkpointB)

	// A pushes first — checkpoint lands on the checkpoint remote.
	cloneA.RunPrePush("origin")

	if !cloneA.BranchExistsOnRemote(checkpointBare, paths.MetadataBranchName) {
		t.Fatal("clone A: checkpoint branch should be on checkpoint remote after push")
	}

	// B pushes second — gets non-fast-forward from the checkpoint remote,
	// must fetch from checkpoint remote (not origin) to rebase, then retry.
	cloneB.RunPrePush("origin")

	// Both checkpoints should be on the checkpoint remote.
	summaryA := CheckpointSummaryPath(checkpointA)
	if !fileExistsOnRemoteBranch(t, checkpointBare, summaryA) {
		t.Errorf("checkpoint remote should have checkpoint A: %s", checkpointA)
	}

	summaryB := CheckpointSummaryPath(checkpointB)
	if !fileExistsOnRemoteBranch(t, checkpointBare, summaryB) {
		t.Errorf("checkpoint remote should have checkpoint B: %s", checkpointB)
	}

	// Origin should never have the checkpoint branch — all routing went to
	// the checkpoint remote.
	if cloneA.BranchExistsOnRemote(mainBare, paths.MetadataBranchName) {
		t.Error("checkpoint branch should NOT be on origin when routed to checkpoint remote")
	}

	// Clone B's metadata tip should have 1 parent (rebased, not merged),
	// confirming the fetch+rebase recovery path worked over HTTPS.
	parentCount := cloneB.GetBranchTipParentCount(paths.MetadataBranchName)
	if parentCount != 1 {
		t.Errorf("clone B metadata tip should have 1 parent (rebased), got %d", parentCount)
	}

	// 3 commits: "Initialize metadata branch" + 2x "Checkpoint: <id>"
	commits := listRemoteMetadataCommits(t, checkpointBare)
	if len(commits) != 3 {
		t.Fatalf("expected 3 commits on checkpoint remote metadata branch, got %d: %v", len(commits), commits)
	}
	assertRemoteHasCheckpointCommit(t, checkpointBare, checkpointA)
	assertRemoteHasCheckpointCommit(t, checkpointBare, checkpointB)
}

// TestHTTPS_OutOfSyncCheckpointBranchRebases verifies that when two clones
// push to the same HTTPS remote, the second pusher fetches, rebases its local
// checkpoint branch, and retries the push successfully.
//
// git-branch only: asserts on v1 commit counts and the rebased tip's parent
// count. The git-refs non-FF fetch+replay+retry equivalent is separate future
// work (test plan D2/D3, git-refs only).
func TestHTTPS_OutOfSyncCheckpointBranchRebases(t *testing.T) {
	t.Parallel()

	srv := startGitHTTPSServer(t, "testorg/main-repo")
	env := NewFeatureBranchEnv(t)

	bareDir := srv.BareDirs["testorg/main-repo"]
	httpsURL := srv.URL + "/testorg/main-repo.git"
	seedBareRepo(t, env, bareDir, httpsURL)

	// Clone A
	cloneA := cloneFromBareWithHTTPS(t, env, bareDir, httpsURL)
	cloneA.ExtraEnv = srv.tokenEnv("clone-a-token")
	cloneA.GitCheckoutNewBranch("feature/clone-a")

	// Clone B
	cloneB := cloneFromBareWithHTTPS(t, env, bareDir, httpsURL)
	cloneB.ExtraEnv = srv.tokenEnv("clone-b-token")
	cloneB.GitCheckoutNewBranch("feature/clone-b")

	// Both create checkpoints independently.
	checkpointA := createCheckpointedCommit(t, cloneA, "Work in clone A", "a.go", "package a", "Work from A")
	t.Logf("Clone A checkpoint: %s", checkpointA)

	checkpointB := createCheckpointedCommit(t, cloneB, "Work in clone B", "b.go", "package b", "Work from B")
	t.Logf("Clone B checkpoint: %s", checkpointB)

	// A pushes first (succeeds cleanly).
	cloneA.RunPrePush("origin")

	// B pushes second (non-fast-forward -> fetch+rebase+retry over HTTPS).
	cloneB.RunPrePush("origin")

	// Both checkpoints should be on the remote.
	summaryA := CheckpointSummaryPath(checkpointA)
	if !fileExistsOnRemoteBranch(t, bareDir, summaryA) {
		t.Errorf("remote should have checkpoint from clone A: %s", checkpointA)
	}

	summaryB := CheckpointSummaryPath(checkpointB)
	if !fileExistsOnRemoteBranch(t, bareDir, summaryB) {
		t.Errorf("remote should have checkpoint from clone B: %s", checkpointB)
	}

	// Clone B's metadata branch tip should have exactly 1 parent (linear
	// rebase, not a merge commit). This confirms the fetch+rebase path.
	parentCount := cloneB.GetBranchTipParentCount(paths.MetadataBranchName)
	if parentCount != 1 {
		t.Errorf("clone B metadata branch tip should have 1 parent (rebased), got %d", parentCount)
	}

	// 3 commits: "Initialize metadata branch" + 2x "Checkpoint: <id>"
	commits := listRemoteMetadataCommits(t, bareDir)
	if len(commits) != 3 {
		t.Fatalf("expected 3 commits on remote metadata branch, got %d: %v", len(commits), commits)
	}
	assertRemoteHasCheckpointCommit(t, bareDir, checkpointA)
	assertRemoteHasCheckpointCommit(t, bareDir, checkpointB)
}

// TestHTTPS_PushFailsWithoutToken verifies that the go-git HTTPS backend
// rejects pushes without an Authorization header, and that setting
// ENTIRE_CHECKPOINT_TOKEN makes the push succeed.
func TestHTTPS_PushFailsWithoutToken(t *testing.T) {
	t.Parallel()

	ForEachBackend(t, func(t *testing.T, backend string) {
		srv := startGitHTTPSServer(t, "testorg/main-repo")
		env := NewFeatureBranchEnv(t)
		env.CheckpointStore = backend

		bareDir := srv.BareDirs["testorg/main-repo"]
		httpsURL := srv.URL + "/testorg/main-repo.git"
		seedBareRepo(t, env, bareDir, httpsURL)

		// SSL trust only — no token.
		env.ExtraEnv = srv.sslEnv()

		checkpointID := createCheckpointedCommit(t, env, "Add service", "service.go", "package service", "Add service")

		if !env.CheckpointsPresentLocally() {
			t.Fatal("checkpoints should exist locally after condensation")
		}

		// Push without token — the server returns 401 for receive-pack. PrePush
		// degrades gracefully (returns nil, logs a warning).
		env.RunPrePush("origin")

		if env.CheckpointsPresentOnRemote(bareDir) {
			t.Error("checkpoints should NOT be on remote without token (401 expected)")
		}

		// Now set the token and push again — should succeed.
		env.ExtraEnv = srv.tokenEnv("valid-token")
		env.RunPrePush("origin")

		if !env.CheckpointsPresentOnRemote(bareDir) {
			t.Fatal("checkpoints should be on remote after push with token")
		}
		if !env.CheckpointExistsOnRemote(bareDir, checkpointID) {
			t.Errorf("checkpoint %s should exist on HTTPS remote after token push", checkpointID)
		}

		if !env.usingGitRefs() {
			// 2 commits: "Initialize metadata branch" + "Checkpoint: <id>"
			commits := listRemoteMetadataCommits(t, bareDir)
			if len(commits) != 2 {
				t.Fatalf("expected 2 commits on remote metadata branch, got %d: %v", len(commits), commits)
			}
			assertRemoteHasCheckpointCommit(t, bareDir, checkpointID)
		}
	})
}

// =============================================================================
// C. Cross-machine clone → fetch (over HTTPS with token)
// =============================================================================

// TestHTTPS_FetchMetadataBranchIfMissingOnPrePush is test C1: the production
// fetchMetadataBranchIfMissing path, exercised for real. A clone that lacks the
// local v1 branch resolves its pre-push settings; because a checkpoint_remote is
// configured, resolvePushSettings fetches the missing v1 branch from the remote
// before pushing — so the local branch appears without any manual `git fetch`.
//
// It runs over HTTPS because a local-path origin can't derive a checkpoint URL
// (ParseURL fails and derivation falls back to origin without fetching). git-branch
// only: it asserts on the v1 branch, which git-refs has no equivalent of.
func TestHTTPS_FetchMetadataBranchIfMissingOnPrePush(t *testing.T) {
	t.Parallel()

	srv := startGitHTTPSServer(t, "testorg/main-repo")
	env := NewFeatureBranchEnv(t)

	mainBare := srv.BareDirs["testorg/main-repo"]
	httpsURL := srv.URL + "/testorg/main-repo.git"
	seedBareRepo(t, env, mainBare, httpsURL)
	env.ExtraEnv = srv.tokenEnv("c1-token")

	// Repo A creates and pushes a checkpoint so the remote has a v1 branch.
	_ = createCheckpointedCommit(t, env, "Add module", "mod.go", "package mod", "Add module")
	env.RunPrePush("origin")
	if !env.BranchExistsOnRemote(mainBare, paths.MetadataBranchName) {
		t.Fatal("v1 branch should be on the remote after repo A's push")
	}

	// Clone over HTTPS. A plain clone leaves v1 only as a remote-tracking ref, not
	// a local branch.
	clone := cloneFromBareWithHTTPS(t, env, mainBare, httpsURL)
	clone.ExtraEnv = srv.tokenEnv("c1-token")
	clone.PatchSettings(map[string]any{
		"strategy_options": map[string]any{
			"checkpoint_remote": map[string]any{
				"provider": "github",
				"repo":     "testorg/main-repo",
			},
		},
	})
	if clone.BranchExists(paths.MetadataBranchName) {
		t.Fatal("clone should not have a local v1 branch before pre-push settings resolution")
	}

	// Resolving pre-push settings triggers fetchMetadataBranchIfMissing, which
	// fetches v1 from the checkpoint remote — no manual git fetch.
	clone.RunPrePush("origin")

	if !clone.BranchExists(paths.MetadataBranchName) {
		t.Error("local v1 branch should appear after pre-push settings resolution fetched it (fetchMetadataBranchIfMissing)")
	}
}

// TestHTTPS_CloneReadAutoFetchesWithToken is test C4: reading a checkpoint from a
// fresh clone over an authenticated HTTPS remote auto-fetches the checkpoint data
// with the token. The git-branch path fetches the v1 branch on miss; the git-refs
// path fetches exactly the per-checkpoint ref via the on-demand RefFetcher. Both
// require ENTIRE_CHECKPOINT_TOKEN to reach the server.
func TestHTTPS_CloneReadAutoFetchesWithToken(t *testing.T) {
	t.Parallel()

	ForEachBackend(t, func(t *testing.T, backend string) {
		srv := startGitHTTPSServer(t, "testorg/main-repo")
		env := NewFeatureBranchEnv(t)
		env.CheckpointStore = backend

		mainBare := srv.BareDirs["testorg/main-repo"]
		httpsURL := srv.URL + "/testorg/main-repo.git"
		seedBareRepo(t, env, mainBare, httpsURL)
		env.ExtraEnv = srv.tokenEnv("c4-token")

		checkpointID := createCheckpointedCommit(t, env, "Add remote feature", "feat.go", "package feat", "Add remote feature")
		if checkpointID == "" {
			t.Fatal("should have a checkpoint ID after condensation")
		}
		// Push checkpoints into the server (token-authenticated CLI push).
		env.RunPrePush("origin")
		if !env.CheckpointExistsOnRemote(mainBare, checkpointID) {
			t.Fatal("checkpoint should be on the HTTPS remote after push")
		}

		clone := cloneFromBareWithHTTPS(t, env, mainBare, httpsURL)
		clone.ExtraEnv = srv.tokenEnv("c4-token")
		if clone.CheckpointsPresentLocally() {
			t.Fatalf("[%s] clone should not have the checkpoint locally before a read", backend)
		}

		// Reading the checkpoint auto-fetches it from the authenticated remote.
		out := clone.RunCLI("checkpoint", "explain", "--checkpoint", checkpointID)
		if !strings.Contains(out, "Add remote feature") {
			t.Errorf("[%s] explain should surface the prompt after auto-fetch, got:\n%s", backend, out)
		}

		// git-refs lands exactly the per-checkpoint ref locally after the read.
		if clone.usingGitRefs() && !refExists(t, clone.RepoDir, checkpointRefName(checkpointID)) {
			t.Errorf("git-refs: checkpoint ref should be fetched locally after the authenticated read")
		}
	})
}
