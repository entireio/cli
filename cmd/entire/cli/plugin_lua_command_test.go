package cli

import (
	"context"
	"testing"
)

func TestMaybeRunLuaCommand_BuiltinWins(t *testing.T) {
	t.Parallel()
	rootCmd := NewRootCmd()
	// "status" is a built-in; the Lua dispatcher must defer to it (not handle).
	handled, code := MaybeRunLuaCommand(context.Background(), rootCmd, []string{"status"})
	if handled {
		t.Error("built-in command must not be handled by the Lua dispatcher")
	}
	if code != 0 {
		t.Errorf("exit code = %d, want 0 for not-handled", code)
	}
}

func TestMaybeRunLuaCommand_IgnoresFlagShapedArgs(t *testing.T) {
	t.Parallel()
	rootCmd := NewRootCmd()
	handled, code := MaybeRunLuaCommand(context.Background(), rootCmd, []string{"--help"})
	if handled || code != 0 {
		t.Errorf("flag-shaped first arg must not be handled, got (%v, %d)", handled, code)
	}
}

func TestMaybeRunLuaCommand_EmptyArgs(t *testing.T) {
	t.Parallel()
	rootCmd := NewRootCmd()
	if handled, code := MaybeRunLuaCommand(context.Background(), rootCmd, nil); handled || code != 0 {
		t.Errorf("no args must not be handled, got (%v, %d)", handled, code)
	}
}
