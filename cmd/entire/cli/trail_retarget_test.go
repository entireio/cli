package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const expectedTrailRetargetWarning = "Warning: retargeting may close an Entire-owned shadow proposal and clear the tracked proposal association; human-authored PRs are not closed."

func TestTrailRetargetHelpDisclosesProposalCleanup(t *testing.T) {
	t.Parallel()
	for _, project := range []bool{false, true} {
		t.Run(fmt.Sprintf("project=%t", project), func(t *testing.T) {
			t.Parallel()
			cmd := newTrailCmdForMode(project)
			var out, errOut bytes.Buffer
			cmd.SetOut(&out)
			cmd.SetErr(&errOut)
			cmd.SetArgs([]string{"retarget", "--help"})
			require.NoError(t, cmd.Execute())
			require.Contains(t, out.String(), expectedTrailRetargetWarning)
			require.Empty(t, errOut.String())
		})
	}
}

func TestTrailRetargetWithoutReadingOldBase(t *testing.T) {
	// Serial: replaces API client constructors.
	for _, project := range []bool{false, true} {
		for _, forge := range []string{"gh", "et"} {
			t.Run(fmt.Sprintf("project=%t/%s", project, forge), func(t *testing.T) {
				selector := "7"
				if project {
					selector = "42"
					setupProjectTrailTest(t, func(w http.ResponseWriter, r *http.Request) {
						if !serveWorkingProjectRead(t, w, r) {
							t.Errorf("unexpected project request: %s %s", r.Method, r.URL.Path)
							http.NotFound(w, r)
						}
					})
				}
				path := "/api/v1/trails/gh/acme/widget/7"
				if forge == "et" {
					path = "/api/v1/repos/repo-id/trails/7"
				}
				patches := 0
				var out, errOut bytes.Buffer
				setupWorkingRepoClient(t, func(w http.ResponseWriter, r *http.Request) {
					switch r.Method + " " + r.URL.Path {
					case "GET " + path:
						assert.False(t, project)
						assert.NoError(t, json.NewEncoder(w).Encode(api.TrailResource{ID: projectTrailTestChange, Number: 7, Branch: "feature/work", Base: "deleted/merged-parent", Status: "open"}))
					case "PATCH " + path:
						assert.Equal(t, expectedTrailRetargetWarning+"\n", errOut.String(), "warning must be visible before server mutation")
						assert.Empty(t, out.String(), "success must not be reported before server mutation")
						patches++
						var body map[string]any
						assert.NoError(t, json.NewDecoder(r.Body).Decode(&body))
						assert.Equal(t, map[string]any{"base": "main"}, body, "retarget must not change any other metadata")
						assert.NoError(t, json.NewEncoder(w).Encode(map[string]any{"trail": map[string]any{"number": 7, "base": "main"}}))
					default:
						t.Errorf("must not fetch or resolve the deleted old target: %s %s", r.Method, r.URL.Path)
						http.NotFound(w, r)
					}
				})
				cmd := newTrailCmdForMode(project)
				cmd.SetOut(&out)
				cmd.SetErr(&errOut)
				args := []string{"retarget", selector, "--repo", forge + "/acme/widget", "--base", "main"}
				if project {
					args = append(args, "--project", "gh/acme")
				}
				cmd.SetArgs(args)
				require.NoError(t, cmd.Execute())
				require.Equal(t, 1, patches)
				require.Contains(t, out.String(), "trail #"+selector)
				require.Contains(t, out.String(), "main")
				require.NotContains(t, out.String(), expectedTrailRetargetWarning, "runtime warning belongs only on stderr")
			})
		}
	}
}

func TestTrailRetargetRejectsInvalidInputBeforeRequests(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name    string
		args    []string
		failure string
	}{
		{"missing base", []string{"retarget", "7"}, "--base"},
		{"empty base", []string{"retarget", "7", "--base", ""}, "--base"},
		{"blank base", []string{"retarget", "7", "--base", "  "}, "--base"},
		{"invalid ref", []string{"retarget", "7", "--base", "bad..ref"}, "invalid branch"},
		{"head", []string{"retarget", "7", "--base", "HEAD"}, "invalid branch"},
		{"reflog", []string{"retarget", "7", "--base", "@{-1}"}, "invalid branch"},
		{"extra selector", []string{"retarget", "7", "8", "--base", "main"}, "arg"},
		{"conflicting selector", []string{"retarget", "7", "--branch", "feature/work", "--base", "main"}, "not both"},
		{"remote without target", []string{"retarget", "--repo", "gh/acme/widget", "--base", "main"}, "pass a trail selector or --branch"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cmd := newTrailCmdForMode(false)
			var out, errOut bytes.Buffer
			cmd.SetOut(&out)
			cmd.SetErr(&errOut)
			cmd.SilenceUsage = true
			cmd.SetArgs(tt.args)
			require.ErrorContains(t, cmd.Execute(), tt.failure)
			require.Empty(t, out.String())
			require.NotContains(t, errOut.String(), expectedTrailRetargetWarning)
		})
	}
}

func TestTrailRetargetDegenerateTargets(t *testing.T) {
	// Serial: replaces the repository client constructor.
	for _, tt := range []struct {
		name, branch, base, status, failure string
		code                                int
		patches                             int
	}{
		{"already targeted", "feature/work", "main", "open", "", 200, 0},
		{"branchless", "", "old", "open", "linked branch", 200, 0},
		{"self target", "main", "old", "open", "own branch", 200, 0},
		{"qualified self target", "refs/heads/main", "old", "open", "own branch", 200, 0},
		{"qualified unchanged target", "feature/work", "refs/heads/main", "open", "", 200, 0},
		{"merged work", "feature/work", "old", "merged", "merged", 200, 0},
		{"closed work", "feature/work", "old", "closed", "closed", 200, 0},
		{"draft work", "feature/work", "old", "draft", "", 200, 1},
		{"missing trail", "", "", "", "404", 404, 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			patches := 0
			setupWorkingRepoClient(t, func(w http.ResponseWriter, r *http.Request) {
				switch r.Method {
				case http.MethodGet:
					if tt.code != http.StatusNotFound {
						assert.Equal(t, "/api/v1/trails/gh/acme/widget/7", r.URL.Path)
					}
					w.WriteHeader(tt.code)
					assert.NoError(t, json.NewEncoder(w).Encode(api.TrailResource{ID: projectTrailTestChange, Number: 7, Branch: tt.branch, Base: tt.base, Status: tt.status}))
				case http.MethodPatch:
					patches++
					assert.NoError(t, json.NewEncoder(w).Encode(map[string]any{"trail": map[string]any{"base": "main"}}))
				default:
					t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
					http.NotFound(w, r)
				}
			})
			cmd := newTrailCmdForMode(false)
			var out, errOut bytes.Buffer
			cmd.SetOut(&out)
			cmd.SetErr(&errOut)
			cmd.SilenceUsage = true
			cmd.SetArgs([]string{"retarget", "7", "--repo", "gh/acme/widget", "--base", "main"})
			err := cmd.Execute()
			if tt.failure != "" {
				require.ErrorContains(t, err, tt.failure)
				require.Empty(t, out.String())
			} else {
				require.NoError(t, err)
				require.Contains(t, out.String(), "main")
			}
			require.Equal(t, tt.patches, patches)
			if tt.patches == 0 {
				require.NotContains(t, errOut.String(), expectedTrailRetargetWarning, "no-ops and rejections must not warn about a mutation")
				if tt.failure == "" {
					require.Empty(t, errOut.String())
				}
			}
		})
	}
}

func TestTrailRetargetBranchSelectionPreservesLocalGit(t *testing.T) {
	// Serial: moves CWD to a disposable clone and replaces client constructors.
	dir := setupTrailBranchSelectionTest(t)
	testutil.RunGit(t, dir, "branch", "main")
	testutil.WriteFile(t, dir, "file.txt", "uncommitted work")
	beforeHead := testutil.RunGit(t, dir, "rev-parse", "HEAD")
	beforeStatus := testutil.RunGit(t, dir, "--no-optional-locks", "status", "--porcelain")
	for _, args := range [][]string{
		{"retarget", "--base", "main"},
		{"retarget", "--branch", "feature/work", "--base", "main"},
		{"retarget", "--repo", "gh/acme/widget", "--branch", "feature/work", "--base", "main"},
	} {
		t.Run(fmt.Sprint(args), func(t *testing.T) {
			patches := 0
			setupWorkingRepoClient(t, func(w http.ResponseWriter, r *http.Request) {
				switch r.Method + " " + r.URL.Path {
				case "GET /api/v1/trails/gh/acme/widget":
					assert.NoError(t, json.NewEncoder(w).Encode(api.TrailListResponse{Trails: []api.TrailResource{
						{Number: 8, Branch: "unrelated", Base: "old", Status: "open"},
						{Number: 7, Branch: "feature/work", Base: "deleted/parent", Status: "open"},
					}}))
				case "PATCH /api/v1/trails/gh/acme/widget/7":
					patches++
					assert.NoError(t, json.NewEncoder(w).Encode(map[string]any{"trail": map[string]any{"base": "main"}}))
				default:
					t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
					http.NotFound(w, r)
				}
			})
			cmd := newTrailCmdForMode(false)
			cmd.SetOut(&bytes.Buffer{})
			cmd.SetErr(&bytes.Buffer{})
			cmd.SetArgs(args)
			require.NoError(t, cmd.ExecuteContext(t.Context()))
			require.Equal(t, 1, patches)
			require.Equal(t, beforeHead, testutil.RunGit(t, dir, "rev-parse", "HEAD"))
			require.Equal(t, "feature/work\n", testutil.RunGit(t, dir, "branch", "--show-current"))
			require.Equal(t, beforeStatus, testutil.RunGit(t, dir, "--no-optional-locks", "status", "--porcelain"))
		})
	}
}

func TestTrailRetargetMutationFailureDoesNotReportSuccess(t *testing.T) {
	// Serial: replaces the repository client constructor.
	for _, code := range []int{http.StatusBadRequest, http.StatusForbidden, http.StatusConflict, http.StatusInternalServerError} {
		t.Run(strconv.Itoa(code), func(t *testing.T) {
			patches := 0
			var out, errOut bytes.Buffer
			setupWorkingRepoClient(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					assert.NoError(t, json.NewEncoder(w).Encode(api.TrailResource{Number: 7, Branch: "feature/work", Base: "old", Status: "open"}))
					return
				}
				assert.Equal(t, http.MethodPatch, r.Method)
				assert.Equal(t, expectedTrailRetargetWarning+"\n", errOut.String(), "even rejected mutations must be preceded by the warning")
				assert.Empty(t, out.String())
				patches++
				w.WriteHeader(code)
				_, err := fmt.Fprint(w, `{"error":"target rejected"}`)
				assert.NoError(t, err)
			})
			cmd := newTrailCmdForMode(false)
			cmd.SetOut(&out)
			cmd.SetErr(&errOut)
			cmd.SilenceUsage = true
			cmd.SetArgs([]string{"retarget", "7", "--repo", "gh/acme/widget", "--base", "main"})
			require.Error(t, cmd.Execute())
			require.Empty(t, out.String())
			require.Equal(t, 1, patches)
		})
	}
}
