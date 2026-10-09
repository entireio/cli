package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/entiredir"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/internal/coreapi"
	"github.com/spf13/cobra"
)

func TestResolveRepoFilters_WhitespaceIDTakesPrecedence(t *testing.T) {
	t.Parallel()
	const id = "01JXYZ123ABC"
	repos := []coreapi.RepoIndexEntry{{ID: id, FullName: "owner/repo"}, {ID: "other", FullName: id}}
	ids, matched := resolveRepoFilters([]string{" \t" + id + "\n", id}, repos)
	if len(ids) != 1 || ids[0] != id || len(matched) != 1 {
		t.Fatalf("ID resolution = %v, %v", ids, matched)
	}
}

func TestDuplicateRepoSlugs_PreserveSearchAndJurisdictions(t *testing.T) {
	t.Parallel()
	repos := []coreapi.RepoIndexEntry{
		{ID: "one", FullName: "Owner/Repo", Provider: coreapi.NewOptString("github"), CheckpointCount: coreapi.NewOptInt64(1), Placements: []coreapi.RepoPlacement{
			{Jurisdiction: "us", Status: coreapi.RepoPlacementStatusReady},
			{Jurisdiction: "eu", Status: coreapi.RepoPlacementStatusReady},
		}},
		{ID: "two", FullName: "gh/owner/repo", Provider: coreapi.NewOptString("github"), CheckpointCount: coreapi.NewOptInt64(1), Placements: []coreapi.RepoPlacement{
			{Jurisdiction: "au", Status: coreapi.RepoPlacementStatusReady},
			{Jurisdiction: "us", Status: coreapi.RepoPlacementStatusReady},
			{Jurisdiction: "ca", Status: coreapi.RepoPlacementStatusProcessing},
		}},
		{ID: "native", FullName: "owner/repo", Provider: coreapi.NewOptString("entire")},
	}
	ids, matched := resolveRepoFilters([]string{"gh/owner/repo", "one", "GH/OWNER/REPO"}, repos)
	if strings.Join(ids, ",") != "one,two" || len(matched) != 2 {
		t.Fatalf("duplicate slug resolution = %v, %v", ids, matched)
	}
	placements := dispatchWizardPlacements(repos)
	if got := strings.Join(placements["gh/owner/repo"], ","); got != "au,eu,us" {
		t.Fatalf("merged jurisdictions = %q", got)
	}
	if got := strings.Join(checkpointRepoSlugs(repos), ","); got != "gh/Owner/Repo" {
		t.Fatalf("picker duplicates = %q", got)
	}
	ids, _ = resolveRepoFilters([]string{"owner/repo"}, []coreapi.RepoIndexEntry{repos[0], repos[2]})
	if strings.Join(ids, ",") != "one,native" {
		t.Fatalf("bare name lost matching entry: %v", ids)
	}
}

func TestCheckpointRepoSlug_ForgeNamedOwners(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ provider, name, want string }{
		{"github", "gh/widget", "gh/gh/widget"},
		{"entire", "et/widget", "et/et/widget"},
		{"github", "gh/gh/widget", "gh/gh/widget"},
		{"entire", "/et/et/widget/", "et/et/widget"},
	} {
		t.Run(tc.provider+"/"+tc.name, func(t *testing.T) {
			t.Parallel()
			entry := coreapi.RepoIndexEntry{FullName: tc.name, Provider: coreapi.NewOptString(tc.provider)}
			if got := checkpointRepoSlug(entry); got != tc.want {
				t.Fatalf("slug = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestResolveRepoFilters_CompletionProviderIsolation(t *testing.T) {
	t.Parallel()
	for _, nativeName := range []string{"project/native", "et/project/native"} {
		t.Run(nativeName, func(t *testing.T) {
			t.Parallel()
			native := coreapi.RepoIndexEntry{ID: "native", FullName: nativeName, Provider: coreapi.NewOptString("entire"), CheckpointCount: coreapi.NewOptInt64(1)}
			github := coreapi.RepoIndexEntry{ID: "github", FullName: "project/native", Provider: coreapi.NewOptString("github"), CheckpointCount: coreapi.NewOptInt64(1)}
			for _, entries := range [][]coreapi.RepoIndexEntry{{native, github}, {github, native}} {
				for _, entry := range entries {
					suggestions := checkpointRepoSlugs([]coreapi.RepoIndexEntry{entry})
					ids, matched := resolveRepoFilters(suggestions, entries)
					if len(ids) != 1 || ids[0] != entry.ID || len(matched) != 1 {
						t.Fatalf("%v resolved to %v, want %s", suggestions, ids, entry.ID)
					}
				}
			}
			if ids, _ := resolveRepoFilters([]string{"et/project/native"}, []coreapi.RepoIndexEntry{github}); len(ids) != 0 {
				t.Fatalf("native filter matched GitHub: %v", ids)
			}
			if ids, _ := resolveRepoFilters([]string{"gh/project/native"}, []coreapi.RepoIndexEntry{native}); len(ids) != 0 {
				t.Fatalf("GitHub filter matched native: %v", ids)
			}
		})
	}
}

func TestCheckpointRepoIndex_PagesAndTruncation(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Query().Get("hasCheckpoints") != "true" {
			t.Errorf("wizard page includes empty repos: %s", r.URL)
		}
		if r.URL.Query().Get("sort") != "last_activity_at" || r.URL.Query().Get("order") != "desc" {
			t.Errorf("not recent-first: %s", r.URL)
		}
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("pageToken") == "" {
			fmt.Fprint(w, `{"repos":[{"id":"new","name":"new","cell":"us","clusterSlug":"us","jurisdiction":"us","visibility":"private","full_name":"owner/new","provider":"github","checkpointCount":3,"placements":[]},{"id":"empty","name":"empty","cell":"us","clusterSlug":"us","jurisdiction":"us","visibility":"private","full_name":"owner/empty","provider":"github","checkpointCount":0,"placements":[]}],"nextPageToken":"next","truncated":true}`)
		} else {
			fmt.Fprint(w, `{"repos":[{"id":"old","name":"old","cell":"us","clusterSlug":"us","jurisdiction":"us","visibility":"private","full_name":"project/old","provider":"entire","checkpointCount":1,"placements":[]}],"truncated":true}`)
		}
	}))
	defer srv.Close()
	old := newCellCoreClient
	newCellCoreClient = func() (cellCoreClient, error) { return coreapi.NewWithBearer(srv.URL, "test") }
	t.Cleanup(func() { newCellCoreClient = old })
	dir := t.TempDir()
	logger, err := logging.New(logging.Config{Root: entiredir.OpenerAt(dir), Dir: logging.LogsName, Level: slog.LevelWarn})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := logger.Close(); err != nil {
			t.Error(err)
		}
	})
	entries, err := listCheckpointRepoIndex(logging.WithLogger(context.Background(), logger))
	if err != nil {
		t.Fatal(err)
	}
	got := append([]string{"*"}, checkpointRepoSlugs(entries)...)
	if err := logger.Close(); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(filepath.Join(dir, logging.LogsDir, logging.LogFileName))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(content), "repo index truncated") {
		t.Fatalf("missing truncation warning: %s", content)
	}
	if strings.Join(got, ",") != "*,gh/owner/new,et/project/old" {
		t.Fatalf("slugs = %v", got)
	}
	if calls.Load() != 2 {
		t.Fatalf("calls = %d", calls.Load())
	}
	ids, _ := resolveRepoFilters(got[2:], []coreapi.RepoIndexEntry{
		{ID: "old", FullName: "project/old", Provider: coreapi.NewOptString("entire")},
		{ID: "mirror", FullName: "project/old", Provider: coreapi.NewOptString("github")},
	})
	if len(ids) != 1 || ids[0] != "old" {
		t.Fatalf("native completion resolved to %v", ids)
	}
}

func TestLoadDispatchWizardScope_OneIndexWalk(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"repos":[{"id":"new","name":"new","cell":"us","clusterSlug":"us","jurisdiction":"us","visibility":"private","full_name":"owner/new","provider":"github","checkpointCount":3,"placements":[{"id":"one","cell":"us","clusterSlug":"us","mirror":true,"jurisdiction":"us","status":"ready"}]},{"id":"native","name":"native","cell":"eu","clusterSlug":"eu","jurisdiction":"eu","visibility":"private","full_name":"project/native","provider":"entire","checkpointCount":1,"placements":[{"id":"two","cell":"eu","clusterSlug":"eu","mirror":false,"jurisdiction":"eu","status":"ready"}]}],"truncated":false}`)
	}))
	defer srv.Close()
	old, oldHome := newCellCoreClient, resolveDispatchWizardHome
	newCellCoreClient = func() (cellCoreClient, error) { return coreapi.NewWithBearer(srv.URL, "test") }
	resolveDispatchWizardHome = func(context.Context) string { return "us" }
	t.Cleanup(func() { newCellCoreClient, resolveDispatchWizardHome = old, oldHome })
	scope := loadDispatchWizardScope(context.Background(), t.TempDir())
	if calls.Load() != 1 {
		t.Fatalf("calls = %d", calls.Load())
	}
	if strings.Join(scope.reposIn("us"), ",") != "gh/owner/new" || strings.Join(scope.reposIn("eu"), ",") != "et/project/native" {
		t.Fatalf("scope = %+v", scope)
	}
}

func TestCompleteRepoFlag_OnePageAndPrefix(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		q := r.URL.Query()
		if q.Get("sort") != "last_activity_at" || q.Get("order") != "desc" || q.Get("pageSize") != "100" || q.Get("q") != "project/old" || q.Get("pageToken") != "" {
			t.Errorf("unexpected query: %s", r.URL)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"repos":[{"id":"old","name":"old","cell":"us","clusterSlug":"us","jurisdiction":"us","visibility":"private","full_name":"project/old","provider":"entire","checkpointCount":1,"placements":[]},{"id":"mirror","name":"old","cell":"us","clusterSlug":"us","jurisdiction":"us","visibility":"private","full_name":"project/old","provider":"github","checkpointCount":1,"placements":[]}],"nextPageToken":"more","truncated":true}`)
	}))
	defer srv.Close()
	old := newCellCoreClient
	newCellCoreClient = func() (cellCoreClient, error) { return coreapi.NewWithBearer(srv.URL, "test") }
	t.Cleanup(func() { newCellCoreClient = old })
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	got, directive := completeRepoFlag(cmd, nil, "et/project/old")
	if strings.Join(got, ",") != "*,et/project/old" || directive != cobra.ShellCompDirectiveNoFileComp {
		t.Fatalf("completion = %v, %v", got, directive)
	}
	if calls.Load() != 1 {
		t.Fatalf("calls = %d", calls.Load())
	}
}

func TestCompleteRepoFlag_PartialForgePrefix(t *testing.T) {
	entries := []coreapi.RepoIndexEntry{
		{ID: "github", Name: "web", FullName: "acme/web", Provider: coreapi.NewOptString("github"), CheckpointCount: coreapi.NewOptInt64(1)},
		{ID: "native", Name: "native", FullName: "proj/native", Provider: coreapi.NewOptString("entire"), CheckpointCount: coreapi.NewOptInt64(1)},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query().Get("q")
		if query != "" {
			t.Errorf("forge prefix used as name filter: %q", query)
		}
		out := coreapi.ListReposOutputBody{Repos: []coreapi.RepoIndexEntry{}}
		for _, entry := range entries {
			if strings.Contains(strings.ToLower(entry.FullName), query) {
				out.Repos = append(out.Repos, entry)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(&out); err != nil {
			t.Error(err)
		}
	}))
	defer srv.Close()
	old := newCellCoreClient
	newCellCoreClient = func() (cellCoreClient, error) { return coreapi.NewWithBearer(srv.URL, "test") }
	t.Cleanup(func() { newCellCoreClient = old })
	for _, tc := range []struct{ prefix, want string }{
		{"g", "*,gh/acme/web"}, {"gh", "*,gh/acme/web"}, {"gh/", "*,gh/acme/web"},
		{"e", "*,et/proj/native"}, {"et", "*,et/proj/native"}, {"et/", "*,et/proj/native"},
		{"G", "*,gh/acme/web"},
	} {
		t.Run(tc.prefix, func(t *testing.T) {
			// This parent replaces the process-global core-client constructor.
			cmd := &cobra.Command{}
			cmd.SetContext(context.Background())
			got, directive := completeRepoFlag(cmd, nil, tc.prefix)
			if strings.Join(got, ",") != tc.want || directive != cobra.ShellCompDirectiveNoFileComp {
				t.Fatalf("completion %q = %v, %v; want %s", tc.prefix, got, directive, tc.want)
			}
		})
	}
}

func TestCompleteRepoFlag_Timeout(t *testing.T) {
	withFakeCellCore(t, &fakeCellCore{blockUntilCtxDone: true})
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	start := time.Now()
	got, directive := completeRepoFlag(cmd, nil, "")
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("completion exceeded short timeout: %s", elapsed)
	}
	if strings.Join(got, ",") != "*" || directive != cobra.ShellCompDirectiveNoFileComp {
		t.Fatalf("completion = %v, %v", got, directive)
	}
}

func TestCompleteRepoFlag_IndexUnavailable(t *testing.T) {
	withFakeCellCore(t, &fakeCellCore{reposErr: errors.New("core unavailable")})
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	got, directive := completeRepoFlag(cmd, nil, "")
	if strings.Join(got, ",") != "*" || directive != cobra.ShellCompDirectiveNoFileComp {
		t.Fatalf("completion = %v, %v", got, directive)
	}
}
