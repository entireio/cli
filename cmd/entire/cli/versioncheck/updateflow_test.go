package versioncheck

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
)

// updateFlowFixture wires the seams RunUpdate depends on: the installer
// and the re-exec of the original command.
type updateFlowFixture struct {
	installCalls int
	installErr   error
	lastCommand  string
	reexecCalls  [][]string
	reexecErr    error
}

func newUpdateFlowFixture(t *testing.T) *updateFlowFixture {
	t.Helper()
	f := &updateFlowFixture{}

	origRun := runInstaller
	runInstaller = func(_ context.Context, cmd string) error {
		f.installCalls++
		f.lastCommand = cmd
		return f.installErr
	}
	origReexec := reexec
	reexec = func(_ context.Context, argv []string) error {
		f.reexecCalls = append(f.reexecCalls, argv)
		return f.reexecErr
	}
	t.Cleanup(func() {
		runInstaller = origRun
		reexec = origReexec
	})
	return f
}

func TestRunUpdate_NilArgvRunsInstallerWithoutRerun(t *testing.T) {
	f := newUpdateFlowFixture(t)

	var out bytes.Buffer
	RunUpdate(context.Background(), &out, "brew upgrade --yes entire", nil)

	if f.installCalls != 1 || f.lastCommand != "brew upgrade --yes entire" {
		t.Fatalf("installer calls = %d with %q, want 1 with the given command", f.installCalls, f.lastCommand)
	}
	if len(f.reexecCalls) != 0 {
		t.Errorf("reexec must not run without argv, got %v", f.reexecCalls)
	}
	got := out.String()
	if !strings.Contains(got, "Update complete. Re-run entire to use the new version.") {
		t.Errorf("missing no-rerun completion message:\n%s", got)
	}
}

func TestRunUpdate_ArgvReexecsOriginalCommand(t *testing.T) {
	f := newUpdateFlowFixture(t)

	argv := []string{"/usr/local/bin/entire", "login", "--device"}
	var out bytes.Buffer
	RunUpdate(context.Background(), &out, "mise upgrade entire", argv)

	if f.installCalls != 1 {
		t.Fatalf("installer calls = %d, want 1", f.installCalls)
	}
	if len(f.reexecCalls) != 1 || strings.Join(f.reexecCalls[0], " ") != strings.Join(argv, " ") {
		t.Fatalf("reexec argv = %v, want the original invocation %v", f.reexecCalls, argv)
	}
	got := out.String()
	if !strings.Contains(got, "Update complete. Rerunning the command:") {
		t.Errorf("missing rerun announcement:\n%s", got)
	}
	if !strings.Contains(got, "entire login --device") {
		t.Errorf("missing rerun command line:\n%s", got)
	}
}

func TestRunUpdate_InstallerFailurePrintsRetryAndSkipsRerun(t *testing.T) {
	f := newUpdateFlowFixture(t)
	f.installErr = errors.New("brew exploded")

	var out bytes.Buffer
	RunUpdate(context.Background(), &out, "brew upgrade --yes entire", []string{"entire", "login"})

	if len(f.reexecCalls) != 0 {
		t.Errorf("reexec must not run after a failed install, got %v", f.reexecCalls)
	}
	got := out.String()
	if !strings.Contains(got, "Update failed") {
		t.Errorf("missing failure message:\n%s", got)
	}
	if !strings.Contains(got, "brew upgrade --yes entire") {
		t.Errorf("missing retry command:\n%s", got)
	}
}

func TestRunUpdate_ReexecFailurePrintsManualRerun(t *testing.T) {
	f := newUpdateFlowFixture(t)
	f.reexecErr = errors.New("exec format error")

	var out bytes.Buffer
	RunUpdate(context.Background(), &out, "mise upgrade entire", []string{"entire", "login", "--device"})

	got := out.String()
	if !strings.Contains(got, "Could not rerun automatically") {
		t.Errorf("missing manual-rerun fallback:\n%s", got)
	}
	if !strings.Contains(got, "entire login --device") {
		t.Errorf("missing manual rerun command:\n%s", got)
	}
}

func TestPromptAllowed_Gates(t *testing.T) {
	useBrewExecutable(t)
	pinNonWindowsGOOS(t)
	origIsTerminalOut := isTerminalOut
	isTerminalOut = func(_ io.Writer) bool { return true }
	t.Cleanup(func() { isTerminalOut = origIsTerminalOut })
	t.Setenv("ENTIRE_TEST_TTY", "1")
	t.Setenv(envKillSwitch, "")
	t.Setenv(envUpgradeRerun, "")

	var out bytes.Buffer
	if !PromptAllowed(&out) {
		t.Fatal("want allowed with TTY, terminal writer, installer, no env gates")
	}

	t.Setenv(envKillSwitch, "1")
	if PromptAllowed(&out) {
		t.Error("want blocked with kill switch set")
	}
	t.Setenv(envKillSwitch, "")

	t.Setenv(envUpgradeRerun, "1")
	if PromptAllowed(&out) {
		t.Error("want blocked on a post-update rerun (loop guard)")
	}
	t.Setenv(envUpgradeRerun, "")

	isTerminalOut = func(_ io.Writer) bool { return false }
	if PromptAllowed(&out) {
		t.Error("want blocked for a non-terminal writer")
	}
}

func TestIsPostUpdateRerun(t *testing.T) {
	t.Setenv(envUpgradeRerun, "")
	if IsPostUpdateRerun() {
		t.Error("want false with the marker unset")
	}
	t.Setenv(envUpgradeRerun, "1")
	if !IsPostUpdateRerun() {
		t.Error("want true with the marker set")
	}
}

func TestRerunCommandLine(t *testing.T) {
	t.Parallel()

	if got := RerunCommandLine([]string{"/usr/local/bin/entire", "api", "/me", "--to", "cell"}); got != "entire api /me --to cell" {
		t.Errorf("got %q, want %q", got, "entire api /me --to cell")
	}
	if got := RerunCommandLine([]string{"entire", "dispatch", "fix the thing"}); got != `entire dispatch "fix the thing"` {
		t.Errorf("got %q, want quoted arg", got)
	}
	if got := RerunCommandLine(nil); got != "" {
		t.Errorf("got %q, want empty for nil argv", got)
	}
}
