package cli

import (
	"strings"
	"testing"
)

func TestRewindCmd_IsDeprecated(t *testing.T) {
	t.Parallel()

	cmd := newRewindCmd()
	if cmd.Deprecated == "" {
		t.Error("rewind command should have Deprecated field set")
	}
	if !strings.Contains(cmd.Deprecated, "removed") {
		t.Errorf("Deprecated message should announce removal, got: %s", cmd.Deprecated)
	}
}
