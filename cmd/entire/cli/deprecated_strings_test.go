package cli

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestNoDeprecatedCommandFormsInUserFacingStrings sweeps the CLI package
// tree's production sources for strings that tell users or agents to run a
// deprecated top-level shortcut (`entire explain`, `entire resume`, …).
// Following such a hint prints a deprecation warning for advice the CLI
// itself gave, so every hint, help example, and prompt must use the
// canonical group form (`entire checkpoint explain`, `entire session
// resume`, …).
//
// Scope: non-test .go files under this package and its subpackages.
// Comment-only lines are skipped — code comments may legitimately discuss
// the deprecated forms. Canonical forms never trip these patterns because
// the group noun intervenes: "entire session resume" does not contain the
// contiguous substring "entire resume".
func TestNoDeprecatedCommandFormsInUserFacingStrings(t *testing.T) {
	t.Parallel()

	deprecatedForms := []string{
		"entire explain", // → entire checkpoint explain
		"entire resume",  // → entire session resume
		"entire attach",  // → entire session attach
		"entire trace",   // → entire doctor trace
		"entire rewind",  // → removed (no replacement); never advertise
		"entire reset",   // → entire clean
	}

	var offenders []string
	err := filepath.WalkDir(".", func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			if d.Name() == "testdata" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		content, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		for i, line := range strings.Split(string(content), "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "//") {
				continue // code comments may discuss deprecated forms
			}
			for _, form := range deprecatedForms {
				if strings.Contains(line, form) {
					offenders = append(offenders, fmt.Sprintf("%s:%d: %s", path, i+1, strings.TrimSpace(line)))
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking package tree: %v", err)
	}

	if len(offenders) > 0 {
		t.Errorf("production strings reference deprecated top-level command forms; use the canonical group form instead:\n  %s",
			strings.Join(offenders, "\n  "))
	}
}
