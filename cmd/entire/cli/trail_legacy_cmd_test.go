package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// Serial callers only: moves CWD to a disposable clone.
func setupTrailBranchSelectionTest(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	testutil.RunGit(t, dir, "remote", "add", "origin", "git@github.com:acme/widget.git")
	testutil.WriteFile(t, dir, "file.txt", "initial")
	testutil.GitAdd(t, dir, "file.txt")
	testutil.GitCommit(t, dir, "initial")
	testutil.RunGit(t, dir, "checkout", "-b", "feature/work")
	t.Chdir(dir)
	return dir
}

func TestLegacyCheckoutRejectsSelectorWithBranch(t *testing.T) {
	// Serial: isolates CWD and replaces the repository client constructor.
	dir := setupTrailBranchSelectionTest(t)
	old := newTrailAPIClient
	calls := 0
	newTrailAPIClient = func(context.Context, bool, string, string, string) (*api.Client, string, error) {
		calls++
		return nil, "", errors.New("unexpected repository request")
	}
	t.Cleanup(func() { newTrailAPIClient = old })
	for _, args := range [][]string{
		{"1520", "--branch", "feature/other"},
		{"1520", "--branch", "feature/other", "--worktree"},
		{"--trail", "1520", "--branch", "feature/other"},
		{projectTrailTestChange, "--branch", "feature/other"},
		{"feature/work", "--branch", "feature/other"},
		{"1520", "--branch", "feature/work"},
	} {
		t.Run(args[0]+"/"+args[len(args)-1], func(t *testing.T) {
			cmd := newTrailCheckoutCmd(legacyTrailMode)
			cmd.SilenceErrors, cmd.SilenceUsage = true, true
			var out bytes.Buffer
			cmd.SetOut(&out)
			cmd.SetErr(&bytes.Buffer{})
			cmd.SetArgs(args)
			require.ErrorContains(t, cmd.ExecuteContext(t.Context()), "pass a trail selector or --branch, not both")
			require.Empty(t, out.String())
			require.Zero(t, calls, "reject before opening the repository client")
			require.Equal(t, "feature/work\n", testutil.RunGit(t, dir, "branch", "--show-current"))
			require.Equal(t, 1, strings.Count(testutil.RunGit(t, dir, "worktree", "list", "--porcelain"), "worktree "), "must not create another worktree")
		})
	}
}

func TestLegacyResumePreservesExpectedBranchAssertion(t *testing.T) {
	// Serial: isolates CWD and replaces the repository client constructor.
	dir := setupTrailBranchSelectionTest(t)
	for _, branch := range []string{"feature/work", "feature/other"} {
		t.Run(branch, func(t *testing.T) {
			setupWorkingRepoClient(t, func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/v1/trails/gh/acme/widget/7":
					assert.NoError(t, json.NewEncoder(w).Encode(api.TrailResource{ID: projectTrailTestChange, Number: 7, Branch: "feature/work"}))
				case "/api/v1/trails/gh/acme/widget/7/reviews/comments":
					assert.Equal(t, "feature/work", branch, "mismatched assertions stop before loading findings")
					assert.NoError(t, json.NewEncoder(w).Encode(map[string]any{"comments": []any{}}))
				default:
					t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
					http.NotFound(w, r)
				}
			})
			cmd := newTrailResumeCmd(legacyTrailMode)
			cmd.SetContext(t.Context())
			var out bytes.Buffer
			cmd.SetOut(&out)
			cmd.SetErr(&bytes.Buffer{})
			err := runTrailResume(cmd, legacyTrailMode, trailResumeOptions{Selector: "7", ExpectedBranch: branch, NoResume: true, JSON: true})
			if branch == "feature/work" {
				require.NoError(t, err)
				var resumed trailResumeContext
				require.NoError(t, json.Unmarshal(out.Bytes(), &resumed))
				require.Equal(t, "feature/work", resumed.Trail.Branch)
			} else {
				require.ErrorContains(t, err, `not expected branch "feature/other"`)
				require.Empty(t, out.String())
			}
			require.Equal(t, "feature/work\n", testutil.RunGit(t, dir, "branch", "--show-current"))
		})
	}
}

func TestTrailCheckoutPreservesBranchSelectionByMode(t *testing.T) {
	// Serial: isolates CWD and replaces the repository/project clients.
	for _, project := range []bool{false, true} {
		name := "legacy branch only"
		if project {
			name = "project selector and branch"
		}
		t.Run(name, func(t *testing.T) {
			dir := setupTrailBranchSelectionTest(t)
			if project {
				setupProjectTrailTest(t, func(w http.ResponseWriter, r *http.Request) {
					if !serveWorkingProjectRead(t, w, r) {
						t.Errorf("unexpected project request: %s %s", r.Method, r.URL.Path)
						http.NotFound(w, r)
					}
				})
			}
			setupWorkingRepoClient(t, func(w http.ResponseWriter, r *http.Request) {
				assert.False(t, project, "project checkout reads the work through its owned route")
				assert.Equal(t, "/api/v1/trails/gh/acme/widget", r.URL.Path)
				assert.NoError(t, json.NewEncoder(w).Encode(api.TrailListResponse{Trails: []api.TrailResource{{ID: projectTrailTestChange, Number: 7, Branch: "feature/work"}}}))
			})
			cmd := newTrailCheckoutCmd(trailModeFor(project))
			var out bytes.Buffer
			cmd.SetOut(&out)
			cmd.SetErr(&bytes.Buffer{})
			args := []string{"--branch", "feature/work"}
			if project {
				args = append([]string{"42"}, args...)
			}
			cmd.SetArgs(args)
			require.NoError(t, cmd.ExecuteContext(t.Context()))
			require.Contains(t, out.String(), "Already on branch feature/work")
			require.Equal(t, "feature/work\n", testutil.RunGit(t, dir, "branch", "--show-current"))
		})
	}
}
