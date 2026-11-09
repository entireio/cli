package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// projectMergeRepos stands in for every repository a project merge can reach.
// newTrailAPIClient maps a repository name to its ID; each ID's
// repo_id-addressed detail serves mergeability, and merge POSTs are recorded
// by path.
type projectMergeRepos struct {
	t            *testing.T
	mergeability string
	ids          map[string]string

	mu      sync.Mutex
	posts   []string
	details []string
}

var projectMergeDetailPath = regexp.MustCompile(`^/api/v1/repos/([^/]+)/trails/([0-9]+)(/merge)?$`)

func setupProjectMergeRepos(t *testing.T, mergeability string, ids map[string]string) *projectMergeRepos {
	t.Helper()
	repos := &projectMergeRepos{t: t, mergeability: mergeability, ids: ids}
	server := httptest.NewServer(repos)
	t.Cleanup(server.Close)
	old := newTrailAPIClient
	newTrailAPIClient = func(_ context.Context, _ bool, _, _, repo string) (*api.Client, string, error) {
		id, ok := ids[repo]
		if !ok {
			return nil, "", fmt.Errorf("unexpected repository %q", repo)
		}
		return api.NewClientWithBaseURL("repo-token", server.URL), id, nil
	}
	t.Cleanup(func() { newTrailAPIClient = old })
	return repos
}

func (s *projectMergeRepos) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	m := projectMergeDetailPath.FindStringSubmatch(r.URL.Path)
	switch {
	case m != nil && m[3] == "" && r.Method == http.MethodGet:
		s.mu.Lock()
		s.details = append(s.details, r.URL.Path)
		s.mu.Unlock()
		_, err := fmt.Fprintf(w, `{"id":%q,"number":%s,"status":"open","branch":"feature/work","base":"main","mergeability":%s,"actions":{"merge_with_bypass":{"state":"enabled","reason":null}}}`,
			projectTrailTestChange, m[2], s.mergeability)
		assert.NoError(s.t, err)
	case m != nil && m[3] != "" && r.Method == http.MethodPost:
		_, err := io.ReadAll(r.Body)
		assert.NoError(s.t, err)
		s.mu.Lock()
		s.posts = append(s.posts, r.URL.Path)
		s.mu.Unlock()
		_, err = fmt.Fprint(w, `{"ok":true,"mergeCommitSha":"merge-example"}`)
		assert.NoError(s.t, err)
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/v1/trails/"):
		// Trail lists: branch discovery, or a failed command re-probing enablement.
		http.NotFound(w, r)
	default:
		s.t.Errorf("unexpected repository request: %s %s", r.Method, r.URL.Path)
		http.NotFound(w, r)
	}
}

func (s *projectMergeRepos) mergePosts() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.posts...)
}

func serveProjectMergeTrail(t *testing.T) {
	t.Helper()
	setupProjectTrailTest(t, func(w http.ResponseWriter, r *http.Request) {
		if !serveWorkingProjectRead(t, w, r) {
			t.Errorf("unexpected project request: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	})
}

// cloneWithOrigin makes this test's CWD a committed clone of origin.
func cloneWithOrigin(t *testing.T, origin string) string {
	t.Helper()
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	testutil.IsolateGitConfigEnv(t)
	testutil.RunGit(t, dir, "remote", "add", "origin", origin)
	testutil.WriteFile(t, dir, "README.md", "widget")
	testutil.GitAdd(t, dir, "README.md")
	testutil.GitCommit(t, dir, "initial")
	t.Chdir(dir)
	return dir
}

// merge resolves a project trail to its one change, merges it through that
// change's repository number, and names it as <repo>/<number>: a bare number
// in a project hint would mean a different project trail.
// Not parallel: replaces client constructors.
func TestProjectTrailMergeMergesTheSelectedChange(t *testing.T) {
	serveProjectMergeTrail(t)
	repos := setupProjectMergeRepos(t, trailMergeability(t), map[string]string{"widget": "repo-id"})

	out, _, err := executeProjectTrailTest(t, "merge", "42", "--repo", "gh/acme/widget")
	require.NoError(t, err)
	require.Equal(t, []string{"/api/v1/repos/repo-id/trails/7/merge"}, repos.mergePosts())
	require.Contains(t, out, "Change widget/7 of trail #42 (feature/work → main)")
	require.Contains(t, out, "Merged change widget/7 of trail #42 into main (merge-example)")
	require.NotContains(t, out, "trail #7")
}

// Every retry hint names the change with its project. Without --project a
// change selector resolves in origin's owner, so from a clone whose origin is
// another owner (a fork) a bare widget/7 would name different work.
// Not parallel: changes CWD and replaces client constructors.
func TestProjectTrailMergeHintsKeepTheProject(t *testing.T) {
	cloneWithOrigin(t, "git@github.com:fork/widget.git")
	serveProjectMergeTrail(t)
	repos := setupProjectMergeRepos(t, trailMergeability(t, blocked, baseChecksRed), map[string]string{"widget": "repo-id"})

	_, _, err := executeProjectTrailTest(t, "merge", "42", "--project", "gh/acme")
	require.Error(t, err)
	require.Empty(t, repos.mergePosts())
	require.Contains(t, err.Error(), "change widget/7 of trail #42 is not mergeable: 1 blocking gate failing")
	require.Contains(t, err.Error(), "rerun 'entire trail merge widget/7 --project gh/acme --force'")
	require.NotRegexp(t, `merge widget/7 --force`, err.Error())
	require.NotContains(t, err.Error(), "merge 42 ")
}

// The refusal hints after a POST carry the project too.
func TestProjectTrailMergeRefusalHintsKeepTheProject(t *testing.T) {
	t.Parallel()
	ref := projectTrailMergeRef("widget", 7, 42, "gh/acme")
	for _, tc := range []struct {
		status int
		code   string
		want   string
	}{
		{http.StatusUnprocessableEntity, "merge_gates_failed", "run 'entire trail merge widget/7 --project gh/acme --dry-run' to see why, or rerun 'entire trail merge widget/7 --project gh/acme --force' to bypass"},
		{http.StatusConflict, "merge_head_mismatch", "rerun 'entire trail merge widget/7 --project gh/acme' to merge the new head"},
	} {
		err := trailMergeRefusal(&api.HTTPError{StatusCode: tc.status, Code: tc.code}, ref, trailBypassVerdict{allowed: true}, "admins")
		require.ErrorContains(t, err, tc.want)
	}
}

// A blocked change with no blocking gates to name gets no "for details" hint:
// the only source of details for a project change is the merge readiness
// report the command already printed.
// Not parallel: replaces client constructors.
func TestProjectTrailMergeBlockedWithoutGatesHasNoCircularHint(t *testing.T) {
	serveProjectMergeTrail(t)
	setupProjectMergeRepos(t, trailMergeability(t, blocked), map[string]string{"widget": "repo-id"})

	_, _, err := executeProjectTrailTest(t, "merge", "42", "--repo", "gh/acme/widget", "--dry-run")
	require.Error(t, err)
	require.Equal(t, "change widget/7 of trail #42 is not mergeable", err.Error())
	require.Equal(t, []string{"a blocking gate failed"}, describeMergeabilityBlockers(projectTrailMergeRef("widget", 7, 42, "gh/acme"), &api.TrailMergeabilityResponse{}))
}

// Not parallel: replaces client constructors.
func TestProjectTrailMergeJSONNamesTheChangeAndProject(t *testing.T) {
	serveProjectMergeTrail(t)
	repos := setupProjectMergeRepos(t, trailMergeability(t), map[string]string{"widget": "repo-id"})

	out, _, err := executeProjectTrailTest(t, "merge", "42", "--repo", "gh/acme/widget", "--dry-run", "--json")
	require.NoError(t, err)
	require.Empty(t, repos.mergePosts())
	var result trailMergeResultJSON
	require.NoError(t, json.Unmarshal([]byte(strings.TrimSpace(out)), &result))
	require.Equal(t, "widget/7", result.Change)
	require.Equal(t, "gh/acme", result.Project)
	require.Equal(t, 42, result.Trail)
	require.Equal(t, 7, result.Number)
	require.True(t, result.Mergeable)
}

// With several open changes merge lists them and merges nothing, even when
// one is on the checked-out branch: merge never selects by checkout.
// Not parallel: changes CWD and replaces client constructors.
func TestProjectTrailMergeWithSeveralChangesNeverGuesses(t *testing.T) {
	dir := cloneWithOrigin(t, "git@github.com:acme/widget.git")
	current := strings.TrimSpace(testutil.RunGit(t, dir, "branch", "--show-current"))
	parent := projectTrailTestResource()
	parent.Changes = []api.ChangeSummary{
		{ID: projectTrailTestChange, RepositoryID: "repo-id", Repository: "widget", Number: 7, Title: "Widget work", Branch: current},
		{ID: "01ARZ3NDEKTSV4RRFFQ69G5FAY", RepositoryID: "api-id", Repository: "api", Number: 9, Title: "API work", Branch: "feature/api"},
	}
	setupProjectTrailTest(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodGet, r.Method)
		assert.NoError(t, json.NewEncoder(w).Encode(parent))
	})
	repos := setupProjectMergeRepos(t, trailMergeability(t), map[string]string{"widget": "repo-id", "api": "api-id"})

	_, _, err := executeProjectTrailTest(t, "merge", "42")
	require.Error(t, err)
	require.Contains(t, err.Error(), "trail #42 has 2 open changes; choose one:")
	require.Contains(t, err.Error(), "widget/7 - Widget work")
	require.Contains(t, err.Error(), "api/9 - API work")
	require.Empty(t, repos.mergePosts())
	require.Empty(t, repos.details, "listing must not read mergeability of any change")
}

// A trail whose only open change is in another repository merges that
// repository's change, through its own repository ID.
// Not parallel: changes CWD and replaces client constructors.
func TestProjectTrailMergeRoutesToTheChangesRepository(t *testing.T) {
	cloneWithOrigin(t, "git@github.com:acme/widget.git")
	const apiChange = "01ARZ3NDEKTSV4RRFFQ69G5FAY"
	parent := projectTrailTestResource()
	parent.Changes = []api.ChangeSummary{
		{ID: projectTrailTestChange, RepositoryID: "repo-id", Repository: "widget", Number: 7, Title: "Merged widget work"},
		{ID: apiChange, RepositoryID: "api-id", Repository: "api", Number: 9, Title: "API work", Branch: "feature/api"},
	}
	setupProjectTrailTest(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/gh/acme/trails/42", projectTrailTestPath:
			assert.NoError(t, json.NewEncoder(w).Encode(parent))
		case projectTrailTestPath + "/changes/" + apiChange:
			assert.NoError(t, json.NewEncoder(w).Encode(map[string]any{"id": apiChange, "number": 9, "trailId": projectTrailTestID, "repositoryId": "api-id", "branch": "feature/api", "base": "main", "status": "open"}))
		default:
			t.Errorf("unexpected project request: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	})
	repos := setupProjectMergeRepos(t, trailMergeability(t), map[string]string{"widget": "repo-id", "api": "api-id"})

	out, _, err := executeProjectTrailTest(t, "merge", "42")
	require.NoError(t, err)
	require.Equal(t, []string{"/api/v1/repos/api-id/trails/9/merge"}, repos.mergePosts())
	require.Contains(t, out, "Merged change api/9 of trail #42 into main")
}

// A GitHub-backed change leaves its branch to GitHub, so the delete-branch
// flags are refused before anything is read or merged.
// Not parallel: replaces client constructors.
func TestProjectTrailMergeRefusesDeleteBranchFlagsOnGitHub(t *testing.T) {
	for _, flag := range []string{"--delete-branch", "--no-delete-branch"} {
		t.Run(flag, func(t *testing.T) {
			serveProjectMergeTrail(t)
			repos := setupProjectMergeRepos(t, trailMergeability(t), map[string]string{"widget": "repo-id"})

			_, _, err := executeProjectTrailTest(t, "merge", "42", "--repo", "gh/acme/widget", flag)
			require.ErrorIs(t, err, errTrailMergeDeleteBranchGitHub)
			require.Empty(t, repos.mergePosts())
			require.Empty(t, repos.details)
		})
	}
}
