package strategy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint/remote"
	"github.com/entireio/cli/cmd/entire/cli/gitrepo"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCheckpointHTTPAuthHint(t *testing.T) {
	t.Parallel()
	for _, err := range []error{remote.ErrHTTPAuthUnavailable, remote.ErrHTTPAuthRejected} {
		t.Run(err.Error(), func(t *testing.T) {
			t.Parallel()
			var out bytes.Buffer
			writeCheckpointHTTPAuthHint(&out, fmt.Errorf("push: %w", err))
			assert.Contains(t, out.String(), checkpointAuthHelpURL)
			assert.Contains(t, out.String(), "does not block your code push")
			assert.Contains(t, out.String(), "remain local")
			assert.Equal(t, errors.Is(err, remote.ErrHTTPAuthUnavailable), strings.Contains(out.String(), "credential helper"))
		})
	}
}

// Real Git against a local 401 server pins the subprocess boundary: the auth
// cause must survive CombinedOutput, and neither backend may fetch/replay or
// retry every ref. CWD/env/stderr changes prevent parallel execution.
func TestCheckpointHTTPAuthFailureSkipsRecovery(t *testing.T) {
	testutil.IsolateGitConfigEnv(t)
	t.Setenv("ENTIRE_CHECKPOINT_TOKEN", "")
	t.Setenv("GIT_ASKPASS", "")
	t.Setenv("SSH_ASKPASS", "")
	workDir, _, refs := setupRepoWithCheckpointRefs(t)
	t.Chdir(workDir)
	repo, err := gitrepo.OpenPath(workDir)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, repo.Close()) })
	queue := enqueueRefs(t, repo, refs)

	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.Header().Set("WWW-Authenticate", `Basic realm="test"`)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(server.Close)
	testutil.AddRemote(t, workDir, "origin", server.URL+"/repo.git")
	checkpointHTTPAuthHintOnce = sync.Once{}
	t.Cleanup(func() { checkpointHTTPAuthHintOnce = sync.Once{} })
	restore := captureStderr(t)
	ctx := context.Background()

	// Policy refresh emits the same hint, only once across all later failures.
	_, err = remote.LsRemoteInDir(ctx, workDir, "origin")
	require.ErrorIs(t, err, remote.ErrHTTPAuthUnavailable)
	warnOrLogCheckpointPolicySyncFailure(ctx, err)
	assert.EqualValues(t, 1, requests.Load())

	pushed, err := flushCheckpointRefsQueue(ctx, repo, pushSettings{remote: "origin"})
	require.ErrorIs(t, err, remote.ErrHTTPAuthUnavailable)
	assert.Zero(t, pushed)
	assert.EqualValues(t, 2, requests.Load(), "one batch attempt, no individual retries or fetches")
	remaining, err := queue.Peek()
	require.NoError(t, err)
	assert.ElementsMatch(t, refs, remaining)

	delivered, err := doPushRef(ctx, "origin", refs[0])
	require.NoError(t, err, "auth failure must not block the user's push")
	assert.False(t, delivered)
	assert.EqualValues(t, 3, requests.Load(), "branch backend must not fetch/rebase on auth failure")

	err = pushCheckpointRefWithRecovery(ctx, "origin", refs[0])
	require.ErrorIs(t, err, remote.ErrHTTPAuthUnavailable)
	assert.EqualValues(t, 4, requests.Load(), "individual recovery must stop at auth failure too")

	err = fetchAndRebaseRefCommon(ctx, "origin", refs[0])
	require.ErrorIs(t, err, remote.ErrHTTPAuthUnavailable, "fetch recovery must preserve the typed cause")
	assert.EqualValues(t, 5, requests.Load())

	// Explicit but rejected credentials are a different diagnosis. Preserve it
	// without copying Git's credential-bearing URL into returned errors.
	target := strings.Replace(server.URL, "http://", "http://user:secret@", 1) + "/repo.git"
	_, err = remote.Push(ctx, target, refs[0].String())
	require.ErrorIs(t, err, remote.ErrHTTPAuthRejected)
	assert.NotContains(t, err.Error(), "secret")

	out := restore()
	assert.Equal(t, 1, strings.Count(out, checkpointAuthHelpURL))
	assert.Contains(t, out, "2 checkpoint ref(s) remain queued locally")
	assert.NotContains(t, out, "retrying")
	assert.NotContains(t, out, "Syncing")
}

func TestClassifyPushFailureKeepsSafeAuthCause(t *testing.T) {
	t.Parallel()
	err := classifyPushFailure(context.Background(), "https://user:secret@github.com", remote.ErrHTTPAuthRejected)
	require.ErrorIs(t, err, remote.ErrHTTPAuthRejected)
	assert.NotContains(t, err.Error(), "secret")
}
