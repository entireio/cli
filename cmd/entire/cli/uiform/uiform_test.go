package uiform

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestProductionFormsUseUIFormHelper(t *testing.T) {
	t.Parallel()

	root := findRepoRoot(t)
	cliDir := filepath.Join(root, "cmd", "entire", "cli")
	allowed := filepath.ToSlash(filepath.Join("cmd", "entire", "cli", "uiform", "uiform.go"))

	err := filepath.WalkDir(cliDir, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}

		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if !strings.Contains(string(data), "huh.NewForm(") {
			return nil
		}

		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if filepath.ToSlash(rel) != allowed {
			t.Errorf("%s uses huh.NewForm directly; use uiform.New instead", rel)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk CLI files: %v", err)
	}
}

func findRepoRoot(t *testing.T) string {
	t.Helper()

	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("get working directory: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("could not find repo root from %s", dir)
		}
		dir = parent
	}
}
