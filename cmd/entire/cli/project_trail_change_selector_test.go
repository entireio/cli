package cli

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

func TestParseProjectTrailChangeSelector(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		in     string
		repo   string
		number int
		ok     bool
	}{
		{"cli/1503", "cli", 1503, true},
		{"entire-api/170", "entire-api", 170, true},
		{"my.repo_x/7", "my.repo_x", 7, true},
		{" cli/7 ", "cli", 7, true},
		{"42", "", 0, false},
		{"01ARZ3NDEKTSV4RRFFQ69G5FAV", "", 0, false},
		{"feature/work", "", 0, false},
		{"cli/0", "", 0, false},
		{"cli/+7", "", 0, false},
		{"cli/7/8", "", 0, false},
		{"/7", "", 0, false},
		{"../7", "", 0, false},
		{"cli/", "", 0, false},
	} {
		t.Run(tt.in, func(t *testing.T) {
			t.Parallel()
			got, ok := parseProjectTrailChangeSelector(tt.in)
			require.Equal(t, tt.ok, ok)
			require.Equal(t, projectTrailChangeSelector{Repo: tt.repo, Number: tt.number}, got)
		})
	}
}

// A <repo>/<number> selector reaches the branch work through the change's own
// parent, with no project-collection lookup, including merged work whose
// change read returns branch null. Not parallel: replaces client constructors.
func TestProjectChangeSelectorReachesBranchWork(t *testing.T) {
	for _, tt := range []struct {
		name          string
		summaryBranch string
		change        map[string]any
	}{
		{"open", "feature/work", map[string]any{"branch": "feature/work"}},
		{"merged", "feature/work", map[string]any{"branch": nil, "original_branch": "feature/work", "status": "merged"}},
		// An unlinked change may list no branch on its trail; identity alone
		// (ID, trail, repository) proves containment then.
		{"unlinked", "", map[string]any{"branch": nil, "original_branch": "feature/work"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			setupProjectTrailTest(t, func(w http.ResponseWriter, r *http.Request) {
				switch r.Method + " " + r.URL.Path {
				case "GET " + projectTrailTestPath:
					w.Header().Set("ETag", `W/"parent-version"`)
					parent := workingProjectTestResource()
					parent.Changes[0].Branch = tt.summaryBranch
					assert.NoError(t, json.NewEncoder(w).Encode(parent))
				case "GET " + projectTrailTestPath + "/changes/" + projectTrailTestChange:
					change := map[string]any{"id": projectTrailTestChange, "number": 7, "trailId": projectTrailTestID, "repositoryId": "repo-id", "status": "open"}
					for k, v := range tt.change {
						change[k] = v
					}
					assert.NoError(t, json.NewEncoder(w).Encode(change))
				default:
					t.Errorf("unexpected project request: %s %s", r.Method, r.URL.String())
					http.NotFound(w, r)
				}
			})
			var paths []string
			setupWorkingRepoClient(t, func(w http.ResponseWriter, r *http.Request) {
				paths = append(paths, r.Method+" "+r.URL.Path)
				switch r.URL.Path {
				case "/api/v1/trails/gh/acme/widget/7":
					assert.NoError(t, json.NewEncoder(w).Encode(api.TrailResource{ID: projectTrailTestChange, Number: 7, Status: "open",
						Parent: &api.TrailParentReference{ID: projectTrailTestID, Number: 42, ProjectID: projectTrailTestProject,
							Host: "gh", Project: "acme", Path: projectTrailTestPath, Jurisdiction: "eu", PrimaryProcessingCell: "project-cell"}}))
				case "/api/v1/trails/gh/acme/widget/7/reviews/comments":
					assert.NoError(t, json.NewEncoder(w).Encode(map[string]any{"comments": []any{}}))
				default:
					t.Errorf("unexpected repo request: %s %s", r.Method, r.URL.Path)
					http.NotFound(w, r)
				}
			})

			out, _, err := executeProjectTrailTest(t, "finding", "list", "widget/7", "--project", "gh/acme", "--json")
			require.NoError(t, err)
			require.Contains(t, paths, "GET /api/v1/trails/gh/acme/widget/7/reviews/comments")
			var got struct {
				Trail struct {
					Number int    `json:"number"`
					ID     string `json:"id"`
				} `json:"trail"`
			}
			require.NoError(t, json.Unmarshal([]byte(out), &got), out)
			require.Equal(t, 42, got.Trail.Number)
			require.Equal(t, projectTrailTestID, got.Trail.ID)
		})
	}
}

func TestProjectChangeSelectorRejectsConflictingTargets(t *testing.T) {
	// Not parallel: replaces client constructors.
	setupProjectTrailTest(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected project request: %s %s", r.Method, r.URL.Path)
		http.NotFound(w, r)
	})
	setupWorkingRepoClient(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected repo request: %s %s", r.Method, r.URL.Path)
		http.NotFound(w, r)
	})
	for _, tt := range []struct {
		args    []string
		failure string
	}{
		{[]string{"finding", "list", "widget/7", "--project", "gh/acme", "--branch", "feature/work"}, "not both"},
		{[]string{"approvals", "widget/7", "--project", "gh/acme", "--repo", "gh/acme/other"}, "does not match widget/7"},
	} {
		_, _, err := executeProjectTrailTest(t, tt.args...)
		require.ErrorContains(t, err, tt.failure, tt.args)
	}
}

// finding apply patches the local clone, so a <repo>/<number> selector naming
// another repository must be refused before any request, just as apply refuses
// --repo. Not parallel: changes CWD and replaces client constructors.
func TestProjectChangeSelectorFindingApplyStaysInThisClone(t *testing.T) {
	repoDir := t.TempDir()
	testutil.InitRepo(t, repoDir)
	testutil.IsolateGitConfigEnv(t)
	testutil.RunGit(t, repoDir, "remote", "add", "origin", "git@github.com:acme/widget.git")
	t.Chdir(repoDir)
	setupProjectTrailTest(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected project request: %s %s", r.Method, r.URL.Path)
		http.NotFound(w, r)
	})
	setupWorkingRepoClient(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected repo request: %s %s", r.Method, r.URL.Path)
		http.NotFound(w, r)
	})

	_, _, err := executeProjectTrailTest(t, "finding", "apply", "other/7", "finding-one", "--project", "gh/acme")
	require.ErrorContains(t, err, "other/7 is not in this clone's repository")
}
