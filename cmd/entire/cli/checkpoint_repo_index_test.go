package cli

import (
	"context"
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

	"github.com/entireio/cli/cmd/entire/cli/entiredir"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/internal/coreapi"
	"github.com/spf13/cobra"
)

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

func TestCheckpointRepoIndex_PagesAndCompletion(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
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
	cmd := &cobra.Command{}
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
	cmd.SetContext(logging.WithLogger(context.Background(), logger))
	got, directive := completeRepoFlag(cmd, nil, "")
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
	if strings.Join(got, ",") != "*,gh/owner/new,et/project/old" || directive != cobra.ShellCompDirectiveNoFileComp {
		t.Fatalf("completion = %v, %v", got, directive)
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

func TestCompleteRepoFlag_IndexUnavailable(t *testing.T) {
	withFakeCellCore(t, &fakeCellCore{reposErr: errors.New("core unavailable")})
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	got, directive := completeRepoFlag(cmd, nil, "")
	if strings.Join(got, ",") != "*" || directive != cobra.ShellCompDirectiveNoFileComp {
		t.Fatalf("completion = %v, %v", got, directive)
	}
}
