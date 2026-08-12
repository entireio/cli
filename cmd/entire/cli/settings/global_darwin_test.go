//go:build darwin

package settings

import "testing"

// Pins the production wiring: matchesExcludePath must pass
// caseInsensitivePaths (true on darwin) to the fold seam. The seam tests in
// global_test.go cover both fold branches on every platform; this catches a
// regression in the platform predicate itself.
func TestMatchesExcludePath_FoldsOnDarwin(t *testing.T) {
	t.Parallel()
	matched, err := matchesExcludePath(t.Context(), []string{"/TMP/Scratch/**"}, "/tmp/scratch/x")
	if err != nil {
		t.Fatalf("matchesExcludePath: %v", err)
	}
	if !matched {
		t.Error("on darwin the production matcher must case-fold")
	}
}
