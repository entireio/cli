package checkpoint

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Regression (PR #1951 review, finding 01M08CYEBTFRR): the checkpoint
// read-candidate chain is threaded by hand — every checkpoint.Open call site
// must pass OpenOptions.ReadRemotes, and a site that forgets silently
// reverts to legacy origin-only reads. That silent fallback produced three
// separate findings in one review (attach, import, and an earlier fix).
// Open cannot derive the chain itself without a strategy→checkpoint import
// cycle, so this test makes the decision compile-adjacent instead: every
// production OpenOptions literal must mention the ReadRemotes key. Setting
// it explicitly to nil is a legitimate, visible decision (a deliberately
// legacy or local-scoped open); omitting it is not.
//
// Test files are exempt — fixtures legitimately exercise the nil-default
// path.
func TestOpenOptionsLiteralsDeclareReadRemotes(t *testing.T) {
	t.Parallel()

	root, err := filepath.Abs(filepath.Join("..", "..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	// Sanity-check the walk root so a layout change fails loudly instead of
	// silently scanning nothing.
	if _, err := os.Stat(filepath.Join(root, "cmd", "entire")); err != nil {
		t.Fatalf("walk root %s does not look like the repo root: %v", root, err)
	}

	var violations []string
	fset := token.NewFileSet()
	walkErr := filepath.WalkDir(filepath.Join(root, "cmd"), func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		// Fast path: skip files that cannot contain a literal.
		if !strings.Contains(string(src), "OpenOptions{") {
			return nil
		}
		file, err := parser.ParseFile(fset, path, src, 0)
		if err != nil {
			return err
		}
		ast.Inspect(file, func(n ast.Node) bool {
			lit, ok := n.(*ast.CompositeLit)
			if !ok || !isOpenOptionsType(lit.Type) {
				return true
			}
			for _, elt := range lit.Elts {
				kv, ok := elt.(*ast.KeyValueExpr)
				if !ok {
					continue
				}
				if ident, ok := kv.Key.(*ast.Ident); ok && ident.Name == "ReadRemotes" {
					return true
				}
			}
			pos := fset.Position(lit.Pos())
			rel, relErr := filepath.Rel(root, pos.Filename)
			if relErr != nil {
				rel = pos.Filename
			}
			violations = append(violations, rel+":"+itoa(pos.Line))
			return true
		})
		return nil
	})
	if walkErr != nil {
		t.Fatal(walkErr)
	}
	if len(violations) > 0 {
		t.Errorf("OpenOptions literals missing the ReadRemotes key (pass strategy.CheckpointReadRemotes(ctx), or an explicit nil with a comment for a deliberately legacy/local open):\n  %s",
			strings.Join(violations, "\n  "))
	}
}

// isOpenOptionsType matches `OpenOptions` (same package) and any
// `<pkg>.OpenOptions` selector (checkpoint.OpenOptions, cpkg.OpenOptions, ...).
func isOpenOptionsType(expr ast.Expr) bool {
	switch t := expr.(type) {
	case *ast.Ident:
		return t.Name == "OpenOptions"
	case *ast.SelectorExpr:
		return t.Sel.Name == "OpenOptions"
	}
	return false
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
