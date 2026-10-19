package cli

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// Keep the public origin identity for API resolution, but configure publication
// to a temporary bare repo. All git transports target that named push remote.
func setupProjectTrailPublication(t *testing.T) (string, string) {
	t.Helper()
	dir, remote, repo := initTrailCleanupRepo(t)
	repo.Close()
	testutil.RunGit(t, dir, "branch", "-M", "main")
	testutil.RunGit(t, dir, "remote", "set-url", "origin", "git@github.com:acme/widget.git")
	testutil.RunGit(t, dir, "remote", "add", "publication", remote)
	testutil.RunGit(t, dir, "config", "remote.pushDefault", "publication")
	t.Chdir(dir)
	return dir, remote
}

func TestProjectTrailCreatePublishesBeforePOSTWithoutCommitting(t *testing.T) {
	// Not parallel: git CWD and routing constructors are process-global.
	dir, remote := setupProjectTrailPublication(t)
	before := testutil.RunGit(t, dir, "rev-parse", "HEAD")
	testutil.WriteFile(t, dir, "work-in-progress.txt", "not committed\n")
	writeTrailCreatePrePushHook(t, dir, "#!/bin/sh\necho checkpoint-hook-ran >&2\n")
	setupProjectTrailRepoPlacement(t)
	var publishedAtPOST []byte
	setupProjectTrailTest(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method)
		var request api.ProjectTrailCreateRequest
		if !assert.NoError(t, json.NewDecoder(r.Body).Decode(&request)) || !assert.Len(t, request.Changes, 1) {
			return
		}
		assert.Equal(t, "start-work", request.Changes[0].BranchName)
		assert.Equal(t, "main", request.Changes[0].Base)
		assert.Equal(t, "link", request.Changes[0].BranchAction)
		var err error
		publishedAtPOST, err = os.ReadFile(filepath.Join(remote, "refs", "heads", "start-work"))
		assert.NoError(t, err)
		assert.NoError(t, json.NewEncoder(w).Encode(projectTrailTestResource()))
	})
	out, errOut, err := executeProjectTrailTest(t, "create", "--title", "Start work", "--json")
	require.NoError(t, err, errOut)
	require.JSONEq(t, mustProjectTrailJSON(t), out)
	require.Contains(t, errOut, "checkpoint-hook-ran")
	require.Equal(t, strings.TrimSpace(before), strings.TrimSpace(string(publishedAtPOST)))
	require.Equal(t, before, testutil.RunGit(t, dir, "rev-parse", "HEAD"))
	require.Equal(t, "main\n", testutil.RunGit(t, dir, "branch", "--show-current"))
	require.Contains(t, testutil.RunGit(t, dir, "status", "--porcelain"), "work-in-progress.txt")
}

func mustProjectTrailJSON(t *testing.T) string {
	t.Helper()
	data, err := json.Marshal(projectTrailTestResource())
	require.NoError(t, err)
	return string(data)
}

func TestProjectTrailCreatePushRejectionStopsBeforePOST(t *testing.T) {
	dir, remote := setupProjectTrailPublication(t)
	writeTrailCreatePrePushHook(t, dir, "#!/bin/sh\necho checkpoint-hook-rejected >&2\nexit 1\n")
	setupProjectTrailRepoPlacement(t)
	setupProjectTrailTest(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("API must not be called after rejected push: %s", r.Method)
		http.Error(w, "unexpected request", http.StatusInternalServerError)
	})
	out, errOut, err := executeProjectTrailTest(t, "create", "--title", "Rejected push", "--json")
	require.ErrorContains(t, err, "trail creation was not sent")
	require.Contains(t, errOut, "checkpoint-hook-rejected")
	require.Empty(t, out)
	require.True(t, gitBranchExistsTrailTest(t, dir, "rejected-push"), "preserve the local branch on failure")
	require.False(t, gitBranchExistsTrailTest(t, remote, "rejected-push"))
}

func TestProjectTrailCreateAPIFailurePreservesPublishedBranch(t *testing.T) {
	dir, remote := setupProjectTrailPublication(t)
	setupProjectTrailRepoPlacement(t)
	setupProjectTrailTest(t, func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"detail":"uncertain outcome"}`, http.StatusServiceUnavailable)
	})
	_, _, err := executeProjectTrailTest(t, "create", "--title", "Preserve work")
	require.Error(t, err)
	require.True(t, gitBranchExistsTrailTest(t, dir, "preserve-work"))
	require.True(t, gitBranchExistsTrailTest(t, remote, "preserve-work"))
}

func TestProjectTrailCreateNoBranchOrServerCreationDoesNotPush(t *testing.T) {
	for _, noBranch := range []bool{true, false} {
		t.Run(map[bool]string{true: "intent only", false: "server creation"}[noBranch], func(t *testing.T) {
			dir, _ := setupProjectTrailPublication(t)
			writeTrailCreatePrePushHook(t, dir, "#!/bin/sh\necho unexpected-push >&2\nexit 1\n")
			setupProjectTrailRepoPlacement(t)
			setupProjectTrailTest(t, func(w http.ResponseWriter, r *http.Request) {
				var request api.ProjectTrailCreateRequest
				if !assert.NoError(t, json.NewDecoder(r.Body).Decode(&request)) {
					return
				}
				if noBranch {
					assert.Empty(t, request.Changes)
				} else if assert.Len(t, request.Changes, 1) {
					assert.Equal(t, cmdCreate, request.Changes[0].BranchAction)
				}
				assert.NoError(t, json.NewEncoder(w).Encode(projectTrailTestResource()))
			})
			args := []string{"create", "--title", "Remote work"}
			if noBranch {
				args = append(args, "--no-branch")
			} else {
				args = append(args, "--branch-action", "create")
			}
			_, errOut, err := executeProjectTrailTest(t, args...)
			require.NoError(t, err)
			require.NotContains(t, errOut, "unexpected-push")
		})
	}
}
