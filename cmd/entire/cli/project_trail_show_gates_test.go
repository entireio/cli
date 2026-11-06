package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/api"
)

const projectTrailGatesFixture = `{"head_sha":"abc1234def","mergeable":false,"conflict_status":"clean",` +
	`"checks":{"availability":"available","runs":[{"name":"ci","status":"completed","conclusion":"failure","details_url":"https://ci.example/1"}]},` +
	`"gates":[{"gate_key":"checks","status":"failed","blocking":true,"rationale":"1 check failed"}]}`

// setupProjectTrailGatesTest serves a project trail whose changes span three
// repositories: widget (two open changes), api (one merged change) and web
// (one open change the reader cannot see). It returns the per-repository
// client opens and the repository trail paths read.
func setupProjectTrailGatesTest(t *testing.T) (opens map[string]int, reads func() []string) {
	t.Helper()
	setupProjectTrailTest(t, func(w http.ResponseWriter, _ *http.Request) {
		item := projectTrailTestResource()
		item.URL = "https://entire.io/gh/acme/-/trails/42"
		item.Changes = []api.ChangeSummary{
			{ID: "01ARZ3NDEKTSV4RRFFQ69G5FB1", RepositoryID: "repo-widget", Repository: "widget", Number: 7, Branch: "feature/a", Status: "open"},
			{ID: "01ARZ3NDEKTSV4RRFFQ69G5FB2", RepositoryID: "repo-widget", Repository: "widget", Number: 8, Branch: "feature/b", Status: "draft"},
			{ID: "01ARZ3NDEKTSV4RRFFQ69G5FB3", RepositoryID: "repo-api", Repository: "api", Number: 3, Branch: "feature/c", Status: "merged"},
			{ID: "01ARZ3NDEKTSV4RRFFQ69G5FB4", RepositoryID: "repo-web", Repository: "web", Number: 4, Branch: "feature/d", Status: "open"},
		}
		assert.NoError(t, json.NewEncoder(w).Encode(item))
	})
	var (
		mu   sync.Mutex
		seen []string
	)
	repoServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.URL.Path)
		mu.Unlock()
		switch r.URL.Path {
		case "/api/v1/trails/gh/acme/widget/7", "/api/v1/trails/gh/acme/widget/8":
			var n int
			_, err := fmt.Sscanf(r.URL.Path, "/api/v1/trails/gh/acme/widget/%d", &n)
			assert.NoError(t, err)
			assert.NoError(t, json.NewEncoder(w).Encode(api.TrailResource{ID: "trail", Number: n, Mergeability: json.RawMessage(projectTrailGatesFixture)}))
		default:
			http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
		}
	}))
	t.Cleanup(repoServer.Close)
	opens = map[string]int{}
	old := newTrailAPIClient
	newTrailAPIClient = func(_ context.Context, _ bool, _, _, repo string) (*api.Client, string, error) {
		mu.Lock()
		opens[repo]++
		mu.Unlock()
		return api.NewClientWithBaseURL("repo-token", repoServer.URL), "repo-" + repo, nil
	}
	t.Cleanup(func() { newTrailAPIClient = old })
	return opens, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), seen...)
	}
}

func TestProjectTrailShowJSONIncludesChangeGates(t *testing.T) {
	opens, reads := setupProjectTrailGatesTest(t)

	out, errOut, err := executeProjectTrailTest(t, "show", "42", "--project", "gh/acme", "--json")
	require.NoError(t, err)
	assert.Equal(t, map[string]int{"widget": 1, "web": 1}, opens, "each repository opens once; a merged change's repository is not opened")
	assert.NotContains(t, reads(), "/api/v1/trails/gh/acme/api/3")
	assert.Equal(t, "Warning: could not load merge gates for 1 change(s)\n", errOut)

	var raw struct {
		Changes []map[string]json.RawMessage `json:"changes"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &raw), "stdout stays JSON when a read fails")
	require.Len(t, raw.Changes, 4)
	for i, c := range raw.Changes {
		require.Contains(t, c, "mergeability", "change %d always carries the key", i)
	}
	for _, i := range []int{0, 1} {
		var mg trailMergeabilityJSON
		require.NoError(t, json.Unmarshal(raw.Changes[i]["mergeability"], &mg))
		assert.False(t, mg.Mergeable)
		require.Len(t, mg.Gates, 1)
		assert.Equal(t, "checks", mg.Gates[0].GateKey)
		require.Len(t, mg.Checks.Runs, 1)
		require.NotNil(t, mg.Checks.Runs[0].DetailsURL)
		assert.Equal(t, "https://ci.example/1", *mg.Checks.Runs[0].DetailsURL)
	}
	assert.JSONEq(t, "null", string(raw.Changes[2]["mergeability"]), "a merged change is not read")
	assert.JSONEq(t, "null", string(raw.Changes[3]["mergeability"]), "an unreadable change is null, not mergeable: false")
}

func TestProjectTrailShowTextIncludesChangeGates(t *testing.T) {
	setupProjectTrailGatesTest(t)

	out, _, err := executeProjectTrailTest(t, "show", "42", "--project", "gh/acme")
	require.NoError(t, err)
	require.Regexp(t, `(?s)  Changes:\n.*`+
		`  Gates for widget/7:\n    Mergeable: no\n    Head:      abc1234d\n    Conflicts: clean\n    Gates:\n      ✗ checks  failed  1 check failed\n.*`+
		`  Gates for widget/8:\n.*\nDescription:\n`, out, "gates sit under the changes, before the description")
	require.NotContains(t, out, "Gates for api/3", "a merged change prints no gates")
	require.NotContains(t, out, "Gates for web/4", "an unreadable change prints no gates")
}

// The change a trail is reached through gets its gates even once merged, so
// `trail show <repo>/<number>` still shows why it was mergeable. Its gates
// are read through the repository client the lookup already opened, so a
// one-repository trail resolves that repository once.
func TestProjectTrailShowCurrentMergedChangeGates(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"branch", []string{"--repo", "gh/acme/widget", "--branch", "feature/a"}},
		{"change selector", []string{"widget/7", "--project", "gh/acme"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setupProjectTrailTest(t, func(w http.ResponseWriter, _ *http.Request) {
				item := projectTrailTestResource()
				item.Changes[0].Repository, item.Changes[0].RepositoryID, item.Changes[0].Status = "", "repo-widget", "merged"
				assert.NoError(t, json.NewEncoder(w).Encode(item))
			})
			parent := &api.TrailParentReference{ID: projectTrailTestID, Number: 42, ProjectID: projectTrailTestProject,
				Host: "gh", Project: "acme", Path: projectTrailTestPath, Jurisdiction: "eu", PrimaryProcessingCell: "project-cell"}
			repoServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/v1/trails/gh/acme/widget":
					assert.NoError(t, json.NewEncoder(w).Encode(api.TrailListResponse{Trails: []api.TrailResource{{
						ID: projectTrailTestChange, Number: 7, Branch: "feature/a", Parent: parent,
					}}}))
				case "/api/v1/trails/gh/acme/widget/7":
					assert.NoError(t, json.NewEncoder(w).Encode(api.TrailResource{ID: projectTrailTestChange, Number: 7, Branch: "feature/a",
						Parent: parent, Mergeability: json.RawMessage(projectTrailGatesFixture)}))
				default:
					t.Errorf("unexpected repository request: %s", r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			t.Cleanup(repoServer.Close)
			opens := 0
			old := newTrailAPIClient
			newTrailAPIClient = func(context.Context, bool, string, string, string) (*api.Client, string, error) {
				opens++
				return api.NewClientWithBaseURL("repo-token", repoServer.URL), "repo-widget", nil
			}
			t.Cleanup(func() { newTrailAPIClient = old })

			out, errOut, err := executeProjectTrailTest(t, append([]string{"show", "--json"}, tc.args...)...)
			require.NoError(t, err)
			require.Empty(t, errOut)
			var item struct {
				Changes []struct {
					Current      bool                   `json:"current"`
					Mergeability *trailMergeabilityJSON `json:"mergeability"`
				} `json:"changes"`
			}
			require.NoError(t, json.Unmarshal([]byte(out), &item))
			require.Len(t, item.Changes, 1)
			require.True(t, item.Changes[0].Current)
			require.NotNil(t, item.Changes[0].Mergeability, "the current change is read by the name the CLI resolved it in")
			assert.Equal(t, 1, opens, "the gates reuse the repository client the lookup opened")
		})
	}
}

// Commands that print a project trail without reading gates omit the key
// rather than claiming every change is unknown.
func TestProjectTrailPrintWithoutGatesOmitsKey(t *testing.T) {
	t.Parallel()
	cmd := newProjectTrailShowCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	require.NoError(t, cmd.Flags().Set("json", "true"))
	require.NoError(t, printProjectTrail(cmd, projectTrailTestResource(), nil, nil))
	require.NotContains(t, out.String(), "mergeability")
}

// A repository whose ID does not match the trail's change is not read, and
// counts as a failure rather than attaching another repository's gates.
func TestProjectTrailShowGatesRepoMismatchIsUnread(t *testing.T) {
	setupProjectTrailGatesTest(t)
	old := newTrailAPIClient
	newTrailAPIClient = func(context.Context, bool, string, string, string) (*api.Client, string, error) {
		return api.NewClientWithBaseURL("repo-token", "http://127.0.0.1:1"), "someone-else", nil
	}
	t.Cleanup(func() { newTrailAPIClient = old })

	out, errOut, err := executeProjectTrailTest(t, "show", "42", "--project", "gh/acme", "--json")
	require.NoError(t, err)
	assert.Equal(t, "Warning: could not load merge gates for 3 change(s)\n", errOut)
	var item struct {
		Changes []struct {
			Mergeability *trailMergeabilityJSON `json:"mergeability"`
		} `json:"changes"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &item))
	for _, c := range item.Changes {
		assert.Nil(t, c.Mergeability)
	}
}

// Past the lookup cap the current change is still read, and the reader is
// told how many changes were not.
func TestProjectTrailShowGatesCapKeepsCurrent(t *testing.T) {
	const extra = 5
	setupProjectTrailTest(t, func(w http.ResponseWriter, _ *http.Request) {
		item := projectTrailTestResource()
		item.Changes = nil
		for i := range trailChangeReviewLimit + extra {
			item.Changes = append(item.Changes, api.ChangeSummary{ID: fmt.Sprintf("other-%d", i), RepositoryID: "repo-widget",
				Repository: "widget", Number: 100 + i, Status: "open"})
		}
		item.Changes = append(item.Changes, api.ChangeSummary{ID: projectTrailTestChange, RepositoryID: "repo-widget",
			Repository: "widget", Number: 7, Branch: "feature/a", Status: "open"})
		assert.NoError(t, json.NewEncoder(w).Encode(item))
	})
	repoServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/trails/gh/acme/widget" {
			assert.NoError(t, json.NewEncoder(w).Encode(api.TrailListResponse{Trails: []api.TrailResource{{
				ID: projectTrailTestChange, Number: 7, Branch: "feature/a",
				Parent: &api.TrailParentReference{ID: projectTrailTestID, Number: 42, ProjectID: projectTrailTestProject,
					Host: "gh", Project: "acme", Path: projectTrailTestPath, Jurisdiction: "eu", PrimaryProcessingCell: "project-cell"},
			}}}))
			return
		}
		assert.NoError(t, json.NewEncoder(w).Encode(api.TrailResource{ID: "trail", Number: 1, Mergeability: json.RawMessage(projectTrailGatesFixture)}))
	}))
	t.Cleanup(repoServer.Close)
	old := newTrailAPIClient
	newTrailAPIClient = func(context.Context, bool, string, string, string) (*api.Client, string, error) {
		return api.NewClientWithBaseURL("repo-token", repoServer.URL), "repo-widget", nil
	}
	t.Cleanup(func() { newTrailAPIClient = old })

	out, errOut, err := executeProjectTrailTest(t, "show", "--repo", "gh/acme/widget", "--branch", "feature/a", "--json")
	require.NoError(t, err)
	assert.Equal(t, fmt.Sprintf("Merge gates are shown for the first %d changes; %d more were not read.\n", trailChangeReviewLimit, extra+1), errOut)
	var item struct {
		Changes []struct {
			Current      bool                   `json:"current"`
			Mergeability *trailMergeabilityJSON `json:"mergeability"`
		} `json:"changes"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &item))
	current := item.Changes[len(item.Changes)-1]
	require.True(t, current.Current)
	require.NotNil(t, current.Mergeability, "the current change is read even past the cap")
	read := 0
	for _, c := range item.Changes {
		if c.Mergeability != nil {
			read++
		}
	}
	assert.Equal(t, trailChangeReviewLimit, read)
}

// A change read successfully whose server has no snapshot yet is null in
// --json, like the repository trail's own --json, but unlike a failed read it
// raises no warning; the text view shows it as unknown.
func TestProjectTrailShowServedNullGates(t *testing.T) {
	setupProjectTrailTest(t, func(w http.ResponseWriter, _ *http.Request) {
		item := projectTrailTestResource()
		item.Changes = []api.ChangeSummary{{ID: "01ARZ3NDEKTSV4RRFFQ69G5FB1", RepositoryID: "repo-widget", Repository: "widget", Number: 7, Branch: "feature/a", Status: "open"}}
		assert.NoError(t, json.NewEncoder(w).Encode(item))
	})
	repoServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/api/v1/trails/gh/acme/widget/7", r.URL.Path)
		assert.NoError(t, json.NewEncoder(w).Encode(api.TrailResource{ID: "trail", Number: 7, Mergeability: json.RawMessage("null")}))
	}))
	t.Cleanup(repoServer.Close)
	old := newTrailAPIClient
	newTrailAPIClient = func(context.Context, bool, string, string, string) (*api.Client, string, error) {
		return api.NewClientWithBaseURL("repo-token", repoServer.URL), "repo-widget", nil
	}
	t.Cleanup(func() { newTrailAPIClient = old })

	out, errOut, err := executeProjectTrailTest(t, "show", "42", "--project", "gh/acme", "--json")
	require.NoError(t, err)
	assert.Empty(t, errOut, "no snapshot is not a failed read")
	var raw struct {
		Changes []map[string]json.RawMessage `json:"changes"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &raw))
	require.Len(t, raw.Changes, 1)
	assert.JSONEq(t, "null", string(raw.Changes[0]["mergeability"]))

	out, errOut, err = executeProjectTrailTest(t, "show", "42", "--project", "gh/acme")
	require.NoError(t, err)
	assert.Empty(t, errOut)
	assert.Contains(t, out, "  Gates for widget/7:\n    Mergeable: unknown\n")
}
