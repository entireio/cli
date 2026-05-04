//go:build unix

package execx

import (
	"context"
	"testing"
)

func TestNonInteractive_SetsSetsidUnix(t *testing.T) {
	t.Parallel()
	cmd := NonInteractive(context.Background(), "/bin/true")
	if cmd.SysProcAttr == nil || !cmd.SysProcAttr.Setsid {
		t.Errorf("NonInteractive: Setsid = false; want true")
	}
}
