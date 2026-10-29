package cli

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

func TestTrailChangeChoice(t *testing.T) {
	t.Parallel()
	parent := api.ProjectTrail{Number: 42, Changes: []api.ChangeSummary{
		{ID: "a", RepositoryID: "one", Repository: "cli", Number: 11, Title: "Add merge", Branch: "feature/a"},
		{ID: "b", RepositoryID: "one", Repository: "cli", Number: 12, Branch: "feature/b"},
		{ID: "c", RepositoryID: "two", Repository: "api", Number: 21, Title: "Expose mergeability", Branch: "feature/a"},
		{ID: "merged", RepositoryID: "three", Repository: "web", Number: 31, Title: "Merged work"},
	}}
	retry := func(change string) string { return "entire trail approve " + change }
	for _, tt := range []struct {
		name     string
		choice   trailChangeChoice
		parent   *api.ProjectTrail
		id       string
		failures []string
		absent   []string
	}{
		{name: "discovered change wins over a same-named branch elsewhere", choice: trailChangeChoice{ID: "c", Branch: "feature/a", Local: "two"}, id: "c"},
		{name: "discovered change hidden from the trail", choice: trailChangeChoice{ID: "gone", Branch: "feature/gone"},
			failures: []string{`trail #42 does not list branch "feature/gone"`}},
		{name: "explicit branch in scope", choice: trailChangeChoice{Scope: "one", Branch: "feature/b", Preferred: "feature/a", Local: "one"}, id: "b"},
		{name: "checked-out branch in this clone", choice: trailChangeChoice{Local: "one", Preferred: "feature/b"}, id: "b"},
		{name: "checked-out branch never picks another repository", choice: trailChangeChoice{Local: "three", Preferred: "feature/a"},
			failures: []string{"trail #42 has 3 open changes; choose one:", "cli/11 - Add merge", "cli/12 - branch feature/b", "api/21 - Expose mergeability", "Run, for example: entire trail approve cli/11"},
			absent:   []string{"web/31"}},
		{name: "sole open change in another repository", choice: trailChangeChoice{Local: "three"},
			parent: &api.ProjectTrail{Number: 42, Changes: []api.ChangeSummary{parent.Changes[2], parent.Changes[3]}}, id: "c"},
		{name: "sole change in --repo", choice: trailChangeChoice{Scope: "two"}, id: "c"},
		{name: "explicit branch ambiguous across repositories", choice: trailChangeChoice{Local: "three", Branch: "feature/a"},
			failures: []string{"trail #42 has 2 open changes; choose one:", "cli/11 - Add merge", "api/21 - Expose mergeability"},
			absent:   []string{"cli/12"}},
		{name: "explicit branch missing", choice: trailChangeChoice{Scope: "one", Branch: "missing"},
			failures: []string{`trail #42 has no open change on branch "missing" in the selected repository`}},
		{name: "local-only lists other repositories as needing a clone", choice: trailChangeChoice{Scope: "one", Local: "one", LocalOnly: true},
			failures: []string{"has 2 open changes in this clone's repository; choose one:", "Changes in other repositories need a clone of that repository:\n  api/21 - Expose mergeability"}},
		{name: "local-only with nothing here", choice: trailChangeChoice{Scope: "three", Local: "three", LocalOnly: true},
			failures: []string{"trail #42 has no open changes in this clone's repository", "need a clone", "cli/11 - Add merge"},
			absent:   []string{"Run, for example"}},
		{name: "local-only branch miss still shows that branch elsewhere", choice: trailChangeChoice{Scope: "three", Local: "three", LocalOnly: true, Branch: "feature/a"},
			failures: []string{`trail #42 has no open change on branch "feature/a" in this clone's repository`, "need a clone of that repository:\n  cli/11 - Add merge\n  api/21 - Expose mergeability"},
			absent:   []string{"cli/12"}},
		{name: "unnumbered change is never suggested as repo/0", choice: trailChangeChoice{},
			parent:   &api.ProjectTrail{Number: 42, Changes: []api.ChangeSummary{{ID: "x", RepositoryID: "one", Repository: "cli", Branch: "feature/x"}, parent.Changes[2]}},
			failures: []string{"branch feature/x (repository not resolved yet; select it with --branch)", "Run, for example: entire trail approve api/21"},
			absent:   []string{"cli/0"}},
		{name: "unresolved repository is named by branch", choice: trailChangeChoice{},
			parent:   &api.ProjectTrail{Number: 42, Changes: []api.ChangeSummary{{ID: "x", RepositoryID: "one", Branch: "feature/x"}, parent.Changes[2]}},
			failures: []string{"branch feature/x (repository not resolved yet; select it with --branch)", "Run, for example: entire trail approve api/21"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			from := parent
			if tt.parent != nil {
				from = *tt.parent
			}
			got, err := tt.choice.selectFrom(from, retry, nil)
			if len(tt.failures) > 0 {
				require.Error(t, err)
				for _, want := range tt.failures {
					require.Contains(t, err.Error(), want)
				}
				for _, unwanted := range tt.absent {
					require.NotContains(t, err.Error(), unwanted)
				}
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.id, got.ID)
		})
	}
}

func TestSummarizeTrailApprovals(t *testing.T) {
	t.Parallel()
	at := func(hour int) time.Time { return time.Date(2026, 10, 20, hour, 0, 0, 0, time.UTC) }
	for _, tt := range []struct {
		name      string
		approvals []api.TrailApproval
		want      string
	}{
		{"none", nil, "no reviews yet"},
		{"latest decision per reviewer", []api.TrailApproval{
			{Author: "bob", Event: "approve", CreatedAt: at(9)},
			{Author: "alice", Event: "approve", CreatedAt: at(10)},
			{Author: "bob", Event: "request_changes", CreatedAt: at(11)},
			{Author: "carol", Event: "approve", CreatedAt: at(12)},
		}, "approved by alice, carol; changes requested by bob"},
		{"repeat approvals count once", []api.TrailApproval{
			{Author: "alice", Event: "approve", CreatedAt: at(10)},
			{Author: "alice", Event: "approve", CreatedAt: at(11)},
		}, "approved by alice"},
		{"anonymous records are ignored", []api.TrailApproval{{Event: "approve", CreatedAt: at(10)}}, "no reviews yet"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tt.want, summarizeTrailApprovals(tt.approvals).String())
		})
	}
}

func TestTrailChangeChoiceListsReviewState(t *testing.T) {
	t.Parallel()
	parent := api.ProjectTrail{Number: 42, Changes: []api.ChangeSummary{
		{ID: "a", RepositoryID: "one", Repository: "cli", Number: 11, Title: "Add merge", Branch: "feature/a"},
		{ID: "b", RepositoryID: "two", Repository: "api", Number: 21, Title: "Expose mergeability", Branch: "feature/b"},
		{ID: "c", RepositoryID: "three", Repository: "web", Number: 31, Title: "Show state", Branch: "feature/c"},
	}}
	var asked []string
	reviews := func(changes []api.ChangeSummary) map[string]trailChangeReviews {
		for _, item := range changes {
			asked = append(asked, item.ID)
		}
		// c's lookup failed: it is listed without state.
		return map[string]trailChangeReviews{"a": {Approved: []string{"alice"}}, "b": {}}
	}
	retry := func(change string) string { return "entire trail approve " + change }
	_, err := trailChangeChoice{PreferUnapproved: true}.selectFrom(parent, retry, reviews)
	require.Error(t, err)
	require.Equal(t, "trail #42 has 3 open changes; choose one:\n"+
		"  cli/11 - Add merge (approved by alice)\n"+
		"  api/21 - Expose mergeability (no reviews yet)\n"+
		"  web/31 - Show state\n"+
		"Run, for example: entire trail approve api/21", err.Error())
	require.Equal(t, []string{"a", "b", "c"}, asked)

	asked = nil
	got, err := trailChangeChoice{Scope: "two"}.selectFrom(parent, retry, reviews)
	require.NoError(t, err)
	require.Equal(t, "b", got.ID)
	require.Empty(t, asked, "a selected change needs no review lookups")
}

// The suggested command keeps what the user typed except the trail and branch
// selection the change replaces: dropping --project would resolve the change
// in origin's project, and dropping -m would leave request-changes incomplete.
func TestTrailChangeRetryCommandKeepsFlags(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name string
		args []string
		want string
	}{
		{"positional selector", []string{"request-changes", "42", "-m", "Fix it", "--project", "gh/acme", "--branch", "feature/a"},
			"trail request-changes widget/7 --message='Fix it' --project=gh/acme"},
		{"trail flag with a finding ID", []string{"finding", "resolve", "finding-one", "--trail", "42", "-m", "done"},
			"trail finding resolve widget/7 finding-one --message=done"},
		{"bool flag", []string{"finding", "list", "42", "--json"}, "trail finding list widget/7 --json"},
		{"explicitly empty list still clears", []string{"update", "42", "--branch", "feat", "--assignee=", "--title", "X"},
			"trail update widget/7 --assignee= --title=X"},
		{"project implied by --repo is spelled out", []string{"approve", "42", "--repo", "gh/other/widget"},
			"trail approve widget/7 --repo=gh/other/widget --project=gh/other"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			root := newTrailCmdForMode(true)
			cmd, rest, err := root.Find(tt.args)
			require.NoError(t, err)
			require.NoError(t, cmd.ParseFlags(rest))
			require.Equal(t, tt.want, trailChangeRetryCommand(cmd, "42", "widget/7", "gh/other"))
		})
	}
}

// approve writes to the change it selects, so with several open changes it
// lists them with their review state, even when one is on the checked-out
// branch, suggests the unapproved one, and never writes.
// Not parallel: changes CWD and replaces client constructors.
func TestWorkingTrailWriteListsChangesInsteadOfGuessing(t *testing.T) {
	repoDir := t.TempDir()
	testutil.InitRepo(t, repoDir)
	testutil.IsolateGitConfigEnv(t)
	testutil.RunGit(t, repoDir, "remote", "add", "origin", "git@github.com:acme/widget.git")
	testutil.WriteFile(t, repoDir, "README.md", "widget")
	testutil.GitAdd(t, repoDir, "README.md")
	testutil.GitCommit(t, repoDir, "initial")
	current := strings.TrimSpace(testutil.RunGit(t, repoDir, "branch", "--show-current"))
	require.NotEmpty(t, current)
	t.Chdir(repoDir)
	parent := projectTrailTestResource()
	parent.Changes = []api.ChangeSummary{
		{ID: projectTrailTestChange, RepositoryID: "repo-id", Repository: "widget", Number: 7, Title: "Widget work", Branch: current},
		{ID: "01ARZ3NDEKTSV4RRFFQ69G5FAY", RepositoryID: "api-id", Repository: "api", Number: 9, Title: "API work", Branch: "feature/api"},
	}
	setupProjectTrailTest(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodGet, r.Method)
		assert.Contains(t, []string{"/api/v1/gh/acme/trails/42", projectTrailTestPath}, r.URL.Path)
		assert.NoError(t, json.NewEncoder(w).Encode(parent))
	})
	// Both repositories share one stub cell; reads only, never a write.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("must not touch repository work: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
			return
		}
		switch r.URL.Path {
		case "/api/v1/trails/gh/acme/widget/7/approvals":
			// alice requested changes, then approved: her latest decision counts.
			assert.NoError(t, json.NewEncoder(w).Encode(map[string]any{"approvals": []map[string]any{
				{"id": "1", "author": "alice", "event": "request_changes", "created_at": "2026-10-20T10:00:00Z"},
				{"id": "2", "author": "alice", "event": "approve", "created_at": "2026-10-20T11:00:00Z"},
			}}))
		case "/api/v1/trails/gh/acme/api/9/approvals":
			assert.NoError(t, json.NewEncoder(w).Encode(map[string]any{"approvals": []any{}}))
		default:
			// The failed command also re-probes enablement.
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	old := newTrailAPIClient
	newTrailAPIClient = func(_ context.Context, _ bool, _, _, repo string) (*api.Client, string, error) {
		return api.NewClientWithBaseURL("repo-token", server.URL), map[string]string{"widget": "repo-id", "api": "api-id"}[repo], nil
	}
	t.Cleanup(func() { newTrailAPIClient = old })

	_, _, err := executeProjectTrailTest(t, "approve", "42")
	require.Error(t, err)
	require.Contains(t, err.Error(), "trail #42 has 2 open changes; choose one:\n  widget/7 - Widget work (approved by alice)\n  api/9 - API work (no reviews yet)")
	require.Contains(t, err.Error(), "Run, for example: trail approve api/9")
}

// A trail whose only open change lives in another repository acts on that
// repository's work without --repo. Not parallel: changes CWD and replaces
// client constructors.
func TestWorkingTrailSoleChangeInAnotherRepository(t *testing.T) {
	repoDir := t.TempDir()
	testutil.InitRepo(t, repoDir)
	testutil.IsolateGitConfigEnv(t)
	testutil.RunGit(t, repoDir, "remote", "add", "origin", "git@github.com:acme/widget.git")
	t.Chdir(repoDir)
	const apiChange = "01ARZ3NDEKTSV4RRFFQ69G5FAY"
	parent := projectTrailTestResource()
	parent.Changes = []api.ChangeSummary{
		{ID: projectTrailTestChange, RepositoryID: "repo-id", Repository: "widget", Number: 7, Title: "Merged widget work"},
		{ID: apiChange, RepositoryID: "api-id", Repository: "api", Number: 9, Title: "API work", Branch: "feature/api"},
	}
	setupProjectTrailTest(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/gh/acme/trails/42", projectTrailTestPath:
			w.Header().Set("ETag", `W/"parent-version"`)
			assert.NoError(t, json.NewEncoder(w).Encode(parent))
		case projectTrailTestPath + "/changes/" + apiChange:
			assert.NoError(t, json.NewEncoder(w).Encode(map[string]any{"id": apiChange, "number": 9, "trailId": projectTrailTestID, "repositoryId": "api-id", "branch": "feature/api", "status": "open"}))
		default:
			t.Errorf("unexpected project request: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	})
	var writes []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writes = append(writes, r.Method+" "+r.URL.Path)
		w.WriteHeader(http.StatusCreated)
	}))
	t.Cleanup(server.Close)
	old := newTrailAPIClient
	var opened []string
	newTrailAPIClient = func(_ context.Context, _ bool, forge, owner, repo string) (*api.Client, string, error) {
		opened = append(opened, forge+"/"+owner+"/"+repo)
		return api.NewClientWithBaseURL("repo-token", server.URL), map[string]string{"widget": "repo-id", "api": "api-id"}[repo], nil
	}
	t.Cleanup(func() { newTrailAPIClient = old })

	out, _, err := executeProjectTrailTest(t, "approve", "42")
	require.NoError(t, err)
	require.Equal(t, []string{"gh/acme/widget", "gh/acme/api"}, opened)
	require.Equal(t, []string{"POST /api/v1/trails/gh/acme/api/9/approvals"}, writes)
	require.Contains(t, out, "gh/acme/api / feature/api")
	// Nothing about widget was read, so its trails enablement stays unknown.
	scope, err := currentTrailEnablementScope(t.Context())
	require.NoError(t, err)
	require.Equal(t, trailEnablementCacheUnknown, cachedTrailsEnablementForScope(t.Context(), scope, time.Now()))
}

// finding add reads a patch or file location from this worktree, so it must
// not attach them to a sole change in another repository that happens to
// share the path. Not parallel: changes CWD and replaces client constructors.
func TestWorkingTrailFindingAddWithLocalAnchorStaysInThisClone(t *testing.T) {
	repoDir := t.TempDir()
	testutil.InitRepo(t, repoDir)
	testutil.IsolateGitConfigEnv(t)
	testutil.RunGit(t, repoDir, "remote", "add", "origin", "git@github.com:acme/widget.git")
	testutil.WriteFile(t, repoDir, "README.md", "widget")
	t.Chdir(repoDir)
	parent := projectTrailTestResource()
	parent.Changes = []api.ChangeSummary{{ID: "01ARZ3NDEKTSV4RRFFQ69G5FAY", RepositoryID: "api-id", Repository: "api", Number: 9, Title: "API work", Branch: "feature/api"}}
	setupProjectTrailTest(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodGet, r.Method)
		assert.NoError(t, json.NewEncoder(w).Encode(parent))
	})
	setupWorkingRepoClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("must not post a finding: %s %s", r.Method, r.URL.Path)
		}
		http.NotFound(w, r)
	})

	_, _, err := executeProjectTrailTest(t, "finding", "add", "42", "-m", "Check this", "--file", "README.md", "--line", "1")
	require.Error(t, err)
	require.Contains(t, err.Error(), "trail #42 has no open changes in this clone's repository")
	require.Contains(t, err.Error(), "need a clone of that repository:\n  api/9 - API work")
}

// Not parallel: replaces the repository client constructor with an isolated cell.
func setupWorkingRepoClient(t *testing.T, handler http.HandlerFunc) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	old := newTrailAPIClient
	newTrailAPIClient = func(context.Context, bool, string, string, string) (*api.Client, string, error) {
		return api.NewClientWithBaseURL("repo-token", server.URL), "repo-id", nil
	}
	t.Cleanup(func() { newTrailAPIClient = old })
}

func workingProjectTestResource() api.ProjectTrail {
	parent := projectTrailTestResource()
	parent.Changes = []api.ChangeSummary{{ID: projectTrailTestChange, RepositoryID: "repo-id", Repository: "widget", Branch: "feature/work"}}
	return parent
}

func serveWorkingProjectRead(t *testing.T, w http.ResponseWriter, r *http.Request) bool {
	t.Helper()
	if r.Method != http.MethodGet {
		return false
	}
	switch r.URL.Path {
	case "/api/v1/gh/acme/trails/42", projectTrailTestPath:
		w.Header().Set("ETag", `W/"parent-version"`)
		assert.NoError(t, json.NewEncoder(w).Encode(workingProjectTestResource()))
	case projectTrailTestPath + "/changes/" + projectTrailTestChange:
		assert.NoError(t, json.NewEncoder(w).Encode(map[string]any{"id": projectTrailTestChange, "number": 7, "trailId": projectTrailTestID, "repositoryId": "repo-id", "branch": "feature/work", "base": "main", "status": "open"}))
	default:
		return false
	}
	return true
}

func TestWorkingTrailCommandsKeepProjectAndRepoNumbersSeparate(t *testing.T) {
	// Replaces constructors; neither this test nor its subtests may run in parallel.
	for _, tt := range []struct {
		args                  []string
		method, suffix, event string
	}{
		{[]string{"approve", "42"}, http.MethodPost, "/approvals", "approve"},
		{[]string{"approvals", "42"}, http.MethodGet, "/approvals", ""},
		{[]string{"finding", "list", "42", "--json"}, http.MethodGet, "/reviews/comments", ""},
	} {
		t.Run(tt.args[0], func(t *testing.T) {
			setupProjectTrailTest(t, func(w http.ResponseWriter, r *http.Request) {
				if !serveWorkingProjectRead(t, w, r) {
					t.Errorf("unexpected project request: %s %s", r.Method, r.URL.Path)
					http.NotFound(w, r)
				}
			})
			calls := 0
			setupWorkingRepoClient(t, func(w http.ResponseWriter, r *http.Request) {
				calls++
				assert.Equal(t, tt.method, r.Method)
				assert.Equal(t, "/api/v1/trails/gh/acme/widget/7"+tt.suffix, r.URL.Path)
				if tt.event != "" {
					var request api.TrailApprovalRequest
					assert.NoError(t, json.NewDecoder(r.Body).Decode(&request))
					assert.Equal(t, tt.event, request.Event)
				}
				assert.NoError(t, json.NewEncoder(w).Encode(map[string]any{"approvals": []any{}, "comments": []any{}}))
			})
			args := append(append([]string{}, tt.args...), "--repo", "gh/acme/widget", "--branch", "feature/work")
			out, _, err := executeProjectTrailTest(t, args...)
			require.NoError(t, err)
			require.Equal(t, 1, calls)
			if tt.args[0] == "approvals" {
				require.Contains(t, out, "No approvals on trail #42")
			}
			if tt.args[0] != "finding" {
				require.Contains(t, out, "trail #42")
				require.Contains(t, out, "feature/work")
				require.NotContains(t, out, "trail #7")
			}
		})
	}
}

func TestWorkingTrailFindingResolveUsesReviewIDFromCellPayload(t *testing.T) {
	setupProjectTrailTest(t, func(w http.ResponseWriter, r *http.Request) {
		if !serveWorkingProjectRead(t, w, r) {
			t.Errorf("unexpected project request: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	})
	var paths []string
	setupWorkingRepoClient(t, func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.Method+" "+r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		switch r.Method + " " + r.URL.Path {
		case "GET /api/v1/trails/gh/acme/widget/7/reviews/comments":
			assert.NoError(t, json.NewEncoder(w).Encode(map[string]any{
				"comments": []map[string]any{{"id": "finding-one", "review_id": "review-one", "status": "open"}},
			}))
		case "PATCH /api/v1/trails/gh/acme/widget/7/reviews/review-one/comments/finding-one":
			assert.NoError(t, json.NewEncoder(w).Encode(map[string]any{
				"id": "finding-one", "review_id": "review-one", "status": "resolved",
			}))
		default:
			t.Errorf("unexpected repository request: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	})
	out, _, err := executeProjectTrailTest(t, "finding", "resolve", "42", "finding-one", "--repo", "gh/acme/widget", "--branch", "feature/work")
	require.NoError(t, err)
	require.Contains(t, out, "open → resolved")
	require.Equal(t, []string{
		"GET /api/v1/trails/gh/acme/widget/7/reviews/comments",
		"PATCH /api/v1/trails/gh/acme/widget/7/reviews/review-one/comments/finding-one",
	}, paths)
}

func TestWorkingTrailRejectsChangedOwnershipBeforeWriting(t *testing.T) {
	setupProjectTrailTest(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == projectTrailTestPath {
			assert.NoError(t, json.NewEncoder(w).Encode(workingProjectTestResource()))
			return
		}
		assert.NoError(t, json.NewEncoder(w).Encode(map[string]any{"id": projectTrailTestChange, "number": 7, "trailId": "different-parent", "repositoryId": "repo-id", "branch": "feature/work"}))
	})
	setupWorkingRepoClient(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("must not write after ownership changed: %s %s", r.Method, r.URL.Path)
		http.NotFound(w, r)
	})
	_, _, err := executeProjectTrailTest(t, "approve", projectTrailTestID, "--repo", "gh/acme/widget")
	require.ErrorContains(t, err, "does not match")
}

func TestWorkingTrailBranchDiscoveryDoesNotRequireProjectEnumeration(t *testing.T) {
	// Replaces routing constructors, so subtests cannot run in parallel.
	for _, missingParent := range []bool{false, true} {
		t.Run(map[bool]string{false: "addressed parent", true: "missing parent"}[missingParent], func(t *testing.T) {
			core, _ := setupProjectTrailTest(t, func(w http.ResponseWriter, r *http.Request) {
				if missingParent {
					t.Error("missing parent must not fall back to project discovery")
				}
				if !serveWorkingProjectRead(t, w, r) {
					t.Errorf("unexpected request: %s", r.URL.Path)
					http.NotFound(w, r)
				}
			})
			writes := 0
			setupWorkingRepoClient(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPost {
					writes++
					assert.Equal(t, "/api/v1/trails/gh/acme/widget/7/approvals", r.URL.Path)
					w.WriteHeader(http.StatusCreated)
					return
				}
				var parent *api.TrailParentReference
				if !missingParent {
					parent = &api.TrailParentReference{ID: projectTrailTestID, ProjectID: projectTrailTestProject, Number: 42,
						Host: "gh", Project: "acme", Path: projectTrailTestPath, Jurisdiction: "eu", PrimaryProcessingCell: "project-cell"}
				}
				assert.NoError(t, json.NewEncoder(w).Encode(api.TrailListResponse{Trails: []api.TrailResource{{ID: projectTrailTestChange, Number: 7, Branch: "feature/work", Parent: parent}}}))
			})
			_, _, err := executeProjectTrailTest(t, "approve", "--repo", "gh/acme/widget", "--branch", "feature/work")
			require.Zero(t, core.resolveCalls)
			if missingParent {
				require.ErrorContains(t, err, "no discoverable project trail")
				require.Zero(t, writes)
			} else {
				require.NoError(t, err)
				require.Equal(t, 1, writes)
			}
		})
	}
}

func TestWorkingTrailUnlinkPreservesBranch(t *testing.T) {
	var methods []string
	setupProjectTrailTest(t, func(w http.ResponseWriter, r *http.Request) {
		methods = append(methods, r.Method)
		if serveWorkingProjectRead(t, w, r) {
			return
		}
		assert.Equal(t, http.MethodDelete, r.Method)
		assert.Equal(t, projectTrailTestPath+"/changes/"+projectTrailTestChange, r.URL.Path)
		assert.Equal(t, `W/"parent-version"`, r.Header.Get("If-Match"))
		w.WriteHeader(http.StatusNoContent)
	})
	setupWorkingRepoClient(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unlink must not mutate repository work: %s %s", r.Method, r.URL.Path)
		http.NotFound(w, r)
	})
	out, _, err := executeProjectTrailTest(t, "unlink", projectTrailTestID, "--repo", "gh/acme/widget", "--branch", "feature/work")
	require.NoError(t, err)
	require.Contains(t, out, "trail #42")
	require.Equal(t, []string{http.MethodGet, http.MethodGet, http.MethodDelete}, methods)
}

// A merged change's read has branch null; descriptions name its original branch.
func TestTrailWorkingContextDescriptionNamesMergedBranch(t *testing.T) {
	t.Parallel()
	selected := trailWorkingContext{Target: &projectTrailTarget{}, Parent: api.ProjectTrail{Number: 42},
		Host: "gh", Owner: "acme", Repo: "widget", Work: api.TrailResource{Number: 7, OriginalBranch: "feature/old"}}
	require.Equal(t, "trail #42 (gh/acme/widget / feature/old)", selected.description())
}

// watch labels a project change by its branch, naming a merged change's
// original branch rather than ending in a bare " / ".
func TestTrailWatchTargetDescription(t *testing.T) {
	t.Parallel()
	parent := &api.TrailParentReference{ID: projectTrailTestID, Number: 42}
	for _, tt := range []struct {
		name string
		work api.TrailResource
		want string
	}{
		{"project branch", api.TrailResource{ID: projectTrailTestChange, Number: 7, Branch: "feature/x", Parent: parent},
			"trail #42 (gh/acme/widget, id " + projectTrailTestID + ") / feature/x"},
		{"project merged", api.TrailResource{ID: projectTrailTestChange, Number: 7, OriginalBranch: "feature/old", Parent: parent},
			"trail #42 (gh/acme/widget, id " + projectTrailTestID + ") / feature/old"},
		{"legacy", api.TrailResource{ID: projectTrailTestChange, Number: 7, Branch: "feature/x"},
			"trail #7 (gh/acme/widget, id " + projectTrailTestChange + ")"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tt.want, trailWatchTargetDescription(trailReviewTarget{Host: "gh", Owner: "acme", Repo: "widget", Trail: tt.work}))
		})
	}
}

// Branch discovery already found this repository's change, so a same-named
// branch in another repository of the trail must not make it ambiguous.
// Not parallel: changes CWD and replaces client constructors.
func TestWorkingTrailBranchDiscoveryIgnoresSameBranchElsewhere(t *testing.T) {
	repoDir := t.TempDir()
	testutil.InitRepo(t, repoDir)
	testutil.IsolateGitConfigEnv(t)
	testutil.RunGit(t, repoDir, "remote", "add", "origin", "git@github.com:acme/widget.git")
	testutil.WriteFile(t, repoDir, "README.md", "widget")
	testutil.GitAdd(t, repoDir, "README.md")
	testutil.GitCommit(t, repoDir, "initial")
	testutil.RunGit(t, repoDir, "switch", "-c", "feature/work")
	t.Chdir(repoDir)
	parent := workingProjectTestResource()
	parent.Changes = append(parent.Changes, api.ChangeSummary{ID: "01ARZ3NDEKTSV4RRFFQ69G5FAY", RepositoryID: "api-id", Repository: "api", Number: 9, Branch: "feature/work"})
	setupProjectTrailTest(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case projectTrailTestPath:
			assert.NoError(t, json.NewEncoder(w).Encode(parent))
		default:
			if !serveWorkingProjectRead(t, w, r) {
				t.Errorf("unexpected project request: %s %s", r.Method, r.URL.Path)
				http.NotFound(w, r)
			}
		}
	})
	var writes []string
	setupWorkingRepoClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			writes = append(writes, r.URL.Path)
			w.WriteHeader(http.StatusCreated)
			return
		}
		ref := &api.TrailParentReference{ID: projectTrailTestID, ProjectID: projectTrailTestProject, Number: 42,
			Host: "gh", Project: "acme", Path: projectTrailTestPath, Jurisdiction: "eu", PrimaryProcessingCell: "project-cell"}
		assert.NoError(t, json.NewEncoder(w).Encode(api.TrailListResponse{Trails: []api.TrailResource{{ID: projectTrailTestChange, Number: 7, Branch: "feature/work", Parent: ref}}}))
	})

	_, _, err := executeProjectTrailTest(t, "approve")
	require.NoError(t, err)
	require.Equal(t, []string{"/api/v1/trails/gh/acme/widget/7/approvals"}, writes)
}
