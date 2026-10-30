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
// stdlib-only internal native parsers (Pi's JSONL, Codex's items) so it never
// pulls in the CLI's dependencies. Native parsing stays behind the module's
// internal boundary: the library exposes conversion and the Entire Transcript
// Format, not native readers.
func TestLibraryImportsOnlyStdlib(t *testing.T) {
	t.Parallel()

	repoRoot, ok := testutil.GitGrepGuardRepoRoot(t)
	if !ok {
		return
	}
	checked := 0
	internalChecked := map[string]int{}
	for _, file := range testutil.GuardGoFiles(t, repoRoot) {
		for _, public := range []string{"transcript/pijsonl/", "transcript/codexitem/"} {
			if strings.HasPrefix(file, public) {
				t.Errorf("%s exposes native parsing outside the module's internal boundary; it belongs in internal/%s", file, public)
			}
		}
		internal := ""
		for path := range internalParsers {
			if dir := strings.TrimPrefix(path, "github.com/entireio/cli/") + "/"; strings.HasPrefix(file, dir) {
				internal = path
			}
		}
		if (!strings.HasPrefix(file, "transcript/") && internal == "") || strings.HasSuffix(file, "_test.go") {
			continue
		}
		source, err := parser.ParseFile(token.NewFileSet(), filepath.Join(repoRoot, filepath.FromSlash(file)), nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parsing %s: %v", file, err)
		}
		if internal != "" {
			internalChecked[internal]++
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
			case internal != "":
				t.Errorf("%s imports %q: an internal native parser may import only the standard library", file, path)
			case !internalParsers[path]:
				t.Errorf("%s imports %q: the transcript library may import only the standard library and the internal native parsers", file, path)
			}
		}
	}
	if checked == 0 {
		t.Fatal("no transcript library source files checked")
	}
	for path := range internalParsers {
		if internalChecked[path] == 0 {
			t.Fatalf("no %s source files checked", path)
		}
	}
}

// internalParsers are the stdlib-only native parsers the library may import.
var internalParsers = map[string]bool{
	"github.com/entireio/cli/internal/transcript/pijsonl":   true,
	"github.com/entireio/cli/internal/transcript/codexitem": true,
}

// isStdlib reports whether path is a standard library import path. Only
// standard library paths lack a dot in their first element.
func isStdlib(path string) bool {
	first, _, _ := strings.Cut(path, "/")
	return !strings.Contains(first, ".")
}
