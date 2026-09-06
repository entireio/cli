package providers

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/entireio/cli/app/models"
)

type LiveRepositoryAnalyzer struct{}

func NewLiveRepositoryAnalyzer() *LiveRepositoryAnalyzer {
	return &LiveRepositoryAnalyzer{}
}

func (a *LiveRepositoryAnalyzer) AnalyzeRepository(ctx context.Context, localPath string, forceRefresh bool) (*models.Repository, error) {
	cachePath := filepath.Join(localPath, ".entire", "architecture.json")

	if !forceRefresh {
		if data, err := os.ReadFile(cachePath); err == nil {
			var repo models.Repository
			if err := json.Unmarshal(data, &repo); err == nil {
				return &repo, nil
			}
		}
	}

	arch := &models.RepositoryArchitecture{
		Directories:    []string{},
		ImportantFiles: []string{},
		EntryPoints:    []string{},
		Components:     []string{},
		APIRoutes:      []string{},
		TechStack:      []string{},
		ConfigFiles:    []string{},
		TestStructure:  []string{},
		InferredInfo:   []string{},
		UnknownInfo:    []string{},
	}

	entries, err := os.ReadDir(localPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read local path: %w", err)
	}

	hasGoMod := false
	hasPackageJson := false

	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() {
			if !strings.HasPrefix(name, ".") {
				arch.Directories = append(arch.Directories, name)
				if name == "app" || name == "src" || name == "lib" {
					arch.Components = append(arch.Components, name)
				}
				if name == "api" {
					arch.Components = append(arch.Components, name)
				}
				if name == "tests" || name == "e2e" {
					arch.TestStructure = append(arch.TestStructure, name)
				}
			}
		} else {
			if name == "main.go" || name == "index.js" || name == "app.js" {
				arch.EntryPoints = append(arch.EntryPoints, name)
				arch.ImportantFiles = append(arch.ImportantFiles, name)
			}
			if name == "go.mod" {
				hasGoMod = true
				arch.TechStack = append(arch.TechStack, "Go")
				arch.ImportantFiles = append(arch.ImportantFiles, name)
			}
			if name == "package.json" {
				hasPackageJson = true
				arch.TechStack = append(arch.TechStack, "Node.js/JavaScript")
				arch.ImportantFiles = append(arch.ImportantFiles, name)
			}
			if strings.HasSuffix(name, ".yaml") || strings.HasSuffix(name, ".yml") || strings.HasSuffix(name, ".toml") || name == ".env" || name == ".env.example" {
				arch.ConfigFiles = append(arch.ConfigFiles, name)
			}
			if name == "README.md" || name == "BUILDATHON.md" || name == "CHANGELOG.md" {
				arch.ImportantFiles = append(arch.ImportantFiles, name)
			}
		}
	}

	if hasGoMod {
		arch.InferredInfo = append(arch.InferredInfo, "Go project detected based on go.mod")
	}
	if hasPackageJson {
		arch.InferredInfo = append(arch.InferredInfo, "Node project detected based on package.json")
	}
	
	if len(arch.APIRoutes) == 0 {
		arch.UnknownInfo = append(arch.UnknownInfo, "API routes could not be confidently determined without deep parsing")
	}

	if len(arch.TestStructure) == 0 {
		arch.InferredInfo = append(arch.InferredInfo, "Tests might be co-located with source files (*_test.go)")
	}

	absPath, _ := filepath.Abs(localPath)

	repo := &models.Repository{
		ID:            "repo-inferred",
		Name:          filepath.Base(absPath),
		Owner:         "Unknown",
		URL:           "",
		LocalPath:     absPath,
		DefaultBranch: "main",
		Description:   "Local inferred repository",
		Architecture:  arch,
	}

	if hasGoMod {
		if content, err := os.ReadFile(filepath.Join(localPath, "go.mod")); err == nil {
			lines := strings.Split(string(content), "\n")
			if len(lines) > 0 && strings.HasPrefix(lines[0], "module ") {
				modName := strings.TrimSpace(strings.TrimPrefix(lines[0], "module "))
				parts := strings.Split(modName, "/")
				if len(parts) >= 3 {
					repo.Owner = parts[len(parts)-2]
					repo.Name = parts[len(parts)-1]
					repo.ID = fmt.Sprintf("repo-%s", repo.Name)
					repo.URL = fmt.Sprintf("https://%s", modName)
				}
			}
		}
	}

	if err := os.MkdirAll(filepath.Join(localPath, ".entire"), 0755); err == nil {
		if data, err := json.MarshalIndent(repo, "", "  "); err == nil {
			os.WriteFile(cachePath, data, 0644)
		}
	}

	return repo, nil
}
