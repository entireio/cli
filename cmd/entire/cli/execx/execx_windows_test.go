//go:build windows

package execx

import (
	"os/exec"
	"testing"

	"golang.org/x/sys/windows"
)

// Windows ignores CREATE_NO_WINDOW once DETACHED_PROCESS is set, so each helper
// must set exactly its own console flag; routing one through the other would
// silently undo it.
func TestWindowsDetachHelpersSetDistinctConsoleFlags(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		detach func(*exec.Cmd)
		want   uint32
	}{
		{"detachFromTTY", detachFromTTY, windows.CREATE_NEW_PROCESS_GROUP | windows.DETACHED_PROCESS},
		{"detachBackgroundWorker", detachBackgroundWorker, windows.CREATE_NEW_PROCESS_GROUP | windows.CREATE_NO_WINDOW},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cmd := exec.CommandContext(t.Context(), "entire")
			tt.detach(cmd)
			if cmd.SysProcAttr == nil {
				t.Fatal("SysProcAttr = nil; want creation flags set")
			}
			if got := cmd.SysProcAttr.CreationFlags; got != tt.want {
				t.Fatalf("CreationFlags = %#x; want %#x", got, tt.want)
			}
		})
	}
}
