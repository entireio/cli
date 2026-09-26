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
// command spelling that no longer exists: the deprecated top-level shortcuts
// (`entire explain`, `entire resume`, …) and the control-plane verbs renamed
// onto the unified `repo` surface (`entire repo get`, `entire auth use`, …).
// Following such a hint fails or warns for advice the CLI itself gave, so
// every hint, help example, and prompt must use the current form.
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
		// Control-plane verbs renamed onto the unified `repo` surface.
		"entire auth use",                  // → entire auth switch
		"entire repo get",                  // → entire repo view
		"entire repo mirror create",        // → entire repo mirror add
		"entire repo mirror use",           // → entire repo remote add
		"entire repo remote use",           // → entire repo remote add
		"entire repo remote url",           // → removed; entire repo mirror get lists a URL per cluster
		"entire repo mirror collaborators", // → entire repo grant list
		"entire repo access",               // → entire repo grant list
		"entire repo visibility set",       // → entire repo edit --visibility
		// The grant family moved under its nouns; the old spelling is gone.
		"entire grant org",     // → entire org grant
		"entire grant project", // → entire project grant
		"entire grant repo",    // → entire repo grant
		// A repo's home cluster is its owning project's region, so there is
		// nothing for a caller to choose.
		"--cluster-host", // → removed; the owning project's region decides
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
