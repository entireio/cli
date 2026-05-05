package plugin

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// newTestManager returns a Manager rooted at a fresh temp dir.
func newTestManager(t *testing.T) *Manager {
	t.Helper()
	return &Manager{Root: t.TempDir()}
}

// installFakeScript writes a fake script plugin at root/entire-<name>/entire-<name>.
// On Unix the script is made executable; on Windows we skip these tests.
func installFakeScript(t *testing.T, root, name, body string) {
	t.Helper()
	dir := filepath.Join(root, Prefix+name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	exe := filepath.Join(dir, Prefix+name)
	if err := os.WriteFile(exe, []byte(body), 0o755); err != nil {
		t.Fatalf("write exe: %v", err)
	}
}

func TestDispatch_NoArgs(t *testing.T) {
	t.Parallel()
	root := &cobra.Command{Use: "entire"}
	mgr := newTestManager(t)
	handled, err := DispatchWith(context.Background(), root, nil, mgr)
	if handled || err != nil {
		t.Errorf("Dispatch(nil) = (%v, %v); want (false, nil)", handled, err)
	}
}

func TestDispatch_FlagsOnly(t *testing.T) {
	t.Parallel()
	root := &cobra.Command{Use: "entire"}
	mgr := newTestManager(t)
	handled, err := DispatchWith(context.Background(), root, []string{"--foo", "--bar"}, mgr)
	if handled || err != nil {
		t.Errorf("flags-only: handled=%v err=%v; want false,nil", handled, err)
	}
}

func TestDispatch_BuiltinTakesPrecedence(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == osWindows {
		t.Skip("uses Unix shebang scripts")
	}
	mgr := newTestManager(t)
	installFakeScript(t, mgr.Root, "status", "#!/bin/sh\nexit 0\n")

	rootCmd := &cobra.Command{Use: "entire"}
	rootCmd.AddCommand(&cobra.Command{Use: "status"})

	handled, err := DispatchWith(context.Background(), rootCmd, []string{"status"}, mgr)
	if handled || err != nil {
		t.Errorf("built-in shadow: handled=%v err=%v; want false,nil", handled, err)
	}
}

func TestDispatch_RunsPlugin(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == osWindows {
		t.Skip("uses Unix shebang scripts")
	}
	mgr := newTestManager(t)
	installFakeScript(t, mgr.Root, "echo", "#!/bin/sh\nif [ \"$1\" = \"ok\" ]; then exit 0; else exit 7; fi\n")

	rootCmd := &cobra.Command{Use: "entire"}
	handled, err := DispatchWith(context.Background(), rootCmd, []string{"echo", "ok"}, mgr)
	if !handled {
		t.Fatalf("plugin not handled: err=%v", err)
	}
	if err != nil {
		t.Errorf("Dispatch error: %v", err)
	}
}

func TestDispatch_PropagatesExitCode(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == osWindows {
		t.Skip("uses Unix shebang scripts")
	}
	mgr := newTestManager(t)
	installFakeScript(t, mgr.Root, "fail", "#!/bin/sh\nexit 42\n")

	rootCmd := &cobra.Command{Use: "entire"}
	handled, err := DispatchWith(context.Background(), rootCmd, []string{"fail"}, mgr)
	if !handled {
		t.Fatalf("expected handled=true; err=%v", err)
	}
	if err == nil {
		t.Fatalf("expected non-nil err for non-zero exit")
	}
	var exitErr *exec.ExitError
	if !asExitError(err, &exitErr) {
		t.Fatalf("err = %v; want *exec.ExitError", err)
	}
	if got := exitErr.ExitCode(); got != 42 {
		t.Errorf("exit code = %d; want 42", got)
	}
	if got := PropagateExitCode(err); got != 42 {
		t.Errorf("PropagateExitCode = %d; want 42", got)
	}
}

func TestDispatch_InjectsPluginDataDir(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == osWindows {
		t.Skip("uses Unix shebang scripts")
	}
	mgr := newTestManager(t)
	out := filepath.Join(t.TempDir(), "out")
	body := "#!/bin/sh\nprintf '%s' \"$ENTIRE_PLUGIN_DATA_DIR\" > " + out + "\n"
	installFakeScript(t, mgr.Root, "envprobe", body)

	rootCmd := &cobra.Command{Use: "entire"}
	if _, err := DispatchWith(context.Background(), rootCmd, []string{"envprobe"}, mgr); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read out: %v", err)
	}
	want := filepath.Join(mgr.Root, Prefix+"envprobe", "data")
	if string(got) != want {
		t.Errorf("ENTIRE_PLUGIN_DATA_DIR = %q; want %q", got, want)
	}
}

func TestDispatch_UnknownPluginFallsThrough(t *testing.T) {
	t.Parallel()
	mgr := newTestManager(t)
	rootCmd := &cobra.Command{Use: "entire"}
	handled, err := DispatchWith(context.Background(), rootCmd, []string{"does-not-exist"}, mgr)
	if handled || err != nil {
		t.Errorf("unknown plugin: handled=%v err=%v; want false,nil", handled, err)
	}
}

func TestDispatch_RejectsInvalidName(t *testing.T) {
	t.Parallel()
	mgr := newTestManager(t)
	rootCmd := &cobra.Command{Use: "entire"}
	handled, err := DispatchWith(context.Background(), rootCmd, []string{"BadName"}, mgr)
	if handled || err != nil {
		t.Errorf("invalid name: handled=%v err=%v; want false,nil", handled, err)
	}
}

func TestDispatch_FirstNonFlagIsCandidate(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == osWindows {
		t.Skip("uses Unix shebang scripts")
	}
	mgr := newTestManager(t)
	out := filepath.Join(t.TempDir(), "out")
	body := "#!/bin/sh\nprintf '%s|' \"$@\" > " + out + "\n"
	installFakeScript(t, mgr.Root, "args", body)

	rootCmd := &cobra.Command{Use: "entire"}
	if _, err := DispatchWith(context.Background(), rootCmd, []string{"--global-flag", "args", "--child-flag", "value"}, mgr); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read out: %v", err)
	}
	if !strings.Contains(string(got), "--child-flag|value|") {
		t.Errorf("child args = %q; want to contain --child-flag|value|", got)
	}
}

func asExitError(err error, target **exec.ExitError) bool {
	for cur := err; cur != nil; {
		ee := &exec.ExitError{}
		if errors.As(cur, &ee) {
			*target = ee
			return true
		}
		type unwrapper interface{ Unwrap() error }
		u, ok := cur.(unwrapper)
		if !ok {
			return false
		}
		cur = u.Unwrap()
	}
	return false
}
