//go:build !windows

package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// standInEnv selects how the test binary behaves when it runs as the `entire`
// stand-in. Linking the test binary rather than writing a script avoids the
// ETXTBSY race of exec-ing a freshly written file while parallel tests fork.
const standInEnv = "ENTIRE_HOOK_WRAPPER_STAND_IN"

const (
	// standInBroken is on PATH but cannot run, like a version-manager shim
	// with no version selected.
	standInBroken = "broken"
	// standInWorking echoes the hook payload from stdin to stdout.
	standInWorking = "working"
	// standInHookError runs, but the hook itself fails.
	standInHookError = "hook-error"
	// standInHang runs, and the hook never finishes.
	standInHang = "hang"
)

// hookPayload stands in for the JSON an agent writes to a hook's stdin.
const hookPayload = `{"session_id":"s1"}`

func TestMain(m *testing.M) {
	if mode := os.Getenv(standInEnv); mode != "" && filepath.Base(os.Args[0]) == "entire" {
		os.Exit(runEntireStandIn(mode, os.Args[1:]))
	}
	os.Exit(m.Run())
}

func runEntireStandIn(mode string, args []string) int {
	if mode == standInBroken {
		fmt.Fprintln(os.Stderr, "stand-in ERROR No version is set for shim: entire")
		return 1
	}
	if len(args) == 1 && args[0] == "--version" {
		// Drain stdin so a probe that reads the hook payload is caught by the
		// payload pass-through case.
		_, _ = io.Copy(io.Discard, os.Stdin) //nolint:errcheck // best effort
		fmt.Println("Entire CLI stand-in")
		return 0
	}
	switch mode {
	case standInWorking:
		if _, err := io.Copy(os.Stdout, os.Stdin); err != nil {
			return 1
		}
		return 0
	case standInHang:
		time.Sleep(time.Minute)
		return 0
	default:
		fmt.Fprintln(os.Stderr, "stand-in hook failed")
		return 1
	}
}

// wrapperCommand builds the wrapper the way agents run it: the stored string
// handed to sh, with PATH holding only sh and, unless mode is empty, the
// `entire` stand-in, so an `entire` installed on the host can't leak in.
func wrapperCommand(t *testing.T, wrapper, mode string) *exec.Cmd {
	t.Helper()

	binDir := t.TempDir()
	shPath, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("sh not available")
	}
	if err := os.Symlink(shPath, filepath.Join(binDir, "sh")); err != nil {
		t.Fatalf("link sh: %v", err)
	}
	env := []string{"PATH=" + binDir}
	if mode != "" {
		self, err := os.Executable()
		if err != nil {
			t.Fatalf("os.Executable: %v", err)
		}
		if err := os.Symlink(self, filepath.Join(binDir, "entire")); err != nil {
			t.Fatalf("link entire stand-in: %v", err)
		}
		env = append(env, standInEnv+"="+mode)
	}

	cmd := exec.CommandContext(context.Background(), filepath.Join(binDir, "sh"), "-c", wrapper)
	cmd.Env = env
	return cmd
}

func runWrapper(t *testing.T, wrapper, mode string) (string, int) {
	t.Helper()

	cmd := wrapperCommand(t, wrapper, mode)
	cmd.Stdin = strings.NewReader(hookPayload)
	out, err := cmd.Output()
	var exitErr *exec.ExitError
	switch {
	case err == nil:
		return string(out), 0
	case errors.As(err, &exitErr):
		return string(out), exitErr.ExitCode()
	default:
		t.Fatalf("run wrapper: %v", err)
		return "", 0
	}
}

func TestWarningWrappers_Execution(t *testing.T) {
	t.Parallel()

	command := "entire hooks test-agent session-start"

	jsonMessage := func(t *testing.T, stdout string) string {
		t.Helper()
		var got struct {
			SystemMessage string `json:"systemMessage"`
		}
		if err := json.Unmarshal([]byte(stdout), &got); err != nil {
			t.Fatalf("stdout is not a JSON hook response: %v\n%q", err, stdout)
		}
		return got.SystemMessage
	}
	plainMessage := func(_ *testing.T, stdout string) string {
		return strings.TrimSuffix(stdout, "\n")
	}

	wrappers := []struct {
		name    string
		wrapper string
		format  WarningFormat
		message func(*testing.T, string) string
	}{
		{"json", WrapProductionJSONWarningHookCommand(command, WarningFormatMultiLine), WarningFormatMultiLine, jsonMessage},
		{"plain text", WrapProductionPlainTextWarningHookCommand(command, WarningFormatSingleLine), WarningFormatSingleLine, plainMessage},
	}

	for _, w := range wrappers {
		t.Run(w.name+"/entire missing warns and exits 0", func(t *testing.T) {
			t.Parallel()
			stdout, code := runWrapper(t, w.wrapper, "")
			if code != 0 {
				t.Fatalf("exit code = %d, want 0", code)
			}
			if got, want := w.message(t, stdout), MissingEntireWarning(w.format); got != want {
				t.Fatalf("warning = %q, want %q", got, want)
			}
		})

		t.Run(w.name+"/entire that cannot run warns and exits 0", func(t *testing.T) {
			t.Parallel()
			stdout, code := runWrapper(t, w.wrapper, standInBroken)
			if code != 0 {
				t.Fatalf("exit code = %d, want 0", code)
			}
			if got, want := w.message(t, stdout), FailedEntireWarning(w.format); got != want {
				t.Fatalf("warning = %q, want %q", got, want)
			}
		})

		t.Run(w.name+"/working entire gets the payload and owns stdout", func(t *testing.T) {
			t.Parallel()
			stdout, code := runWrapper(t, w.wrapper, standInWorking)
			if code != 0 {
				t.Fatalf("exit code = %d, want 0", code)
			}
			if stdout != hookPayload {
				t.Fatalf("stdout = %q, want only entire's output %q", stdout, hookPayload)
			}
		})

		t.Run(w.name+"/a failing hook keeps its exit code and gets no warning", func(t *testing.T) {
			t.Parallel()
			stdout, code := runWrapper(t, w.wrapper, standInHookError)
			if code != 1 {
				t.Fatalf("exit code = %d, want the hook's 1", code)
			}
			if stdout != "" {
				t.Fatalf("stdout = %q, want nothing: the hook ran, so no install warning applies", stdout)
			}
		})

		// Agents enforce hook timeouts by signalling the process they started.
		// That must be entire itself, or a hung hook outlives the timeout and
		// keeps the agent's stdout pipe open.
		t.Run(w.name+"/signalling the hook process stops entire", func(t *testing.T) {
			t.Parallel()
			// exec drops the harness's own sh -c, which Debian's dash keeps as a
			// parent, so the signal reaches the wrapper's sh as it would when
			// the agent starts the wrapper directly.
			cmd := wrapperCommand(t, "exec "+w.wrapper, standInHang)
			cmd.Stdin = strings.NewReader(hookPayload)
			stdout, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatalf("stdout pipe: %v", err)
			}
			if err := cmd.Start(); err != nil {
				t.Fatalf("start wrapper: %v", err)
			}
			t.Cleanup(func() { _ = cmd.Wait() }) //nolint:errcheck // reaped only

			// Let the wrapper reach the hook before the "timeout" fires.
			time.Sleep(time.Second)
			if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
				t.Fatalf("signal wrapper: %v", err)
			}

			closed := make(chan struct{})
			go func() {
				_, _ = io.Copy(io.Discard, stdout) //nolint:errcheck // waiting for EOF only
				close(closed)
			}()
			select {
			case <-closed:
			case <-time.After(10 * time.Second):
				t.Fatal("stdout still open 10s after signalling the hook process: entire outlived it")
			}
		})
	}
}
