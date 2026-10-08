package cli

import (
	"go/parser"
	"go/token"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// The public transcript library must be usable without CLI or redaction dependencies.
func TestTranscriptLibraryDependencies(t *testing.T) {
	t.Parallel()

	repoRoot, ok := testutil.GitGrepGuardRepoRoot(t)
	if !ok {
		return
	}
	files := testutil.GitGrepGuard(t, repoRoot, "-l", "^package ", "--",
		":(glob)transcript/**/*.go", ":(exclude,glob)**/*_test.go")

	cmd := exec.Command("go", "list", "std") //nolint:noctx // source guard, no cancellation needed
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("listing standard library packages: %v", err)
	}
	stdlib := make(map[string]bool)
	for path := range strings.FieldsSeq(string(out)) {
		stdlib[path] = true
	}
	checked := 0
	for file := range strings.SplitSeq(strings.TrimSpace(files), "\n") {
		source, err := parser.ParseFile(token.NewFileSet(), filepath.Join(repoRoot, file), nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parsing %s: %v", file, err)
		}
		checked++
		for _, imported := range source.Imports {
			path, err := strconv.Unquote(imported.Path.Value)
			if err != nil {
				t.Fatalf("parsing import in %s: %v", file, err)
			}
			if !stdlib[path] && !strings.HasPrefix(path, "github.com/entireio/cli/transcript/") {
				t.Errorf("%s imports %q: transcript library may import only stdlib and transcript subpackages", file, path)
			}
		}
	}
	if checked == 0 {
		t.Fatal("no transcript library source files checked")
	}
}
