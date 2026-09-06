package providers_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/entireio/cli/app/providers"
)

func TestLiveRepositoryAnalyzer_AnalyzeRepository(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "analyzer_test")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	// Create a dummy Go project structure
	os.MkdirAll(filepath.Join(tempDir, "app", "api"), 0755)
	os.MkdirAll(filepath.Join(tempDir, "tests"), 0755)
	os.WriteFile(filepath.Join(tempDir, "go.mod"), []byte("module github.com/test/repo\n"), 0644)
	os.WriteFile(filepath.Join(tempDir, "main.go"), []byte("package main"), 0644)
	os.WriteFile(filepath.Join(tempDir, ".env"), []byte("ENV=test"), 0644)

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
	if len(arch.TechStack) != 1 || arch.TechStack[0] != "Go" {
		t.Errorf("expected TechStack to be [Go], got %v", arch.TechStack)
	}

	// Check Components
	if len(arch.Components) != 1 || arch.Components[0] != "app" {
		t.Errorf("expected Components to include app, got %v", arch.Components)
	}

	// Check TestStructure
	if len(arch.TestStructure) != 1 || arch.TestStructure[0] != "tests" {
		t.Errorf("expected TestStructure to include tests, got %v", arch.TestStructure)
	}

	// Check UnknownInfo
	foundNoAPI := false
	for _, info := range arch.UnknownInfo {
		if info == "API routes could not be confidently determined without deep parsing" {
			foundNoAPI = true
			break
		}
	}
	if !foundNoAPI {
		t.Errorf("expected UnknownInfo about API routes")
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

	// Check InferredInfo for tests
	foundTestsInferred := false
	for _, info := range arch.InferredInfo {
		if info == "Tests might be co-located with source files (*_test.go)" {
			foundTestsInferred = true
			break
		}
	}
	if !foundTestsInferred {
		t.Errorf("expected InferredInfo about tests")
	}

	// Check TechStack
	if len(arch.TechStack) != 0 {
		t.Errorf("expected empty TechStack, got %v", arch.TechStack)
	}
}
