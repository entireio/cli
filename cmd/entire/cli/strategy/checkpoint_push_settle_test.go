package strategy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-git/go-billy/v6/osfs"
	"github.com/go-git/go-git/v6/backend"
	"github.com/go-git/go-git/v6/plumbing/transport"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

func TestGitURLIsLocal(t *testing.T) {
	t.Parallel()

	cases := map[string]bool{
		"/srv/repo.git":                   true,
		"../repo.git":                     true,
		"./repo.git":                      true,
		"repo.git":                        true,
		"/srv/a:b.git":                    true, // a slash before the colon: a path, not host:path
		"file:///srv/repo.git":            true,
		"https://github.com/o/r.git":      false,
		"http://127.0.0.1:8080/r.git":     false,
		"ssh://git@github.com/o/r.git":    false,
		"git@github.com:o/r.git":          false,
		"github.com:o/r.git":              false,
		"entire://gh/o/r":                 false,
		"persistent-https::https://h/r":   false,
		"codecommit::us-east-1://my-repo": false,
	}
	if runtime.GOOS == "windows" {
		cases[`C:\repos\r.git`] = true
		cases[`\\server\share\r.git`] = true
	}
	for url, want := range cases {
		assert.Equal(t, want, gitURLIsLocal(url), "gitURLIsLocal(%q)", url)
	}
}

// Not parallel: t.Chdir.
func TestPushTargetIsLocal(t *testing.T) {
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	bare := filepath.Join(t.TempDir(), "origin.git")
	testutil.RunGit(t, dir, "init", "--bare", bare)

	testutil.RunGit(t, dir, "remote", "add", "local", bare)
	testutil.RunGit(t, dir, "remote", "add", "forge", "https://github.com/acme/app.git")
	// The push URL decides, not the fetch URL.
	testutil.RunGit(t, dir, "remote", "add", "split", bare)
	testutil.RunGit(t, dir, "remote", "set-url", "--push", "split", "https://github.com/acme/app.git")
	// A push to every URL of a remote is local only if every one of them is.
	testutil.RunGit(t, dir, "remote", "add", "mirrors", bare)
	testutil.RunGit(t, dir, "remote", "set-url", "--add", "--push", "mirrors", bare)
	testutil.RunGit(t, dir, "remote", "set-url", "--add", "--push", "mirrors", "git@github.com:acme/app.git")
	// A remote name can contain '@' and still be a name, not an scp-like URL.
	testutil.RunGit(t, dir, "remote", "add", "team@mirror", "https://github.com/acme/app.git")
	testutil.RunGit(t, dir, "remote", "add", "team@local", bare)
	t.Chdir(dir)

	ctx := context.Background()
	assert.True(t, pushTargetIsLocal(ctx, "local"))
	assert.False(t, pushTargetIsLocal(ctx, "forge"))
	assert.False(t, pushTargetIsLocal(ctx, "split"))
	assert.False(t, pushTargetIsLocal(ctx, "mirrors"))
	assert.False(t, pushTargetIsLocal(ctx, "team@mirror"), "a configured remote is resolved whatever its name looks like")
	assert.True(t, pushTargetIsLocal(ctx, "team@local"))
	assert.True(t, pushTargetIsLocal(ctx, bare), "git push <path> pushes to that path")
	assert.False(t, pushTargetIsLocal(ctx, "https://github.com/acme/app.git"))
	assert.False(t, pushTargetIsLocal(ctx, "git@github.com:acme/app.git"))
}

// Not parallel: t.Chdir and the checkpointPushSettle var.
func TestSettleBeforeUserPush(t *testing.T) {
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	t.Chdir(dir)

	orig := checkpointPushSettle
	t.Cleanup(func() { checkpointPushSettle = orig })
	checkpointPushSettle = 150 * time.Millisecond

	start := time.Now()
	settleBeforeUserPush(context.Background(), "https://github.com/acme/app.git")
	assert.GreaterOrEqual(t, time.Since(start), checkpointPushSettle, "a hosted remote gets the full settle")

	start = time.Now()
	settleBeforeUserPush(context.Background(), filepath.Join(t.TempDir(), "origin.git"))
	assert.Less(t, time.Since(start), checkpointPushSettle, "a local remote has nothing to race")

	checkpointPushSettle = time.Minute
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start = time.Now()
	settleBeforeUserPush(ctx, "https://github.com/acme/app.git")
	assert.Less(t, time.Since(start), 10*time.Second, "an ended context stops the wait")
}

// receivePackClock serves bare repositories over smart HTTP and counts the
// pushes they receive.
type receivePackClock struct {
	mu     sync.Mutex
	pushes int
}

func (c *receivePackClock) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.pushes
}

func startReceivePackClock(t *testing.T, root string) (*receivePackClock, string) {
	t.Helper()
	c := &receivePackClock{}
	b := backend.New(transport.NewFilesystemLoader(osfs.New(root), false))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b.ServeHTTP(w, r)
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/git-receive-pack") {
			c.mu.Lock()
			c.pushes++
			c.mu.Unlock()
		}
	}))
	t.Cleanup(srv.Close)
	return c, srv.URL
}

// TestPrePush_GitBranchSettlesAfterMovingV1 is #2794: after the hook pushes
// entire/checkpoints/v1 to the user's push remote, git sends the user's own
// push at once, and GitHub rejects the later of two back-to-back pushes. The
// hook must wait before returning when, and only when, its push moved v1 on
// a remote that is not on this machine.
//
// Not parallel: t.Chdir and the waitForSettle var.
func TestPrePush_GitBranchSettlesAfterMovingV1(t *testing.T) {
	root := t.TempDir()
	testutil.RunGit(t, root, "init", "--bare", "origin.git")
	clock, serverURL := startReceivePackClock(t, root)

	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	testutil.WriteFile(t, dir, "f.txt", "init")
	testutil.GitAdd(t, dir, "f.txt")
	testutil.GitCommit(t, dir, "init")
	testutil.WriteFile(t, dir, ".entire/settings.json", `{"enabled": true}`)
	testutil.RunGit(t, dir, "remote", "add", "origin", serverURL+"/origin.git")
	// go-git's backend refuses receive-pack without an Authorization header.
	testutil.RunGit(t, dir, "config", "http.extraHeader", "Authorization: Basic eDp5")
	testutil.RunGit(t, dir, "branch", paths.MetadataBranchName)
	t.Chdir(dir)
	paths.ClearWorktreeRootCache()

	var waits []time.Duration
	origWait := waitForSettle
	t.Cleanup(func() { waitForSettle = origWait })
	waitForSettle = func(_ context.Context, d time.Duration) { waits = append(waits, d) }

	restore := captureStderr(t)
	err := NewManualCommitStrategy().PrePush(context.Background(), "origin")
	output := restore()
	require.NoError(t, err, output)
	require.Equal(t, 1, clock.count(), "the hook pushed v1 to the remote: %s", output)
	assert.Equal(t, []time.Duration{checkpointPushSettle}, waits,
		"a push that moved v1 must settle before git sends the user's push")

	// v1 is unchanged and its tracking ref matches, so this push carries no
	// checkpoint and has nothing for the user's push to land on.
	restore = captureStderr(t)
	err = NewManualCommitStrategy().PrePush(context.Background(), "origin")
	output = restore()
	require.NoError(t, err, output)
	assert.Equal(t, 1, clock.count(), "nothing new to push: %s", output)
	assert.Len(t, waits, 1, "a push that moved nothing must not delay the user's push")
}

// TestPrePush_GitBranchLocalRemoteDoesNotSettle keeps the wait off remotes on
// this machine, which is where every other test pushes.
//
// Not parallel: t.Chdir and the waitForSettle var.
func TestPrePush_GitBranchLocalRemoteDoesNotSettle(t *testing.T) {
	dir := setupRepoWithCheckpointBranch(t)
	testutil.WriteFile(t, dir, ".entire/settings.json", `{"enabled": true}`)
	bare := filepath.Join(t.TempDir(), "origin.git")
	testutil.RunGit(t, dir, "init", "--bare", bare)
	testutil.RunGit(t, dir, "remote", "add", "origin", bare)
	t.Chdir(dir)
	paths.ClearWorktreeRootCache()

	waited := false
	origWait := waitForSettle
	t.Cleanup(func() { waitForSettle = origWait })
	waitForSettle = func(context.Context, time.Duration) { waited = true }

	restore := captureStderr(t)
	err := NewManualCommitStrategy().PrePush(context.Background(), "origin")
	output := restore()
	require.NoError(t, err, output)
	testutil.RunGit(t, bare, "show-ref", "--verify", "refs/heads/"+paths.MetadataBranchName)
	assert.False(t, waited, "a local remote never races, so the push must not wait")
}
