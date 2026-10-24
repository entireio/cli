package transcript

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// Other modules import this library, and every module its non-test files
// import becomes part of their build. Keep it to the standard library and the
// stdlib-only internal Pi parser so it never pulls in the CLI's dependencies.
func TestLibraryImportsOnlyStdlib(t *testing.T) {
	t.Parallel()

	repoRoot, ok := testutil.GitGrepGuardRepoRoot(t)
	if !ok {
		return
	}
	checked, internalChecked := 0, 0
	for _, file := range testutil.GuardGoFiles(t, repoRoot) {
		internalPi := strings.HasPrefix(file, "internal/transcript/pijsonl/")
		if (!strings.HasPrefix(file, "transcript/") && !internalPi) || strings.HasSuffix(file, "_test.go") {
			continue
		}
		source, err := parser.ParseFile(token.NewFileSet(), filepath.Join(repoRoot, filepath.FromSlash(file)), nil, parser.ImportsOnly)
		if errors.Is(err, fs.ErrNotExist) {
			continue // Tracked files deleted from the working tree are not compiled.
		}
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
			if !isStdlib(path) && (internalPi || path != "github.com/entireio/cli/internal/transcript/pijsonl") {
				t.Errorf("%s imports %q: only the transcript library may import the internal Pi parser; all other imports must be standard library", file, path)
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

// Native parsing belongs behind the module's internal boundary; transcript
// exposes conversion and the Entire Transcript Format, not native readers.
func TestLibraryHasNoNativeReaderExports(t *testing.T) {
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
		source, err := parser.ParseFile(token.NewFileSet(), filepath.Join(repoRoot, filepath.FromSlash(file)), nil, parser.SkipObjectResolution)
		if errors.Is(err, fs.ErrNotExist) {
			continue // Tracked files deleted from the working tree are not compiled.
		}
		if err != nil {
			t.Fatalf("parsing %s: %v", file, err)
		}
		if strings.HasPrefix(file, "transcript/pijsonl/") {
			t.Errorf("%s exposes native Pi parsing outside the module's internal boundary", file)
		}
		if filepath.Dir(file) != "transcript" {
			continue
		}
		checked++
		checkName := func(name *ast.Ident) {
			switch name.Name {
			case "ReadCalls", "Call", "ToolUse":
				t.Errorf("%s exports %s: the transcript library must not expose native readers", file, name.Name)
			}
		}
		for _, declaration := range source.Decls {
			switch declaration := declaration.(type) {
			case *ast.FuncDecl:
				if declaration.Recv == nil {
					checkName(declaration.Name)
				}
			case *ast.GenDecl:
				for _, spec := range declaration.Specs {
					switch spec := spec.(type) {
					case *ast.TypeSpec:
						checkName(spec.Name)
					case *ast.ValueSpec:
						for _, name := range spec.Names {
							checkName(name)
						}
					}
				}
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
