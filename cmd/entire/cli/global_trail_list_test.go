package cli

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/internal/coreapi"
)

func globalTrailTestItem(n int, jurisdiction string) api.ProjectTrail {
	item := projectTrailTestResource()
	item.ID = fmt.Sprintf("%026d", n)
	item.Project = &api.GlobalTrailProject{ID: item.ProjectID, Jurisdiction: jurisdiction, Provider: "github"}
	item.Project.Reference.Forge, item.Project.Reference.Project = "gh", "acme"
	item.Order = &api.GlobalTrailOrder{TrailID: item.ID, UpdatedAt: time.Date(2026, 9, 17, 12, 0, 0, n, time.UTC).Format("2006-01-02T15:04:05.000000000Z")}
	item.ContinuationToken = "after-" + item.ID
	return item
}

func globalTrailTestPage(items []api.ProjectTrail, jurisdiction string, next *string) api.GlobalTrailListResponse {
	return api.GlobalTrailListResponse{ProjectTrailListResponse: api.ProjectTrailListResponse{Items: items, NextPageToken: next}, Jurisdiction: jurisdiction}
}

// Not parallel: routing/client constructors and (in some tests) cwd are global.
func TestGlobalTrailListMergesAndResumesAcrossCells(t *testing.T) {
	t.Chdir(t.TempDir()) // No implicit repository or project, even outside a clone.
	builder := &fakeCellClientBuilder{}
	withFakeCellClientBuilder(t, builder)
	factoryCalls := 0
	build := newCellClientBuilder
	newCellClientBuilder = func(ctx context.Context, insecure bool) (cellClientBuilder, error) {
		factoryCalls++
		return build(ctx, insecure)
	}
	core, _ := setupProjectTrailTest(t, func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "wrong cell", http.StatusInternalServerError)
	})
	core.clusters = nil
	var mu sync.Mutex
	var requests []string
	for i, ns := range [][]int{{9, 4}, {8, 5}, {7, 6, 3}} {
		region := "eu"
		if i == 2 {
			region = "us"
		}
		items := []api.ProjectTrail{}
		for _, n := range ns {
			items = append(items, globalTrailTestItem(n, region))
		}
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			assert.Equal(t, "/api/v1/trails", r.URL.Path)
			assert.Equal(t, "updated", r.URL.Query().Get("sort"))
			assert.Equal(t, "none", r.URL.Query().Get("groupBy"))
			assert.Empty(t, r.URL.Query().Get("projectId"))
			assert.Empty(t, r.URL.Query().Get("repoId"))
			mu.Lock()
			requests = append(requests, r.URL.String())
			mu.Unlock()
			start := 0
			if token := r.URL.Query().Get("pageToken"); token != "" {
				start = slices.IndexFunc(items, func(item api.ProjectTrail) bool { return item.ContinuationToken == token }) + 1
				assert.Positive(t, start)
			}
			size, err := strconv.Atoi(r.URL.Query().Get("pageSize"))
			assert.NoError(t, err)
			end := min(start+size, len(items))
			var next *string
			if end < len(items) {
				next = &items[end-1].ContinuationToken
			}
			assert.NoError(t, json.NewEncoder(w).Encode(globalTrailTestPage(items[start:end], region, next)))
		}))
		t.Cleanup(server.Close)
		core.clusters = append(core.clusters, coreapi.Cluster{Slug: fmt.Sprintf("cell-%d", i), Jurisdiction: region, ApiUrl: coreapi.NewOptString(server.URL)})
	}
	// Alias catalog rows for one endpoint must not cause duplicate requests.
	core.clusters = append(core.clusters, core.clusters[0])
	var got []string
	cursor := ""
	for pageNum := range 4 {
		args := []string{"list", "--limit", "2", "--json"}
		if cursor != "" {
			args = append(args, "--page-token", cursor)
		}
		out, _, err := executeProjectTrailTest(t, args...)
		require.NoError(t, err)
		var page api.ProjectTrailListResponse
		require.NoError(t, json.Unmarshal([]byte(out), &page))
		for _, item := range page.Items {
			got = append(got, item.ID)
		}
		if pageNum == 3 {
			require.Nil(t, page.NextPageToken)
		} else {
			require.NotNil(t, page.NextPageToken)
			cursor = *page.NextPageToken
		}
	}
	var want []string
	for _, n := range []int{9, 8, 7, 6, 5, 4, 3} {
		want = append(want, fmt.Sprintf("%026d", n))
	}
	require.Equal(t, want, got, "must use sub-millisecond server ordering and retain unconsumed rows")
	require.Zero(t, core.resolveCalls)
	require.Equal(t, 4, factoryCalls, "one shared credential factory per list invocation")
	require.Len(t, requests, 10, "exhausted cells are not queried again")
}

func TestGlobalTrailListExplicitProjectUsesGlobalEndpoint(t *testing.T) {
	withFakeCellClientBuilder(t, &fakeCellClientBuilder{})
	core, _ := setupProjectTrailTest(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/api/v1/trails", r.URL.Path)
		assert.Equal(t, projectTrailTestProject, r.URL.Query().Get("projectId"))
		assert.Equal(t, "closed", r.URL.Query().Get("status"))
		item := globalTrailTestItem(1, "eu")
		item.Status = "closed"
		assert.NoError(t, json.NewEncoder(w).Encode(globalTrailTestPage([]api.ProjectTrail{item}, "eu", nil)))
	})
	core.clusters = nil // Explicit project may live in a hidden assigned cell.
	out, _, err := executeProjectTrailTest(t, "list", "--project", "gh/acme", "--status", "closed")
	require.NoError(t, err)
	require.Contains(t, out, "gh/acme")
	require.Contains(t, out, "closed")
	require.Equal(t, 1, core.resolveCalls)
	require.Zero(t, core.catalogCalls)
}

func TestGlobalTrailListFailureDoesNotEmitPartialPage(t *testing.T) {
	withFakeCellClientBuilder(t, &fakeCellClientBuilder{})
	core, _ := setupProjectTrailTest(t, func(w http.ResponseWriter, _ *http.Request) {
		assert.NoError(t, json.NewEncoder(w).Encode(globalTrailTestPage([]api.ProjectTrail{globalTrailTestItem(1, "eu")}, "eu", nil)))
	})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprint(w, `{"error":"unavailable"}`)
	}))
	t.Cleanup(server.Close)
	core.clusters = append(core.clusters, coreapi.Cluster{Slug: "bad-cell", Jurisdiction: "us", ApiUrl: coreapi.NewOptString(server.URL)})
	out, _, err := executeProjectTrailTest(t, "list", "--json")
	require.ErrorContains(t, err, "HTTP 503")
	require.Empty(t, out)
}

func TestGlobalTrailListCursorBinding(t *testing.T) {
	t.Parallel()
	cells := []cellGroup{{cell: "eu|https://one.example"}, {cell: "eu|https://two.example"}}
	q := url.Values{"status": {"open"}}
	cursor, err := decodeGlobalTrailCursor("", q, cells)
	require.NoError(t, err)
	data, err := json.Marshal(cursor)
	require.NoError(t, err)
	// The command uses base64url; encode through the same standard encoding.
	raw := base64.RawURLEncoding.EncodeToString(data)
	_, err = decodeGlobalTrailCursor(raw, q, cells)
	require.NoError(t, err)
	_, err = decodeGlobalTrailCursor(raw, url.Values{"status": {"closed"}}, cells)
	require.ErrorContains(t, err, "filters or cells changed")
	_, err = decodeGlobalTrailCursor(raw, q, []cellGroup{{cell: "eu|https://attacker.example"}, cells[1]})
	require.ErrorContains(t, err, "cells changed")
	_, err = decodeGlobalTrailCursor("bad", q, cells)
	require.ErrorContains(t, err, "invalid global trail page token")
}

func TestGlobalTrailPageValidation(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name   string
		mutate func(*api.GlobalTrailListResponse)
		want   string
	}{
		{"jurisdiction", func(p *api.GlobalTrailListResponse) { p.Jurisdiction = "us" }, "jurisdiction"},
		{"identity", func(p *api.GlobalTrailListResponse) { p.Items[0].Project.ID = "wrong" }, "identity"},
		{"missing project", func(p *api.GlobalTrailListResponse) { p.Items[0].Project = nil }, "identity"},
		{"reference", func(p *api.GlobalTrailListResponse) { p.Items[0].Project.Reference.Project = "bad/name" }, "reference"},
		{"order", func(p *api.GlobalTrailListResponse) { p.Items[0].Order = nil }, "metadata"},
		{"continuation", func(p *api.GlobalTrailListResponse) { p.Items[0].ContinuationToken = "" }, "metadata"},
		{"timestamp", func(p *api.GlobalTrailListResponse) { p.Items[0].Order.UpdatedAt = "invalid" }, "timestamp"},
		{"status", func(p *api.GlobalTrailListResponse) { p.Items[0].Status = "closed" }, "filters"},
		{"order reversed", func(p *api.GlobalTrailListResponse) { p.Items = append(p.Items, globalTrailTestItem(2, "eu")) }, "strictly ordered"},
		{"empty with next", func(p *api.GlobalTrailListResponse) { p.Items = nil; next := "next"; p.NextPageToken = &next }, "did not advance"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			page := globalTrailTestPage([]api.ProjectTrail{globalTrailTestItem(1, "eu")}, "eu", nil)
			tt.mutate(&page)
			err := validateGlobalTrailPage(page, cellGroup{jurisdiction: "eu"}, url.Values{"status": {"open"}}, 2, "")
			require.ErrorContains(t, err, tt.want)
		})
	}
}

func TestGlobalTrailListValidatesFlagsBeforeIO(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{{"list", "--limit", "0"}, {"list", "--limit", "101"}, {"list", "--status", "merged"}} {
		_, _, err := executeProjectTrailTest(t, args...)
		require.Error(t, err)
	}
}

func TestGlobalTrailListEmptyCatalog(t *testing.T) {
	core, _ := setupProjectTrailTest(t, func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "unexpected", http.StatusInternalServerError)
	})
	core.clusters = nil
	out, _, err := executeProjectTrailTest(t, "list", "--json")
	require.ErrorContains(t, err, "no available cells")
	require.Empty(t, out)
}

// Constructors are global; do not parallelize these routing tests.
func TestGlobalTrailListRepositoryFilter(t *testing.T) {
	for _, explicitProject := range []bool{false, true} {
		t.Run(strconv.FormatBool(explicitProject), func(t *testing.T) {
			withFakeCellClientBuilder(t, &fakeCellClientBuilder{})
			setupProjectTrailRepoPlacement(t)
			core, server := setupProjectTrailTest(t, func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, "/api/v1/trails", r.URL.Path)
				assert.Equal(t, "repo-id", r.URL.Query().Get("repoId"))
				if explicitProject {
					assert.Equal(t, projectTrailTestProject, r.URL.Query().Get("projectId"))
				} else {
					assert.Empty(t, r.URL.Query().Get("projectId"))
				}
				item := globalTrailTestItem(1, "eu")
				item.RepositoryIDs = []string{"repo-id"}
				assert.NoError(t, json.NewEncoder(w).Encode(globalTrailTestPage([]api.ProjectTrail{item}, "eu", nil)))
			})
			repoCore, err := newCellCoreClient()
			require.NoError(t, err)
			fake, ok := repoCore.(*fakeCellCore)
			require.True(t, ok)
			fake.clusters[0].ApiUrl = coreapi.NewOptString(server.URL)
			args := []string{"list", "--repo", "gh/acme/widget", "--json"}
			if explicitProject {
				args = append(args, "--project", "gh/acme")
				// The explicit project destination must win, not the repo's placement.
				fake.clusters[0].ApiUrl = coreapi.NewOptString("https://wrong.example")
			}
			out, _, err := executeProjectTrailTest(t, args...)
			require.NoError(t, err)
			require.Contains(t, out, "repo-id")
			require.Zero(t, core.catalogCalls)
			if explicitProject {
				require.Equal(t, 1, core.resolveCalls)
			} else {
				require.Zero(t, core.resolveCalls)
			}
		})
	}
}

func TestGlobalTrailListRejectsDuplicateRowsAcrossCells(t *testing.T) {
	withFakeCellClientBuilder(t, &fakeCellClientBuilder{})
	handler := func(w http.ResponseWriter, _ *http.Request) {
		assert.NoError(t, json.NewEncoder(w).Encode(globalTrailTestPage([]api.ProjectTrail{globalTrailTestItem(1, "eu")}, "eu", nil)))
	}
	core, _ := setupProjectTrailTest(t, handler)
	server := httptest.NewServer(http.HandlerFunc(handler))
	t.Cleanup(server.Close)
	core.clusters = append(core.clusters, coreapi.Cluster{Slug: "another", Jurisdiction: "eu", ApiUrl: coreapi.NewOptString(server.URL)})
	out, _, err := executeProjectTrailTest(t, "list", "--json")
	require.ErrorContains(t, err, "multiple cells")
	require.Empty(t, out)
}

func TestGlobalTrailListAgentHelpScope(t *testing.T) {
	t.Parallel()
	root := &cobra.Command{Use: "entire"}
	root.AddCommand(newTrailCmd())
	cmd, _, err := root.Find([]string{"trail", "list"})
	require.NoError(t, err)
	out := renderAgentHelpCommand(cmd, "gh/acme/widget", true)
	require.Contains(t, out, "Scope: global")
	require.NotContains(t, out, "pass --repo only for a DIFFERENT repo")
}

func TestGlobalTrailListEmptyResponse(t *testing.T) {
	withFakeCellClientBuilder(t, &fakeCellClientBuilder{})
	setupProjectTrailTest(t, func(w http.ResponseWriter, _ *http.Request) {
		assert.NoError(t, json.NewEncoder(w).Encode(globalTrailTestPage(nil, "eu", nil)))
	})
	out, _, err := executeProjectTrailTest(t, "list", "--json")
	require.NoError(t, err)
	require.JSONEq(t, `{"items":[],"nextPageToken":null}`, out)
}
