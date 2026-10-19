package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/cmd/entire/cli/auth"
	"github.com/entireio/cli/cmd/entire/cli/settings"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

func TestProjectTrailRepoAuthErrorsAreActionable(t *testing.T) {
	// Serial: isolates CWD and replaces the repository client constructor.
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	testutil.RunGit(t, dir, "remote", "add", "origin", "git@github.com:acme/widget.git")
	t.Chdir(dir)
	for _, failure := range []struct {
		name string
		err  error
		hint string
	}{
		{"logged out", auth.ErrNotLoggedIn, "Not logged in. Run 'entire login' to authenticate."},
		{"not onboarded", errRepoNotOnboarded, "is not onboarded to Entire, or is not visible to your login."},
	} {
		t.Run(failure.name, func(t *testing.T) {
			for _, command := range []struct {
				args []string
				repo string
			}{
				{[]string{"approve", "42", "--repo", "gh/acme/foreign"}, "foreign"},
				{[]string{"request-changes", "42", "--repo", "gh/acme/foreign", "-m", "Fix it"}, "foreign"},
				{[]string{"approvals", "42", "--repo", "gh/acme/foreign"}, "foreign"},
				{[]string{"finding", "list", "42", "--repo", "gh/acme/foreign"}, "foreign"},
				{[]string{"watch", "42", "--repo", "gh/acme/foreign"}, "foreign"},
				{[]string{"show", "--branch", "feature/work", "--repo", "gh/acme/foreign"}, "foreign"},
				{[]string{"show", "foreign/7", "--project", "gh/acme"}, "foreign"},
				{[]string{"checkout", "42", "--branch", "feature/work"}, "widget"},
				{[]string{"resume", "42", "--branch", "feature/work"}, "widget"},
				{[]string{"finding", "apply", "42", "finding-one"}, "widget"},
			} {
				t.Run(command.args[0]+"/"+command.repo, func(t *testing.T) {
					old := newTrailAPIClient
					calls := 0
					newTrailAPIClient = func(_ context.Context, _ bool, forge, owner, repo string) (*api.Client, string, error) {
						calls++
						assert.Equal(t, "gh", forge)
						assert.Equal(t, "acme", owner)
						assert.Equal(t, command.repo, repo)
						return nil, "", fmt.Errorf("resolve repository: %w", failure.err)
					}
					t.Cleanup(func() { newTrailAPIClient = old })
					out, errOut, err := executeProjectTrailTest(t, command.args...)
					require.Equal(t, 1, calls)
					require.ErrorIs(t, err, failure.err)
					var silent *SilentError
					require.ErrorAs(t, err, &silent)
					require.Empty(t, out)
					require.Contains(t, errOut, failure.hint)
					require.Equal(t, 1, strings.Count(errOut, "\n"), "print the hint only once")
					if errors.Is(failure.err, errRepoNotOnboarded) {
						require.Contains(t, errOut, "acme/"+command.repo)
					}
				})
			}
		})
	}
}

func TestProjectTrailCollectionAuthErrorsAreActionable(t *testing.T) {
	// Serial: replaces the project client constructors.
	for _, phase := range []string{"core", "cell"} {
		t.Run(phase, func(t *testing.T) {
			setupProjectTrailTest(t, func(_ http.ResponseWriter, r *http.Request) {
				t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			})
			if phase == "core" {
				newProjectTrailCoreClient = func() (projectTrailCoreClient, error) { return nil, auth.ErrNotLoggedIn }
			} else {
				newProjectTrailCellClient = func(context.Context, bool, *auth.CellTarget) (*api.Client, error) {
					return nil, auth.ErrNotLoggedIn
				}
			}
			for _, args := range [][]string{
				{"show", projectTrailTestID, "--project", "gh/acme"},
				{"list", "--project", "gh/acme"},
				{"create", "--project", "gh/acme", "--no-branch", "--title", "Intent"},
			} {
				t.Run(args[0], func(t *testing.T) {
					out, errOut, err := executeProjectTrailTest(t, args...)
					require.ErrorIs(t, err, auth.ErrNotLoggedIn)
					var silent *SilentError
					require.ErrorAs(t, err, &silent)
					require.Empty(t, out)
					require.Equal(t, "Not logged in. Run 'entire login' to authenticate.\n", errOut)
				})
			}
		})
	}
}

func TestProjectTrailEnablementCacheOnlyTracksLocalRepository(t *testing.T) {
	// Serial: isolates CWD and replaces the project/repository constructors.
	for _, tt := range []struct {
		name    string
		args    []string
		enabled bool
	}{
		{"working context", []string{"approvals", "42", "--branch", "feature/work"}, true},
		{"branch discovery", []string{"show", "--branch", "feature/work"}, true},
		{"local change", []string{"approvals", "widget/7"}, true},
		{"foreign change", []string{"approvals", "other/7"}, false},
		{"explicit repo", []string{"approvals", "42", "--repo", "gh/acme/widget", "--branch", "feature/work"}, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			testutil.InitRepo(t, dir)
			testutil.RunGit(t, dir, "remote", "add", "origin", "git@github.com:acme/widget.git")
			t.Chdir(dir)
			require.NoError(t, saveTrailsEnabledForRepo(context.Background(), false))
			setupProjectTrailTest(t, func(w http.ResponseWriter, r *http.Request) {
				if !serveWorkingProjectRead(t, w, r) {
					t.Errorf("unexpected project request: %s %s", r.Method, r.URL.Path)
					http.NotFound(w, r)
				}
			})
			setupWorkingRepoClient(t, func(w http.ResponseWriter, r *http.Request) {
				work := api.TrailResource{ID: projectTrailTestChange, Number: 7, Branch: "feature/work",
					Parent: &api.TrailParentReference{ID: projectTrailTestID, Number: 42, ProjectID: projectTrailTestProject,
						Host: "gh", Project: "acme", Path: projectTrailTestPath, Jurisdiction: "eu", PrimaryProcessingCell: "project-cell"}}
				switch r.URL.Path {
				case "/api/v1/trails/gh/acme/widget":
					assert.NoError(t, json.NewEncoder(w).Encode(api.TrailListResponse{Trails: []api.TrailResource{work}}))
				case "/api/v1/trails/gh/acme/widget/7", "/api/v1/trails/gh/acme/other/7":
					assert.NoError(t, json.NewEncoder(w).Encode(work))
				case "/api/v1/trails/gh/acme/widget/7/approvals", "/api/v1/trails/gh/acme/other/7/approvals":
					assert.NoError(t, json.NewEncoder(w).Encode(api.TrailApprovalsResponse{}))
				default:
					t.Errorf("unexpected repository request: %s %s", r.Method, r.URL.Path)
					http.NotFound(w, r)
				}
			})
			_, _, err := executeProjectTrailTest(t, tt.args...)
			require.NoError(t, err)
			prefs, err := settings.LoadClonePreferences(context.Background())
			require.NoError(t, err)
			require.NotNil(t, prefs.TrailsEnabled)
			require.Equal(t, tt.enabled, *prefs.TrailsEnabled)
			require.Equal(t, "gh/acme/widget", prefs.TrailsEnabledRepoKey)
		})
	}
}
