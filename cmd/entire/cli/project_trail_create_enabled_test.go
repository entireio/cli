package cli

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/api"
)

// A repository with Trails off is refused before its branch is published or
// a create is sent, naming the setting rather than relaying the server's 422.
func TestProjectTrailCreateRefusesTrailsOffBeforePush(t *testing.T) {
	// Not parallel: git CWD and routing constructors are process-global.
	dir, remote := setupProjectTrailPublication(t)
	writeTrailCreatePrePushHook(t, dir, "#!/bin/sh\necho unexpected-push >&2\nexit 1\n")
	setupRepositoryTrailCreationWithRepo(t, "gh", `{"trails_enabled":false}`, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("no trail request when Trails is off: %s %s", r.Method, r.URL.Path)
		http.Error(w, "unexpected request", http.StatusInternalServerError)
	})

	out, errOut, err := executeProjectTrailTest(t, "create", "--title", "Start work", "--json")
	require.ErrorContains(t, err, "trails are not enabled for gh/acme/widget")
	require.ErrorContains(t, err, "Settings > Trails")
	require.ErrorContains(t, err, "Nothing was created or pushed")
	require.Empty(t, out)
	require.NotContains(t, errOut, "unexpected-push", "the branch is not published")
	_, statErr := os.Stat(filepath.Join(remote, "refs", "heads", "start-work"))
	require.ErrorIs(t, statErr, os.ErrNotExist)
}

// The settings read is best effort: when it fails, the create goes ahead.
func TestProjectTrailCreateProceedsWhenRepoReadFails(t *testing.T) {
	calls := 0
	setupRepositoryTrailCreationWithRepo(t, "gh", "", func(w http.ResponseWriter, r *http.Request) {
		calls++
		assert.Equal(t, http.MethodPost, r.Method)
		out := projectTrailTestResource()
		out.Changes = nil
		writeJSONResponse(t, w, http.StatusCreated, out)
	})
	_, _, err := executeProjectTrailTest(t, "create", "--no-branch", "--title", "Intent", "--repo", "gh/acme/widget", "--json")
	require.NoError(t, err)
	require.Equal(t, 1, calls)
}

// A create the server refuses says nothing was created, instead of asking
// the user to check whether it might have been.
func TestProjectTrailCreateRefusalSaysNothingCreated(t *testing.T) {
	setupRepositoryTrailCreation(t, "gh", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"title":"Unprocessable Entity","detail":"repoIds must contain repositories owned by the project with Trails enabled"}`, http.StatusUnprocessableEntity)
	})
	_, _, err := executeProjectTrailTest(t, "create", "--no-branch", "--title", "Intent", "--repo", "gh/acme/widget")
	require.ErrorContains(t, err, "no trail was created")
	require.NotContains(t, err.Error(), "may have succeeded")
	require.True(t, api.IsHTTPErrorStatus(err, http.StatusUnprocessableEntity))
}

func TestTrailCreateRefused(t *testing.T) {
	t.Parallel()
	for status, want := range map[int]bool{
		http.StatusBadRequest:          true,
		http.StatusForbidden:           true,
		http.StatusUnprocessableEntity: true,
		http.StatusRequestTimeout:      false,
		http.StatusConflict:            false,
		http.StatusTooManyRequests:     false,
		http.StatusInternalServerError: false,
		http.StatusBadGateway:          false,
	} {
		require.Equal(t, want, trailCreateRefused(&api.HTTPError{StatusCode: status}), "status %d", status)
	}
	require.False(t, trailCreateRefused(errors.New("connection reset")), "a transport error may have been applied")
}

// With --idempotency-key, a refused create keeps the retry hint: the key is
// what makes a retry safe.
func TestProjectTrailCreateRefusalKeepsIdempotencyHint(t *testing.T) {
	setupRepositoryTrailCreation(t, "gh", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"detail":"refused"}`, http.StatusUnprocessableEntity)
	})
	_, _, err := executeProjectTrailTest(t, "create", "--no-branch", "--title", "Intent", "--repo", "gh/acme/widget", "--idempotency-key", "k-1")
	require.ErrorContains(t, err, "retry with --idempotency-key")
}

// A repository response without the setting does not block the create.
func TestProjectTrailCreateProceedsWithoutTrailsSetting(t *testing.T) {
	calls := 0
	setupRepositoryTrailCreationWithRepo(t, "gh", `{"id":"repo"}`, func(w http.ResponseWriter, r *http.Request) {
		calls++
		assert.Equal(t, http.MethodPost, r.Method)
		out := projectTrailTestResource()
		out.Changes = nil
		writeJSONResponse(t, w, http.StatusCreated, out)
	})
	_, _, err := executeProjectTrailTest(t, "create", "--no-branch", "--title", "Intent", "--repo", "gh/acme/widget", "--json")
	require.NoError(t, err)
	require.Equal(t, 1, calls)
}

// After a failed settings read, create in the current repository still
// publishes the branch and links it.
func TestProjectTrailCreatePublishesWhenRepoReadFails(t *testing.T) {
	// Not parallel: git CWD and routing constructors are process-global.
	_, remote := setupProjectTrailPublication(t)
	setupRepositoryTrailCreationWithRepo(t, "gh", "", repositoryTrailCreationHandler(t, "gh", nil))
	_, errOut, err := executeProjectTrailTest(t, "create", "--title", "Start work", "--json")
	require.NoError(t, err, errOut)
	_, statErr := os.Stat(filepath.Join(remote, "refs", "heads", "start-work"))
	require.NoError(t, statErr, "the branch was published")
}
