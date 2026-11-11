package main

import (
	"errors"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// dieFromSignalEnvVar, when set on a re-executed test binary, makes TestMain
// invoke dieFromSignal with the named signal instead of running the test suite.
// The parent process then inspects how the child died. This is how
// TestDieFromSignal_TerminatesBySignal exercises real process death without
// killing the test runner itself.
const dieFromSignalEnvVar = "ENTIRE_TEST_DIE_FROM_SIGNAL"

// runMainEnvVar, when set on a re-executed test binary, makes TestMain run
// main() with the variable's value as the command line (space-separated)
// instead of the test suite.
const runMainEnvVar = "ENTIRE_TEST_RUN_MAIN"

// sigCountEnvVar, when set on a re-executed test binary, makes it a plugin
// that counts the SIGINTs it receives: see runSigCountPlugin.
const sigCountEnvVar = "ENTIRE_TEST_SIGCOUNT_FILE"

func TestMain(m *testing.M) {
	// Child mode: exercise dieFromSignal for the named signal and let it
	// terminate this process. dieFromSignal only returns if the re-raise
	// couldn't be delivered, so the os.Exit fallback mirrors it.
	if name := os.Getenv(dieFromSignalEnvVar); name != "" {
		sig := os.Interrupt
		if name == "TERM" {
			sig = syscall.SIGTERM
		}
		dieFromSignal(sig)
		os.Exit(exitCodeForSignal(sig))
	}
	// Before runMainEnvVar: a plugin inherits the ENTIRE_* environment.
	if path := os.Getenv(sigCountEnvVar); path != "" {
		runSigCountPlugin(path)
	}
	if line := os.Getenv(runMainEnvVar); line != "" {
		os.Args = append([]string{"entire"}, strings.Fields(line)...)
		main()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// runSigCountPlugin is a plugin that marks path+".ready" once it is listening
// for SIGINT, and after the first one counts every SIGINT that arrives within
// the next 500ms, writes the count to path, and exits 0.
func runSigCountPlugin(path string) {
	sigs := make(chan os.Signal, 16)
	signal.Notify(sigs, os.Interrupt)
	if err := os.WriteFile(path+".ready", nil, 0o600); err != nil {
		os.Exit(2)
	}
	<-sigs
	n := 1
	window := time.After(500 * time.Millisecond)
	for counting := true; counting; {
		select {
		case <-sigs:
			n++
		case <-window:
			counting = false
		}
	}
	if err := os.WriteFile(path, []byte(strconv.Itoa(n)), 0o600); err != nil {
		os.Exit(2)
	}
	os.Exit(0)
}

// nonNumericSignal is an os.Signal that isn't a syscall.Signal, exercising
// exitCodeForSignal's fallback branch.
type nonNumericSignal struct{}

func (nonNumericSignal) String() string { return "non-numeric" }
func (nonNumericSignal) Signal()        {}

// TestDieFromSignal_TerminatesBySignal is the regression guard for the headline
// behavior: an enclosing `while true; do entire …; done` loop only breaks when
// the process is *killed by* the signal (WIFSIGNALED), not when it exits
// normally with code 130. A "simplification" of dieFromSignal back to a plain
// os.Exit(130) would leave the exit code looking right while silently breaking
// loop-escape — this test catches exactly that by re-executing the test binary
// in child mode and asserting it died by the signal.
func TestDieFromSignal_TerminatesBySignal(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("signal-to-self / WIFSIGNALED semantics do not apply on Windows")
	}

	tests := []struct {
		name string
		env  string
		want syscall.Signal
	}{
		{"SIGINT", "INT", syscall.SIGINT},
		{"SIGTERM", "TERM", syscall.SIGTERM},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			// -test.run=^$ matches no test; TestMain's child branch runs before
			// m.Run() and terminates the process, so no test actually executes.
			cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^$")
			cmd.Env = append(os.Environ(), dieFromSignalEnvVar+"="+tc.env)

			err := cmd.Run()

			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) {
				t.Fatalf("child did not exit with an error status; err=%v (expected death by signal)", err)
			}
			ws, ok := exitErr.Sys().(syscall.WaitStatus)
			if !ok {
				t.Fatalf("no syscall.WaitStatus available: %T", exitErr.Sys())
			}
			if !ws.Signaled() {
				t.Fatalf("child exited normally with code %d; want death by signal %v — dieFromSignal must re-raise, not os.Exit", ws.ExitStatus(), tc.want)
			}
			if ws.Signal() != tc.want {
				t.Fatalf("child killed by %v, want %v", ws.Signal(), tc.want)
			}
		})
	}
}

// TestExitCodeForSignal locks the conventional 128+signum mapping the
// Ctrl-C/SIGTERM fix relies on, so a future "simplification" back to a
// hardcoded 130 can't silently regress SIGTERM's 143.
func TestExitCodeForSignal(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		sig  os.Signal
		want int
	}{
		{"SIGINT", os.Interrupt, 130},
		{"SIGTERM", syscall.SIGTERM, 143},
		{"non-numeric signal falls back to 130", nonNumericSignal{}, 130},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := exitCodeForSignal(tc.sig); got != tc.want {
				t.Errorf("exitCodeForSignal(%v) = %d, want %d", tc.sig, got, tc.want)
			}
		})
	}
}

// A signal ignored at startup stays ignored: Notify would un-ignore it.
func TestShutdownSignals_SkipsSignalsIgnoredAtStartup(t *testing.T) {
	t.Parallel()
	none := func(os.Signal) bool { return false }
	ignoring := func(ignored ...os.Signal) func(os.Signal) bool {
		return func(sig os.Signal) bool { return slices.Contains(ignored, sig) }
	}
	tests := []struct {
		name    string
		goos    string
		ignored func(os.Signal) bool
		want    []os.Signal
	}{
		{"unix", "linux", none, []os.Signal{os.Interrupt, syscall.SIGTERM, syscall.SIGHUP}},
		{"unix under nohup", "linux", ignoring(syscall.SIGHUP), []os.Signal{os.Interrupt, syscall.SIGTERM}},
		{"unix background job", "darwin", ignoring(os.Interrupt), []os.Signal{syscall.SIGTERM, syscall.SIGHUP}},
		{"windows", "windows", none, []os.Signal{os.Interrupt}},
		{"windows, SIGINT ignored", "windows", ignoring(os.Interrupt), []os.Signal{}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := shutdownSignals(tc.goos, tc.ignored); !slices.Equal(got, tc.want) {
				t.Errorf("shutdownSignals(%s) = %v, want %v", tc.goos, got, tc.want)
			}
		})
	}
}
