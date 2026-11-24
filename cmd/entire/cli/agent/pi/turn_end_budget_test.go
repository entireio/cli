package pi

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

// extensionHookTimeout must match the execFile timeout the embedded extension
// actually applies, or the budget is sized against a limit pi never enforces.
func TestExtensionHookTimeoutMatchesTheExtension(t *testing.T) {
	t.Parallel()

	require.Contains(t, extensionTemplate, fmt.Sprintf("timeout: %d,", extensionHookTimeout.Milliseconds()))
}
