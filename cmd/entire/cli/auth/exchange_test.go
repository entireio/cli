package auth

import (
	"testing"
)

func TestEnableInsecureHTTP_FlipsOverride(t *testing.T) {
	// Manually save/restore because tests in this file may run before
	// any EnableInsecureHTTP() call from production code in the same
	// binary, and we don't want one test to bleed into another.
	prev := insecureHTTPOverride.Load()
	t.Cleanup(func() { insecureHTTPOverride.Store(prev) })

	insecureHTTPOverride.Store(false)
	EnableInsecureHTTP()
	if !insecureHTTPOverride.Load() {
		t.Fatal("EnableInsecureHTTP() did not flip the override to true")
	}
}
