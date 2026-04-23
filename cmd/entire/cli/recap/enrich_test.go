package recap

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
)

// labelFeatureBuild is shared across enrichment tests to avoid repeated
// string literals (goconst).
const labelFeatureBuild = "feature_build"

func writeAnalysis(t *testing.T, w http.ResponseWriter, resp CheckpointAnalysisResponse) {
	t.Helper()
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		t.Fatalf("encode analysis: %v", err)
	}
}

func newTestEnricher(t *testing.T) *Enricher {
	t.Helper()
	cache, err := NewAnalysisCache(t.TempDir())
	if err != nil {
		t.Fatalf("cache: %v", err)
	}
	return NewEnricher(api.NewClient("test-token"), cache)
}

func TestEnricher_PopulatesLabels(t *testing.T) {
	// t.Parallel() omitted: t.Setenv is incompatible per CLAUDE.md.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeAnalysis(t, w, CheckpointAnalysisResponse{
			PipelineVersion: "2026-04-10.v3",
			Extraction:      CheckpointExtraction{Labels: []string{labelFeatureBuild, "testing"}},
		})
	}))
	defer srv.Close()
	t.Setenv("ENTIRE_API_BASE_URL", srv.URL) // verified in api/base_url.go:19

	e := newTestEnricher(t)

	cp := RecapCheckpoint{ID: id.CheckpointID("aa11bb22cc33"), Repo: "org/repo"}
	got, err := e.EnrichCheckpoint(context.Background(), cp)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Labels) != 2 || got.Labels[0] != labelFeatureBuild {
		t.Errorf("labels = %v", got.Labels)
	}
	if got.Source != SourceMixed {
		t.Errorf("expected SourceMixed, got %v", got.Source)
	}
}

func TestEnricher_UsesCache(t *testing.T) {
	// t.Parallel() omitted: t.Setenv is incompatible per CLAUDE.md.
	var requests int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		writeAnalysis(t, w, CheckpointAnalysisResponse{
			PipelineVersion: "v1",
			Extraction:      CheckpointExtraction{Labels: []string{"bug_fix"}},
		})
	}))
	defer srv.Close()
	t.Setenv("ENTIRE_API_BASE_URL", srv.URL)

	e := newTestEnricher(t)

	cp := RecapCheckpoint{ID: id.CheckpointID("ab12cd34ef56"), Repo: "org/repo"}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	for range 3 {
		if _, err := e.EnrichCheckpoint(ctx, cp); err != nil {
			t.Fatalf("enrich: %v", err)
		}
	}

	if requests != 1 {
		t.Errorf("expected 1 api request, got %d", requests)
	}
}

func TestEnricher_HTTPErrorYieldsLocalCheckpoint(t *testing.T) {
	// t.Parallel() omitted: t.Setenv is incompatible per CLAUDE.md.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()
	t.Setenv("ENTIRE_API_BASE_URL", srv.URL)

	e := newTestEnricher(t)

	cp := RecapCheckpoint{ID: id.CheckpointID("cc33dd44ee55"), Repo: "org/repo"}
	got, err := e.EnrichCheckpoint(context.Background(), cp)
	if err != nil {
		t.Fatalf("expected nil err on enrichment failure, got %v", err)
	}
	if got.Source != SourceLocal {
		t.Errorf("expected SourceLocal on failure, got %v", got.Source)
	}
	if len(got.Labels) != 0 {
		t.Errorf("expected no labels, got %v", got.Labels)
	}
}

func TestEnricher_DeepCopiesToolProfile(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeAnalysis(t, w, CheckpointAnalysisResponse{
			PipelineVersion: "v1",
			Extraction:      CheckpointExtraction{Labels: []string{labelFeatureBuild}},
			ToolProfile: &ToolProfile{
				Total:      5,
				Categories: map[string]ToolCategoryMetrics{"shell": {Count: 5}},
			},
		})
	}))
	defer srv.Close()
	t.Setenv("ENTIRE_API_BASE_URL", srv.URL)

	e := newTestEnricher(t)
	cp := RecapCheckpoint{ID: id.CheckpointID("dd44ee55ff66"), Repo: "org/repo"}
	got1, err := e.EnrichCheckpoint(context.Background(), cp)
	if err != nil {
		t.Fatal(err)
	}
	got2, err := e.EnrichCheckpoint(context.Background(), cp)
	if err != nil {
		t.Fatal(err)
	}

	// Mutate the first result's map.
	got1.ToolProfile.Categories["shell"] = ToolCategoryMetrics{Count: 999}

	// Second result should NOT see the mutation.
	if got2.ToolProfile.Categories["shell"].Count == 999 {
		t.Error("enricher leaked mutable map between EnrichCheckpoint calls")
	}
}

func TestEnricher_RejectsInvalidRepo(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		t.Error("server should not be reached for invalid repo")
	}))
	defer srv.Close()
	t.Setenv("ENTIRE_API_BASE_URL", srv.URL)

	e := newTestEnricher(t)
	cp := RecapCheckpoint{ID: id.CheckpointID("aa11bb22cc33"), Repo: "no-slash"}
	got, err := e.EnrichCheckpoint(context.Background(), cp)
	if err != nil {
		t.Fatal(err)
	}
	// Graceful fallback — the invalid-repo error is logged at debug and
	// the input checkpoint is returned unchanged.
	if got.Source != SourceLocal {
		t.Errorf("expected SourceLocal fallback, got %v", got.Source)
	}
	if len(got.Labels) != 0 {
		t.Errorf("expected no labels, got %v", got.Labels)
	}
}
