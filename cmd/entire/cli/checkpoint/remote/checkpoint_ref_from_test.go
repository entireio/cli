package remote

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os/exec"
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
