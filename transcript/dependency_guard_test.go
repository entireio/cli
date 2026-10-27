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
// import becomes part of their build. Keep it to the standard library and the
// stdlib-only internal Pi parser so it never pulls in the CLI's dependencies.
// Native parsing stays behind the module's internal boundary: the library
// exposes conversion and the Entire Transcript Format, not native readers.
func TestLibraryImportsOnlyStdlib(t *testing.T) {
	t.Parallel()

	repoRoot, ok := testutil.GitGrepGuardRepoRoot(t)
	if !ok {
		return
	}
	checked, internalChecked := 0, 0
	for _, file := range testutil.GuardGoFiles(t, repoRoot) {
		if strings.HasPrefix(file, "transcript/pijsonl/") {
			t.Errorf("%s exposes native Pi parsing outside the module's internal boundary; it belongs in internal/transcript/pijsonl", file)
		}
		internalPi := strings.HasPrefix(file, "internal/transcript/pijsonl/")
		if (!strings.HasPrefix(file, "transcript/") && !internalPi) || strings.HasSuffix(file, "_test.go") {
			continue
		}
		source, err := parser.ParseFile(token.NewFileSet(), filepath.Join(repoRoot, filepath.FromSlash(file)), nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parsing %s: %v", file, err)
		}
		if internalPi {
			internalChecked++
		} else {
			checked++
		}
		for _, imported := range source.Imports {
			path, err := strconv.Unquote(imported.Path.Value)
			if err != nil {
				t.Fatalf("parsing import in %s: %v", file, err)
			}
			switch {
			case isStdlib(path):
			case internalPi:
				t.Errorf("%s imports %q: the internal Pi parser may import only the standard library", file, path)
			case path != "github.com/entireio/cli/internal/transcript/pijsonl":
				t.Errorf("%s imports %q: the transcript library may import only the standard library and the internal Pi parser", file, path)
			}
		}
	}
	if checked == 0 {
		t.Fatal("no transcript library source files checked")
	}
	if internalChecked == 0 {
		t.Fatal("no internal Pi parser source files checked")
	}
}

// isStdlib reports whether path is a standard library import path. Only
// standard library paths lack a dot in their first element.
func isStdlib(path string) bool {
	first, _, _ := strings.Cut(path, "/")
	return !strings.Contains(first, ".")
}
