package execx

import (
	"testing"
)

// A detached child must not write into a pipe this process drains: after the
// parent exits, the child's next stdout/stderr write raises SIGPIPE and kills
// it. Nil streams are opened as the null device instead.
func TestDetachedCommand_StdioIsNullDevice(t *testing.T) {
	t.Parallel()
	cmd := detachedCommand("/bin/entire", "", "__opf_flush")
	if cmd.Stdout != nil {
		t.Errorf("Stdout = %T; want nil so the child writes to the null device, not a pipe", cmd.Stdout)
	}
	if cmd.Stderr != nil {
		t.Errorf("Stderr = %T; want nil so the child writes to the null device, not a pipe", cmd.Stderr)
	}
	if cmd.Stdin != nil {
		t.Errorf("Stdin = %T; want nil", cmd.Stdin)
	}
	if cmd.Dir == "" {
		t.Error("Dir is empty; want os.TempDir() fallback")
	}
}
