package remote

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/go-git/go-git/v6/plumbing"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// When both remotes contain the ref, they point at different commits so tests
// can identify which candidate served the fetch.
func candidatesFixture(t *testing.T, refOnUpstream, refOnOrigin bool) (workDir string, ref plumbing.ReferenceName, upstreamHash, originHash string) {
	t.Helper()
	testutil.IsolateGitConfigEnv(t)

	upstreamBare := t.TempDir()
	originBare := t.TempDir()
	for _, bare := range []string{upstreamBare, originBare} {
		out, err := exec.CommandContext(t.Context(), "git", "init", "--bare", bare).CombinedOutput()
		require.NoError(t, err, "git init --bare: %s", out)
	}

	workDir = t.TempDir()
	testutil.InitRepo(t, workDir)
	testutil.WriteFile(t, workDir, "f.txt", "one")
	testutil.GitAdd(t, workDir, "f.txt")
	testutil.GitCommit(t, workDir, "one")
	firstCommit := candidateRevParse(t, workDir, "HEAD")
	testutil.WriteFile(t, workDir, "g.txt", "two")
	testutil.GitAdd(t, workDir, "g.txt")
	testutil.GitCommit(t, workDir, "two")
	secondCommit := candidateRevParse(t, workDir, "HEAD")

	testutil.AddRemote(t, workDir, "upstream", upstreamBare)
	testutil.AddRemote(t, workDir, "origin", originBare)

	ref = plumbing.ReferenceName("refs/entire/checkpoints/Z9/01KVBJCWYA4YW6J5M9GP655HZ9")
	if refOnUpstream {
		upstreamHash = secondCommit
		pushRefTo(t, workDir, "upstream", secondCommit, ref)
	}
	if refOnOrigin {
		originHash = firstCommit
		pushRefTo(t, workDir, "origin", firstCommit, ref)
	}

	t.Chdir(workDir)
	return workDir, ref, upstreamHash, originHash
}

func pushRefTo(t *testing.T, workDir, remoteName, hash string, ref plumbing.ReferenceName) {
	t.Helper()
	out, err := exec.CommandContext(t.Context(), "git", "-C", workDir, "push", "--quiet", remoteName, hash+":"+ref.String()).CombinedOutput()
	require.NoError(t, err, "git push checkpoint ref: %s", out)
}

func candidateRevParse(t *testing.T, dir, rev string) string {
	t.Helper()
	out, err := exec.CommandContext(t.Context(), "git", "-C", dir, "rev-parse", rev).Output()
	require.NoError(t, err, "git rev-parse %s", rev)
	return strings.TrimSpace(string(out))
}

func localRefHash(t *testing.T, dir string, ref plumbing.ReferenceName) string {
	t.Helper()
	return candidateRevParse(t, dir, ref.String())
}

func TestFetchCheckpointRefFrom_FirstCandidateWins(t *testing.T) {
	workDir, ref, upstreamHash, originHash := candidatesFixture(t, true, true)

	require.NoError(t, FetchCheckpointRefFrom(context.Background(), ref, []string{"upstream", "origin"}, nil))

	got := localRefHash(t, workDir, ref)
	require.Equal(t, upstreamHash, got, "the first candidate must serve the fetch")
	require.NotEqual(t, originHash, got)
}

// Regression (PR #1951 review): this fetch INSTALLS the canonical local
// checkpoint ref (+ref:ref), and checkpoint refs advance (backfill parents
// onto the tip). When the elected remote — the only remote writes confine to,
// hence the holder of the newest tip — fails at the transport level, falling
// through to the legacy origin tier could install an OLDER tip as canonical.
// Nothing ever corrects it (hydration only runs when the local ref is
// absent), and later backfills parent onto the stale tip. Transport
// uncertainty on an earlier candidate must therefore abort the chain — only
// authoritative absence lets a later candidate install. The hang is still
// bounded by the per-candidate timeout: the caller gets an error, not a
// stall.
func TestFetchCheckpointRefFrom_ElectedTransportFailureRefusesLegacyInstall(t *testing.T) {
	workDir, ref, _, _ := candidatesFixture(t, false, true)

	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) {
		<-request.Context().Done()
	}))
	t.Cleanup(server.Close)
	out, err := exec.CommandContext(t.Context(), "git", "-C", workDir, "remote", "set-url", "upstream", server.URL+"/repo.git").CombinedOutput()
	require.NoError(t, err, "%s", out)

	start := time.Now()
	fetchErr := fetchCheckpointRefFrom(context.Background(), ref, []string{"upstream", "origin"}, time.Second, 10*time.Second, nil)
	require.Error(t, fetchErr, "an unreachable elected remote must surface, not fall through to a legacy install")
	require.NotErrorIs(t, fetchErr, plumbing.ErrReferenceNotFound)
	require.Less(t, time.Since(start), 10*time.Second, "the per-candidate timeout must still bound the hang")

	_, revErr := exec.CommandContext(t.Context(), "git", "-C", workDir, "rev-parse", "--verify", ref.String()).Output()
	require.Error(t, revErr, "origin's (potentially stale) tip must not be installed as the canonical local ref under elected-remote uncertainty")
}

// The healthy-absence shape of the same chain: the elected remote answers
// authoritatively that it lacks the ref, so the legacy origin tier may serve
// and install it — the pre-#1893 checkpoint that only ever landed on origin.
func TestFetchCheckpointRefFrom_ElectedAbsenceLetsLegacyServe(t *testing.T) {
	workDir, ref, _, originHash := candidatesFixture(t, false, true)

	require.NoError(t, FetchCheckpointRefFrom(context.Background(), ref, []string{"upstream", "origin"}, nil))
	require.Equal(t, originHash, localRefHash(t, workDir, ref))
}

// Any unresolved transport failure makes aggregate absence uncertain,
// regardless of which candidate failed first.
func TestFetchCheckpointRefFrom_AllFailSurfacesTransportError(t *testing.T) {
	tests := []struct {
		name         string
		brokenRemote string
	}{
		{name: "first candidate transport failure", brokenRemote: "upstream"},
		{name: "later candidate transport failure", brokenRemote: "origin"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			workDir, ref, _, _ := candidatesFixture(t, false, false)
			out, err := exec.CommandContext(t.Context(), "git", "-C", workDir, "remote", "set-url", tt.brokenRemote, workDir+"/nonexistent-remote").CombinedOutput()
			require.NoError(t, err, "%s", out)

			err = FetchCheckpointRefFrom(context.Background(), ref, []string{"upstream", "origin"}, nil)
			require.Error(t, err)
			require.NotErrorIs(t, err, plumbing.ErrReferenceNotFound,
				"a transport failure must not be masked by another candidate's absence")
			require.Contains(t, err.Error(), "probe checkpoint ref")
		})
	}
}

func TestFetchCheckpointRefFrom_AbsentOnEveryCandidateIsAbsence(t *testing.T) {
	_, ref, _, _ := candidatesFixture(t, false, false)

	err := FetchCheckpointRefFrom(context.Background(), ref, []string{"upstream", "origin"}, nil)
	require.Error(t, err)
	require.ErrorIs(t, err, plumbing.ErrReferenceNotFound)
}

func TestFetchCheckpointRefFrom_DedicatedCheckpointRemoteBypassesCandidates(t *testing.T) {
	workDir, ref, _, _ := candidatesFixture(t, true, false)
	testutil.WriteFile(t, workDir, ".entire/settings.json",
		`{"enabled": true, "strategy_options": {"checkpoint_remote": null}}`)

	err := FetchCheckpointRefFrom(context.Background(), ref, []string{"upstream", "origin"}, nil)
	require.Error(t, err)
	require.NotErrorIs(t, err, plumbing.ErrReferenceNotFound,
		"a checkpoint_remote key must keep dedicated-store semantics even when malformed")
}

// Forge identities must survive get-url so ownership checks see the real
// topology. Only transport arguments are mapped to isolated bare repositories.
func dedicatedCandidatesFixture(t *testing.T, refOnFork, refOnOrigin bool) (string, plumbing.ReferenceName, string, string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("transport mapping uses a bash git wrapper")
	}
	workDir, ref, forkHash, originHash := candidatesFixture(t, refOnFork, refOnOrigin)
	realGit, err := exec.LookPath("git")
	require.NoError(t, err)
	for _, remoteName := range []string{"upstream", "origin"} {
		out, err := exec.CommandContext(t.Context(), realGit, "remote", "get-url", remoteName).Output()
		require.NoError(t, err)
		t.Setenv("CHECKPOINT_TEST_"+strings.ToUpper(remoteName), strings.TrimSpace(string(out)))
	}
	testutil.RunGit(t, workDir, "remote", "rename", "upstream", "fork")
	testutil.RunGit(t, workDir, "remote", "set-url", "fork", "https://github.com/contributor/app.git")
	testutil.RunGit(t, workDir, "remote", "set-url", "origin", "https://github.com/acme/app.git")
	testutil.WriteFile(t, workDir, ".entire/settings.json", `{"enabled":true,"strategy_options":{"checkpoint_remote":{"provider":"github","repo":"acme/checkpoints"},"checkpoint_push_remote":"fork"}}`)
	t.Setenv(testutil.GitTransportRealGitEnv, realGit)
	t.Setenv("CHECKPOINT_TEST_DEDICATED", filepath.Join(workDir, "missing-dedicated"))
	t.Setenv("GIT_ALLOW_PROTOCOL", "file")
	t.Setenv(CheckpointTokenEnvVar, "")
	// In-process, so the shim is reached through PATH and the variables it
	// dereferences are this process's own — which is what lets a subtest
	// repoint CHECKPOINT_TEST_UPSTREAM at a missing repository afterwards.
	binDir := testutil.GitTransportShim(t, []string{"ls-remote", "fetch"}, map[string]string{
		"https://github.com/contributor/app.git":  "CHECKPOINT_TEST_UPSTREAM",
		"https://github.com/acme/app.git":         "CHECKPOINT_TEST_ORIGIN",
		"https://github.com/acme/checkpoints.git": "CHECKPOINT_TEST_DEDICATED",
	})
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return workDir, ref, forkHash, originHash
}

func TestFetchCheckpointRefFrom_InheritedDedicatedUsesLeadCandidate(t *testing.T) {
	workDir, ref, forkHash, _ := dedicatedCandidatesFixture(t, true, false)
	require.NoError(t, FetchCheckpointRefFrom(t.Context(), ref, []string{"fork", "origin"}, nil))
	require.Equal(t, forkHash, localRefHash(t, workDir, ref))
}

func TestFetchCheckpointRefFrom_AcceptedDedicatedRemainsAuthoritative(t *testing.T) {
	for _, localOverride := range []bool{false, true} {
		name := "same owner"
		if localOverride {
			name = "explicit local override"
		}
		t.Run(name, func(t *testing.T) {
			workDir, ref, forkHash, dedicatedHash := dedicatedCandidatesFixture(t, true, true)
			t.Setenv("CHECKPOINT_TEST_DEDICATED", os.Getenv("CHECKPOINT_TEST_ORIGIN"))
			if localOverride {
				testutil.WriteFile(t, workDir, ".entire/settings.local.json", `{"strategy_options":{"checkpoint_remote":{"provider":"github","repo":"acme/checkpoints"}}}`)
			} else {
				testutil.RunGit(t, workDir, "remote", "set-url", "fork", "https://github.com/acme/fork.git")
			}
			require.NoError(t, FetchCheckpointRefFrom(t.Context(), ref, []string{"fork", "origin"}, nil))
			require.Equal(t, dedicatedHash, localRefHash(t, workDir, ref))
			require.NotEqual(t, forkHash, localRefHash(t, workDir, ref))

			missing := plumbing.ReferenceName("refs/entire/checkpoints/00/missing")
			require.ErrorIs(t, FetchCheckpointRefFrom(t.Context(), missing, []string{"fork", "origin"}, nil), plumbing.ErrReferenceNotFound)
		})
	}
}

func TestFetchCheckpointRefFrom_InheritedDedicatedDoesNotRetry(t *testing.T) {
	for _, transportFailure := range []bool{false, true} {
		name := "missing ref is not authoritative absence"
		if transportFailure {
			name = "selected transport failure"
		}
		t.Run(name, func(t *testing.T) {
			workDir, ref, _, _ := dedicatedCandidatesFixture(t, false, true)
			if transportFailure {
				t.Setenv("CHECKPOINT_TEST_UPSTREAM", filepath.Join(workDir, "unreachable-fork"))
			}
			err := FetchCheckpointRefFrom(t.Context(), ref, []string{"fork", "origin"}, nil)
			require.Error(t, err)
			require.NotErrorIs(t, err, plumbing.ErrReferenceNotFound)
			// Names the remote the error came from. Without this the
			// assertions above pass on unfixed code: the lead-less ownership
			// vote ACCEPTS the dedicated store (origin and the checkpoint
			// repo share an owner), so the probe fails against the store
			// instead of the fork and produces an equally non-absence error.
			require.ErrorContains(t, err, "contributor/app")
			_, err = exec.CommandContext(t.Context(), "git", "rev-parse", "--verify", ref.String()).Output()
			require.Error(t, err, "origin must not install a ref after the selected fallback fails")
		})
	}
}

func TestFetchCheckpointRefFrom_DedicatedWithoutElectedLeadKeepsLegacyTarget(t *testing.T) {
	for _, tt := range []struct {
		name       string
		candidates []string
		election   error
	}{
		{name: "failed election", candidates: []string{"fork", "origin"}, election: errors.New("election failed")},
		{name: "empty candidates"},
		{name: "empty first candidate", candidates: []string{"", "fork", "origin"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			workDir, ref, _, dedicatedHash := dedicatedCandidatesFixture(t, true, true)
			t.Setenv("CHECKPOINT_TEST_DEDICATED", os.Getenv("CHECKPOINT_TEST_ORIGIN"))
			require.NoError(t, FetchCheckpointRefFrom(t.Context(), ref, tt.candidates, tt.election))
			require.Equal(t, dedicatedHash, localRefHash(t, workDir, ref))
		})
	}
}

func TestFetchCheckpointRefFrom_InvalidSettingsKeepsLegacyTarget(t *testing.T) {
	for _, state := range []string{"malformed", "unreadable", "invalid checkpoint remote"} {
		t.Run(state, func(t *testing.T) {
			workDir, ref, _, originHash := dedicatedCandidatesFixture(t, true, true)
			switch state {
			case "malformed":
				testutil.WriteFile(t, workDir, ".entire/settings.json", "{")
			case "unreadable":
				settingsPath := filepath.Join(workDir, ".entire/settings.json")
				require.NoError(t, os.Remove(settingsPath))
				require.NoError(t, os.Mkdir(settingsPath, 0o755))
			case "invalid checkpoint remote":
				testutil.WriteFile(t, workDir, ".entire/settings.json", `{"strategy_options":{"checkpoint_remote":42}}`)
			}
			require.NoError(t, FetchCheckpointRefFrom(t.Context(), ref, []string{"fork", "origin"}, nil))
			require.Equal(t, originHash, localRefHash(t, workDir, ref))
		})
	}
}

func TestFetchCheckpointRefFrom_ConfiguredCancelledContextIsFailure(t *testing.T) {
	_, ref, _, _ := dedicatedCandidatesFixture(t, true, true)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	err := FetchCheckpointRefFrom(ctx, ref, []string{"fork", "origin"}, nil)
	require.Error(t, err)
	require.NotErrorIs(t, err, plumbing.ErrReferenceNotFound)
}

func TestFetchCheckpointRefFrom_ConfiguredHonorsFetchTimeout(t *testing.T) {
	_, ref, _, _ := dedicatedCandidatesFixture(t, true, false)
	started := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) {
		select {
		case started <- struct{}{}:
		default:
		}
		<-request.Context().Done()
	}))
	t.Cleanup(server.Close)
	t.Setenv("CHECKPOINT_TEST_UPSTREAM", server.URL+"/repo.git")
	t.Setenv("GIT_ALLOW_PROTOCOL", "file:http") // Only the mapped loopback fixture uses HTTP.

	// The parent bounds a regression without waiting for the two-minute default.
	// A correct per-fetch timeout returns while this parent is still live.
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	err := fetchCheckpointRefFrom(ctx, ref, []string{"fork", "origin"}, time.Second, ReadChainBudget, nil)
	require.Error(t, err)
	require.NotErrorIs(t, err, plumbing.ErrReferenceNotFound)
	require.NoError(t, ctx.Err(), "configured fetch must honor its shorter per-fetch timeout")
	select {
	case <-started:
	default:
		t.Fatal("fetch must reach the stalled loopback remote before timing out")
	}
}
