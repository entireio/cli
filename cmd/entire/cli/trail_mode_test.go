package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Project trails are the only model the CLI builds; the retired opt-in
// variable no longer selects the legacy tree, whatever it is set to.
func TestTrailCommandIsAlwaysProjectScoped(t *testing.T) {
	for _, value := range []string{"", "0", "1"} {
		t.Run(value, func(t *testing.T) {
			t.Setenv("ENTIRE_PROJECT_TRAILS", value)
			cmd := newTrailCmd()
			require.True(t, usesProjectTrails(cmd))
			require.NotNil(t, cmd.PersistentFlags().Lookup("project"))
			child, _, err := cmd.Find([]string{"finding", "resolve"})
			require.NoError(t, err)
			require.True(t, usesProjectTrails(child))
		})
	}
}

func TestTrailModeCommandSurface(t *testing.T) {
	t.Parallel()
	for _, project := range []bool{false, true} {
		t.Run(fmt.Sprintf("project=%t", project), func(t *testing.T) {
			t.Parallel()
			root := &cobra.Command{Use: "entire"}
			cmd := newTrailCmdForMode(project)
			root.AddCommand(cmd)
			names := commandNames(cmd.Commands())
			for _, name := range []string{"list", "show", "create", "update", "comment", "finding", "approve", "approvals", "watch", "checkout", "resume"} {
				require.Contains(t, names, name)
			}
			for _, name := range []string{"link", "unlink"} {
				require.Equal(t, project, contains(names, name))
			}
			require.Contains(t, names, "merge")
			require.Equal(t, !project, contains(names, "delete"))
			list, _, err := cmd.Find([]string{"list"})
			require.NoError(t, err)
			require.Equal(t, project, list.Flags().Lookup("page-token") != nil)
			require.Equal(t, !project, list.Flags().Lookup("author") != nil)
			for _, c := range cmd.Commands() {
				text := renderAgentHelpCommand(c, "gh/acme/widget", true)
				if !project {
					require.NotContains(t, text, "project trail")
					require.NotContains(t, text, "--project")
					require.NotContains(t, text, "within the trail")
				}
			}
			require.Equal(t, project, contains(commandNames(agentHelpCommands(root, false)), "trail"))
			if !project {
				root.SetArgs([]string{"trail", "list", "--project", "gh/acme"})
				var out bytes.Buffer
				root.SetOut(&out)
				root.SetErr(&out)
				require.ErrorContains(t, root.Execute(), "unknown flag: --project")
			}
		})
	}
}

func TestLegacyTrailRequestsDoNotResolveProject(t *testing.T) {
	// Serial: replaces global clients.
	for _, args := range [][]string{
		{"list", "--json"}, {"show", "7", "--json"},
		{"approve", "7"},
		{"approvals", "7", "--json"}, {"comment", "list", "--trail", "7", "--json"},
		{"finding", "list", "feature/work", "--json"},
		{"finding", "--branch", "missing"},
		{"finding", "resolve", "7", "finding-one"},
	} {
		t.Run(fmt.Sprint(args), func(t *testing.T) {
			old := newProjectTrailCoreClient
			newProjectTrailCoreClient = func() (projectTrailCoreClient, error) {
				t.Error("legacy command must not resolve a project")
				return nil, errors.New("unexpected project resolution")
			}
			t.Cleanup(func() { newProjectTrailCoreClient = old })
			var paths []string
			setupWorkingRepoClient(t, func(w http.ResponseWriter, r *http.Request) {
				paths = append(paths, r.Method+" "+r.URL.Path)
				assert.NotContains(t, r.URL.RawQuery, "projectId")
				item := api.TrailResource{ID: projectTrailTestChange, Number: 7, Branch: "feature/work", Title: "Legacy work", Status: "open", Parent: &api.TrailParentReference{ID: projectTrailTestID, Number: 42}}
				var payload any
				switch r.Method + " " + r.URL.Path {
				case "GET /api/v1/trails/gh/acme/widget":
					payload = api.TrailListResponse{Trails: []api.TrailResource{item}}
				case "GET /api/v1/trails/gh/acme/widget/7":
					payload = item
				case "GET /api/v1/trails/gh/acme/widget/7/body":
					_, err := fmt.Fprint(w, "Description")
					assert.NoError(t, err)
					return
				case "POST /api/v1/trails/gh/acme/widget/7/approvals", "GET /api/v1/trails/gh/acme/widget/7/approvals":
					payload = map[string]any{"approvals": []any{}}
				case "GET /api/v1/trails/gh/acme/widget/7/discussions":
					payload = map[string]any{"discussions": []any{}}
				case "GET /api/v1/trails/gh/acme/widget/7/reviews/comments":
					payload = map[string]any{"comments": []map[string]any{{"id": "finding-one", "review_id": "review-one", "status": "open"}}}
				case "PATCH /api/v1/trails/gh/acme/widget/7/reviews/review-one/comments/finding-one":
					payload = map[string]any{"id": "finding-one", "review_id": "review-one", "status": "resolved"}
				default:
					t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
					http.NotFound(w, r)
					return
				}
				assert.NoError(t, json.NewEncoder(w).Encode(payload))
			})
			cmd := newTrailCmdForMode(false)
			var out bytes.Buffer
			cmd.SetOut(&out)
			cmd.SetErr(&out)
			cmd.SetArgs(append(append([]string{}, args...), "--repo", "gh/acme/widget"))
			require.NoError(t, cmd.ExecuteContext(context.Background()), out.String())
			require.NotEmpty(t, paths)
			require.NotContains(t, out.String(), "trail #42")
			if args[0] == "finding" && args[1] == "--branch" {
				require.Contains(t, out.String(), "showing trails in this repo")
			}
			if args[0] == "finding" && args[1] == "resolve" {
				require.Contains(t, paths, "PATCH /api/v1/trails/gh/acme/widget/7/reviews/review-one/comments/finding-one")
			}
		})
	}
}

// Resume hints are plain commands: project trails need no opt-in prefix.
func TestProjectResumeCommandsNeedNoOptIn(t *testing.T) {
	t.Parallel()
	ctx := buildTrailResumeContext(api.TrailResource{
		ID: projectTrailTestChange, Number: 7, Branch: "feature/work",
		Parent: &api.TrailParentReference{ID: projectTrailTestID, Number: 42, Host: "gh", Project: "acme"},
	}, nil, "", 0, trailResumeFindingsContext{})
	for _, command := range ctx.Commands {
		require.True(t, strings.HasPrefix(command, "entire trail "), command)
		require.Contains(t, command, "42 --project gh/acme --branch feature/work")
	}
}

// Legacy checkout and resume act on the local clone and, as on main, accept a
// trail that has no number yet; only number-keyed subresources (approvals)
// require one. Serial: changes CWD and replaces global clients.
func TestLegacyLocalContextAcceptsUnnumberedTrail(t *testing.T) {
	repoDir := t.TempDir()
	testutil.InitRepo(t, repoDir)
	testutil.IsolateGitConfigEnv(t)
	testutil.RunGit(t, repoDir, "remote", "add", "origin", "git@github.com:acme/widget.git")
	t.Chdir(repoDir)
	setupWorkingRepoClient(t, func(w http.ResponseWriter, _ *http.Request) {
		item := api.TrailResource{ID: projectTrailTestChange, Branch: "feature/work", Title: "Unnumbered", Status: "open"}
		assert.NoError(t, json.NewEncoder(w).Encode(api.TrailListResponse{Trails: []api.TrailResource{item}}))
	})

	root := newTrailCmdForMode(false)
	for _, tc := range []struct {
		command   string
		localOnly bool
		wantErr   string
	}{
		{"checkout", true, ""},
		{"resume", true, ""},
		{"approvals", false, "trail has no number yet"},
	} {
		cmd, _, err := root.Find([]string{tc.command})
		require.NoError(t, err)
		cmd.SetContext(t.Context())
		selected, err := legacyTrailMode.workingContext(cmd, "", "feature/work", tc.localOnly)
		if tc.wantErr != "" {
			require.ErrorContains(t, err, tc.wantErr, tc.command)
			continue
		}
		require.NoError(t, err, tc.command)
		require.Equal(t, "feature/work", selected.Work.Branch, tc.command)
		require.Nil(t, selected.Target, tc.command)
	}
}
