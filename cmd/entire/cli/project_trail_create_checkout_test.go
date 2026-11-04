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

// These tests replace routing constructors and change CWD; they cannot run in parallel.

func currentTestBranch(t *testing.T, dir string) string {
	t.Helper()
	return strings.TrimSpace(testutil.RunGit(t, dir, "branch", "--show-current"))
}

func TestProjectTrailCreateCheckoutSwitchesAfterPublish(t *testing.T) {
	dir, _ := setupProjectTrailPublication(t)
	testutil.WriteFile(t, dir, "README.md", "uncommitted edit\n")
	testutil.WriteFile(t, dir, "work-in-progress.txt", "not committed\n")
	setupRepositoryTrailCreation(t, "gh", repositoryTrailCreationHandler(t, "gh", nil))

	out, errOut, err := executeProjectTrailTest(t, "create", "--title", "Start work", "--checkout")
	require.NoError(t, err, errOut)
	require.Contains(t, out, projectTrailTestID)
	require.Equal(t, "start-work", currentTestBranch(t, dir))
	require.Contains(t, errOut, "Switched to branch start-work")
	require.NotContains(t, errOut, "Next:")
	// Derived branches start at HEAD, so uncommitted work carries over.
	status := testutil.RunGit(t, dir, "status", "--porcelain")
	require.Contains(t, status, "README.md")
	require.Contains(t, status, "work-in-progress.txt")
}

func TestProjectTrailCreateCheckoutJSONStdoutStaysValid(t *testing.T) {
	dir, _ := setupProjectTrailPublication(t)
	setupRepositoryTrailCreation(t, "gh", repositoryTrailCreationHandler(t, "gh", nil))

	out, errOut, err := executeProjectTrailTest(t, "create", "--title", "Start work", "--checkout", "--json")
	require.NoError(t, err, errOut)
	require.True(t, json.Valid([]byte(out)), "stdout must be valid JSON: %q", out)
	require.JSONEq(t, mustProjectTrailJSON(t), out)
	require.Contains(t, errOut, "Switched to branch start-work")
	require.Equal(t, "start-work", currentTestBranch(t, dir))
}

func TestProjectTrailCreateCheckoutRefusalKeepsTrail(t *testing.T) {
	dir, _ := setupProjectTrailPublication(t)
	testutil.RunGit(t, dir, "checkout", "-b", "feature/diverged")
	testutil.WriteFile(t, dir, "README.md", "diverged\n")
	testutil.RunGit(t, dir, "commit", "-am", "diverge")
	testutil.RunGit(t, dir, "checkout", "main")
	testutil.WriteFile(t, dir, "README.md", "local edit on main\n")
	setupRepositoryTrailCreation(t, "gh", repositoryTrailCreationHandler(t, "gh", nil))

	out, errOut, err := executeProjectTrailTest(t, "create", "--title", "Diverged", "--branch", "feature/diverged", "--checkout", "--json")
	require.Error(t, err)
	require.ErrorContains(t, err, "trail #42 was created; checkout failed:")
	require.ErrorContains(t, err, "run `git switch feature/diverged`")
	require.NotContains(t, err.Error(), "checkout failed: checkout failed")
	require.JSONEq(t, mustProjectTrailJSON(t), out, "the created trail is still printed: %s", errOut)
	require.Equal(t, "main", currentTestBranch(t, dir))
	data, readErr := os.ReadFile(filepath.Join(dir, "README.md"))
	require.NoError(t, readErr)
	require.Equal(t, "local edit on main\n", string(data))
}

func TestProjectTrailCreateCheckoutAlreadyCurrentIsNoOp(t *testing.T) {
	dir, _ := setupProjectTrailPublication(t)
	testutil.RunGit(t, dir, "checkout", "-b", "feature/current")
	setupRepositoryTrailCreation(t, "gh", repositoryTrailCreationHandler(t, "gh", func(change api.ChangeCreateRequest) {
		assert.Equal(t, "feature/current", change.BranchName)
	}))

	for _, args := range [][]string{{"--checkout"}, nil} {
		out, errOut, err := executeProjectTrailTest(t, append([]string{"create", "--title", "Current"}, args...)...)
		require.NoError(t, err, errOut)
		require.Contains(t, out, projectTrailTestID)
		require.NotContains(t, errOut, "Switched to branch")
		require.NotContains(t, errOut, "Next:")
		require.Equal(t, "feature/current", currentTestBranch(t, dir))
	}
}

func TestProjectTrailCreatePrintsNextStepForOtherBranch(t *testing.T) {
	dir, _ := setupProjectTrailPublication(t)
	setupRepositoryTrailCreation(t, "gh", repositoryTrailCreationHandler(t, "gh", nil))

	out, errOut, err := executeProjectTrailTest(t, "create", "--title", "Start work", "--json")
	require.NoError(t, err, errOut)
	require.JSONEq(t, mustProjectTrailJSON(t), out)
	require.Contains(t, errOut, "Next: git switch start-work\n")
	require.NotContains(t, out, "Next:")
	require.Equal(t, "main", currentTestBranch(t, dir))
}

func TestProjectTrailCreateNoNextStepWithoutLocalBranch(t *testing.T) {
	for _, tt := range []struct {
		name string
		args []string
	}{
		{"repo", []string{"--repo", "gh/acme/widget", "--branch", "feature/remote"}},
		{"no branch", []string{"--repo", "gh/acme/widget", "--no-branch"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			handler := repositoryTrailCreationHandler(t, "gh", nil)
			setupRepositoryTrailCreation(t, "gh", func(w http.ResponseWriter, r *http.Request) {
				if tt.name == "no branch" {
					out := projectTrailTestResource()
					assert.NoError(t, json.NewEncoder(w).Encode(out))
					return
				}
				handler(w, r)
			})
			_, errOut, err := executeProjectTrailTest(t, append([]string{"create", "--title", "Remote"}, tt.args...)...)
			require.NoError(t, err, errOut)
			require.NotContains(t, errOut, "Next:")
			require.NotContains(t, errOut, "Switched")
		})
	}
}

func TestProjectTrailCreateCheckoutRejectsNonLocalCreation(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name, want string
		args       []string
	}{
		{"no branch", "cannot combine --no-branch with --checkout", []string{"--no-branch"}},
		{"repo", "cannot combine --repo with --checkout", []string{"--repo", "gh/acme/widget", "--branch", "feat"}},
		{"server-side branch", "cannot combine --branch-action create with --checkout", []string{"--branch", "feat", "--branch-action", "create"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			args := append([]string{"create", "--title", "Intent", "--checkout"}, tt.args...)
			_, _, err := executeProjectTrailTest(t, args...)
			require.ErrorContains(t, err, tt.want)
		})
	}
}

func TestProjectTrailCreateAddAssigneeUnionsWithAssignee(t *testing.T) {
	for _, tt := range []struct {
		name string
		args []string
		want []string
	}{
		{"alias only", []string{"--add-assignee", "alice,bob"}, []string{"alice", "bob"}},
		{"union deduped", []string{"--assignee", "alice,bob", "--add-assignee", "bob", "--add-assignee", "carol"}, []string{"alice", "bob", "carol"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var got []string
			setupRepositoryTrailCreation(t, "gh", func(w http.ResponseWriter, r *http.Request) {
				var request api.ProjectTrailCreateRequest
				assert.NoError(t, json.NewDecoder(r.Body).Decode(&request))
				got = request.Assignees
				assert.NoError(t, json.NewEncoder(w).Encode(projectTrailTestResource()))
			})
			args := append([]string{"create", "--repo", "gh/acme/widget", "--no-branch", "--title", "Intent", "--json"}, tt.args...)
			out, errOut, err := executeProjectTrailTest(t, args...)
			require.NoError(t, err, errOut)
			require.True(t, json.Valid([]byte(out)))
			require.Equal(t, tt.want, got)
			require.NotContains(t, strings.ToLower(errOut), "deprecat")
		})
	}
}

func TestProjectTrailCreateHelpHidesAddAssignee(t *testing.T) {
	t.Parallel()
	out, _, err := executeProjectTrailTest(t, "create", "--help")
	require.NoError(t, err)
	require.NotContains(t, out, "--add-assignee")
	require.Contains(t, out, "--checkout")
	require.Contains(t, out, "unless --checkout")
}
