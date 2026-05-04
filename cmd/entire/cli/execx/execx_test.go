package execx

import (
	"context"
	"testing"
)

func TestInteractive_NoSysProcAttr(t *testing.T) {
	t.Parallel()
	cmd := Interactive(context.Background(), "/bin/true")
	if cmd.SysProcAttr != nil {
		t.Errorf("Interactive set SysProcAttr = %+v; want nil", cmd.SysProcAttr)
	}
}
