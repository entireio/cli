//go:build !windows

package cli

import (
	"context"
	"os/exec"
)

// wrappedTitleCommand runs a preserved agy title command the way agy itself
// runs the slot on this host: through sh. See newAntigravityTitleTeeCmd for
// why executing the user's own string here is intentional.
func wrappedTitleCommand(ctx context.Context, original string) *exec.Cmd {
	return exec.CommandContext(ctx, "sh", "-c", original)
}
