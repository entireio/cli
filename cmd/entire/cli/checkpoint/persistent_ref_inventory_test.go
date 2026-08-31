package checkpoint

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestPersistentRefWritersUseNativeCASTransactions(t *testing.T) {
	t.Parallel()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve test source path")
	}
	repoRoot := filepath.Clean(filepath.Join(filepath.Dir(thisFile), "..", "..", "..", ".."))
	dirs := []string{
		filepath.Join(repoRoot, "cmd", "entire", "cli", "checkpoint"),
		filepath.Join(repoRoot, "cmd", "entire", "cli", "strategy"),
	}
	for _, dir := range dirs {
		err := filepath.WalkDir(dir, func(path string, entry os.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			content, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			for _, forbidden := range [][]byte{
				[]byte(".Storer.SetReference("),
				[]byte(".Storer.CheckAndSetReference("),
			} {
				if bytes.Contains(content, forbidden) {
					rel, relErr := filepath.Rel(repoRoot, path)
					if relErr != nil {
						rel = path
					}
					t.Errorf("%s bypasses the native ref CAS boundary with %s", rel, forbidden)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("scan production ref writers in %s: %v", dir, err)
		}
	}
}
