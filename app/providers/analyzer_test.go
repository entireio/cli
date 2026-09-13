package providers_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/app/providers"
)

func TestLiveRepositoryAnalyzer_AnalyzeRepository(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "analyzer_test")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	// Create a Go + Node project structure
	os.MkdirAll(filepath.Join(tempDir, "app", "api"), 0755)
	os.MkdirAll(filepath.Join(tempDir, "cmd", "server"), 0755)
	os.MkdirAll(filepath.Join(tempDir, "tests"), 0755)

	os.WriteFile(filepath.Join(tempDir, "go.mod"), []byte("module github.com/test/demoapp\n"), 0644)
	os.WriteFile(filepath.Join(tempDir, "package.json"), []byte(`{"name": "demoapp"}`), 0644)
	os.WriteFile(filepath.Join(tempDir, "cmd", "server", "main.go"), []byte("package main"), 0644)
	os.WriteFile(filepath.Join(tempDir, "app", "api", "handlers.go"), []byte(`package api
func Register() {
	r.HandleFunc("/api/v1/health", HealthHandler)
}`), 0644)
	os.WriteFile(filepath.Join(tempDir, "Dockerfile"), []byte("FROM golang:1.22"), 0644)

	analyzer := providers.NewLiveRepositoryAnalyzer()
	repo, err := analyzer.AnalyzeRepository(context.Background(), tempDir, false)
	if err != nil {
		t.Fatalf("AnalyzeRepository failed: %v", err)
	}

	if repo.Architecture == nil {
		t.Fatalf("expected Architecture to be populated")
	}

	arch := repo.Architecture

	// Check TechStack
	if len(arch.TechStack) < 2 {
		t.Errorf("expected TechStack to include Go and Node, got %v", arch.TechStack)
	}

	// Check Summary
	if arch.Summary == "" {
		t.Errorf("expected non-empty architecture narrative summary")
	}

	// Check Components
	hasApp := false
	for _, c := range arch.Components {
		if strings.HasPrefix(c, "app") {
			hasApp = true
			break
		}
	}
	if !hasApp {
		t.Errorf("expected Components to include app, got %v", arch.Components)
	}

	// Check API Routes
	if len(arch.APIRoutes) == 0 || arch.APIRoutes[0] != "/api/v1/health" {
		t.Errorf("expected API route /api/v1/health detected, got %v", arch.APIRoutes)
	}

	// Check InferredInfo
	if len(arch.InferredInfo) == 0 {
		t.Errorf("expected InferredInfo to be populated")
	}
}

func TestLiveRepositoryAnalyzer_PartialAnalysis(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "analyzer_partial_test")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	analyzer := providers.NewLiveRepositoryAnalyzer()
	repo, err := analyzer.AnalyzeRepository(context.Background(), tempDir, true)
	if err != nil {
		t.Fatalf("AnalyzeRepository failed: %v", err)
	}

	if repo.Architecture == nil {
		t.Fatalf("expected Architecture to be populated")
	}

	arch := repo.Architecture

	// Check UnknownInfo for missing API routes and test suite
	foundUnknownAPI := false
	for _, info := range arch.UnknownInfo {
		if strings.Contains(info, "API routes could not be confidently determined") {
			foundUnknownAPI = true
			break
		}
	}
	if !foundUnknownAPI {
		t.Errorf("expected UnknownInfo about API routes in empty repo")
	}

	if len(arch.TechStack) != 0 {
		t.Errorf("expected empty TechStack for empty repo, got %v", arch.TechStack)
	}
}

func TestLiveRepositoryAnalyzer_ForceRefreshAndCaching(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "analyzer_cache_test")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	os.WriteFile(filepath.Join(tempDir, "go.mod"), []byte("module github.com/test/cachedapp\n"), 0644)

	analyzer := providers.NewLiveRepositoryAnalyzer()
	repo1, err := analyzer.AnalyzeRepository(context.Background(), tempDir, false)
	if err != nil {
		t.Fatalf("First analysis failed: %v", err)
	}

	cacheFile := filepath.Join(tempDir, ".entire", "architecture.json")
	if _, err := os.Stat(cacheFile); os.IsNotExist(err) {
		t.Fatalf("expected cache file .entire/architecture.json to be created")
	}

	// Read cached analysis
	repo2, err := analyzer.AnalyzeRepository(context.Background(), tempDir, false)
	if err != nil {
		t.Fatalf("Cached analysis read failed: %v", err)
	}
	if repo2.Name != repo1.Name {
		t.Errorf("expected cached repo name %s, got %s", repo1.Name, repo2.Name)
	}

	// Force refresh
	repo3, err := analyzer.AnalyzeRepository(context.Background(), tempDir, true)
	if err != nil {
		t.Fatalf("Force refresh analysis failed: %v", err)
	}
	if repo3.Architecture == nil {
		t.Fatalf("expected Architecture after force refresh")
	}
}

func TestLiveRepositoryAnalyzer_InvalidPath(t *testing.T) {
	analyzer := providers.NewLiveRepositoryAnalyzer()
	_, err := analyzer.AnalyzeRepository(context.Background(), "/non_existent_directory_12345", false)
	if err == nil {
		t.Errorf("expected error for non-existent directory")
	}
}
