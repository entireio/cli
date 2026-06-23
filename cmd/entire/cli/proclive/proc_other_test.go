//go:build !linux && !darwin

package proclive

import (
	"os"
	"testing"
)

// On unsupported platforms liveness must degrade to Unknown (never a wrong
// Alive/Dead), so callers fall back to the inactivity timeout.
func TestCheck_UnsupportedIsUnknown(t *testing.T) {
	t.Parallel()
	id := Identity{PID: os.Getpid(), Start: "anything"}
	if got := Check(id); got != LivenessUnknown {
		t.Errorf("Check on unsupported platform = %v, want unknown", got)
	}
	if _, ok := ResolveOwner(); ok {
		t.Errorf("ResolveOwner on unsupported platform returned ok=true, want false")
	}
}
