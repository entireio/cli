package paths_test

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

// TestNoDirectEntireJoins prevents production paths from bypassing invisible
// runtime routing. Deliberate joins require a per-call-site rationale:
//
//	x := filepath.Join(root, paths.EntireDir, "runners") // entire-join-ok: <reason>
func TestNoDirectEntireJoins(t *testing.T) {
	t.Parallel()

	// filepath.Join(...) with no nested calls; args inspected afterwards.
	joinCall := regexp.MustCompile(`filepath\.Join\(([^()]*)\)`)
	entireToken := regexp.MustCompile(`"\.entire|\bEntire(?:Dir|TmpDir|MetadataDir|LogsDir)\b`)
	entireConstFirst := regexp.MustCompile(`^\s*(?:paths\.)?Entire(?:Dir|TmpDir|MetadataDir|LogsDir)\b`)
	// Inline audited-bypass marker; requires a non-empty rationale.
	joinOKMarker := regexp.MustCompile(`//\s*entire-join-ok:\s*\S`)

	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	repoRoot := filepath.Clean(filepath.Join(filepath.Dir(thisFile), "..", "..", "..", ".."))

	var violations []string
	for _, scanRoot := range []string{"cmd", "internal"} {
		err := filepath.WalkDir(filepath.Join(repoRoot, scanRoot), func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				// Test infrastructure builds fixtures in throwaway repos.
				switch d.Name() {
				case "testutil", "benchutil", "integration_test", "testdata":
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			rel := filepath.ToSlash(strings.TrimPrefix(path, repoRoot+string(filepath.Separator)))
			data, readErr := os.ReadFile(path)
			if readErr != nil {
				return readErr
			}
			src := string(data)
			for _, idx := range joinCall.FindAllStringSubmatchIndex(src, -1) {
				args := src[idx[2]:idx[3]]
				firstArg, rest, found := strings.Cut(args, ",")
				if !found {
					continue // single-arg join cannot combine a base with .entire
				}
				if !entireToken.MatchString(rest) {
					continue
				}
				if entireConstFirst.MatchString(firstArg) {
					continue // builds a relative .entire subpath, not a resolved location
				}
				if joinOKMarker.MatchString(sourceLine(src, idx[0])) {
					continue // audited-deliberate bypass, marked at the call site
				}
				violations = append(violations, rel+": "+strings.TrimSpace(src[idx[0]:idx[1]]))
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", scanRoot, err)
		}
	}

	if len(violations) > 0 {
		t.Errorf("found filepath.Join calls that bypass paths.AbsPath for .entire runtime paths.\n"+
			"Resolve runtime paths through paths.AbsPath (invisible routing), or mark the\n"+
			"call site as audited with an inline `// entire-join-ok: <reason>` comment:\n  %s",
			strings.Join(violations, "\n  "))
	}
}

// sourceLine returns the full source line containing byte offset off.
func sourceLine(src string, off int) string {
	start := strings.LastIndexByte(src[:off], '\n') + 1
	end := strings.IndexByte(src[off:], '\n')
	if end < 0 {
		return src[start:]
	}
	return src[start : off+end]
}
