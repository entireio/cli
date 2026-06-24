package investigate

import (
	"context"
	"testing"
)

func TestOpenInBrowser_NoOpUnderTest(t *testing.T) {
	t.Parallel()

	// Under test (testing.Testing() is true) the opener must not spawn a real
	// browser; it returns nil so callers proceed normally.
	if err := openInBrowser(context.Background(), "/tmp/findings.html"); err != nil {
		t.Errorf("openInBrowser under test should no-op, got: %v", err)
	}
}
