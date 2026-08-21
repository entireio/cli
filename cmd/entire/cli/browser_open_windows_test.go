package cli

import (
	"testing"

	"golang.org/x/sys/windows"
)

// TestOpenBrowserWindows_PassesWholeURLToShellAssociation is the regression
// test for the truncated Windows login: the whole URL, every `&` separator and
// the percent-encoded redirect_uri included, must reach the browser. See
// openBrowserPlatform for why a shell cannot be involved.
//
// This also guards against reintroducing a `cmd /c start` launcher — that would
// stop calling the seam, leaving got empty.
func TestOpenBrowserWindows_PassesWholeURLToShellAssociation(t *testing.T) {
	// No t.Parallel(): this swaps the package-level shellExecute seam.
	var got string
	original := shellExecute
	shellExecute = func(_ windows.Handle, _, file, _, _ *uint16, _ int32) error {
		got = windows.UTF16PtrToString(file)
		return nil
	}
	t.Cleanup(func() { shellExecute = original })

	if err := openBrowserPlatform(authorizeURLWithSeparators); err != nil {
		t.Fatalf("openBrowserPlatform: %v", err)
	}

	if got != authorizeURLWithSeparators {
		t.Errorf("URL handed to the shell association was mangled:\n got: %s\nwant: %s", got, authorizeURLWithSeparators)
	}
}
