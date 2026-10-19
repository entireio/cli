package transcript

import (
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// Other modules import this library, and every module its non-test files
// import becomes part of their build. Keep it to the standard library and its
// own subpackages so importing it never pulls in the CLI's dependencies.
func TestLibraryImportsOnlyStdlib(t *testing.T) {
	t.Parallel()

	repoRoot, ok := testutil.GitGrepGuardRepoRoot(t)
	if !ok {
		return
	}
	checked := 0
	for _, file := range testutil.GuardGoFiles(t, repoRoot) {
		if !strings.HasPrefix(file, "transcript/") || strings.HasSuffix(file, "_test.go") {
			continue
		}
		source, err := parser.ParseFile(token.NewFileSet(), filepath.Join(repoRoot, filepath.FromSlash(file)), nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parsing %s: %v", file, err)
		}
		checked++
		for _, imported := range source.Imports {
			path, err := strconv.Unquote(imported.Path.Value)
			if err != nil {
				t.Fatalf("parsing import in %s: %v", file, err)
			}
			if !isStdlib(path) && !strings.HasPrefix(path, "github.com/entireio/cli/transcript/") {
				t.Errorf("%s imports %q: the transcript library may import only the standard library and its own subpackages", file, path)
			}
		}
	}
	if checked == 0 {
		t.Fatal("no transcript library source files checked")
	}
}

// isStdlib reports whether path is a standard library import path. Only
// standard library paths lack a dot in their first element.
func isStdlib(path string) bool {
	first, _, _ := strings.Cut(path, "/")
	return !strings.Contains(first, ".")
}
