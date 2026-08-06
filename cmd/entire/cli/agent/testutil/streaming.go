// Package testutil holds shared test helpers for the agent package and
// its sub-packages. Not for production use.
package testutil

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"time"
)

const (
	fakeStreamMarkerArg    = "__entire_test_fake_stream_process__"
	fakeStreamHangSentinel = "hang"
)

func init() { //nolint:gochecknoinits // child test binaries must intercept before testing.Main runs
	args := os.Args
	if len(args) < 5 || args[len(args)-4] != fakeStreamMarkerArg {
		return
	}
	// Write stderr BEFORE stdout: hang-mode tests kill this child as soon as
	// they observe a progress event (i.e. stdout output), and asserting on
	// captured stderr is only deterministic if it was fully written before
	// any stdout could have been seen.
	writeDecodedFixture(os.Stderr, args[len(args)-2])
	writeDecodedFixture(os.Stdout, args[len(args)-3])
	if args[len(args)-1] == fakeStreamHangSentinel {
		// Block until killed by the parent's ctx cancellation. Sleep instead
		// of select{} so the runtime's deadlock detector doesn't fire and
		// pollute the captured stderr.
		for {
			time.Sleep(time.Hour)
		}
	}
	exitCode, err := strconv.Atoi(args[len(args)-1])
	if err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "invalid fake stream exit code:", err)
		os.Exit(125)
	}
	os.Exit(exitCode)
}

func writeDecodedFixture(output *os.File, encoded string) {
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "invalid fake stream fixture:", err)
		os.Exit(125)
	}
	if _, err := output.Write(data); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "write fake stream fixture:", err)
		os.Exit(125)
	}
}

// FakeStreamCmd returns a CommandRunner factory whose *exec.Cmd, when
// Start()'d and Wait()'d, produces stdout/stderr/exit-code as configured.
// It relaunches the current Go test binary and is portable across supported
// platforms; package init handles the marked child before testing.Main runs.
func FakeStreamCmd(stdout, stderr string, exitCode int) func(ctx context.Context, name string, args ...string) *exec.Cmd {
	return fakeStreamCmd(stdout, stderr, strconv.Itoa(exitCode))
}

// FakeStreamCmdHang is FakeStreamCmd whose child writes the fixtures and then
// blocks until killed — it never exits on its own. Wire it to the real test
// ctx (do not detach) to deterministically exercise context-kill behavior:
// cancel the ctx and the child dies by signal with the fixtures already
// written to the pipe.
func FakeStreamCmdHang(stdout, stderr string) func(ctx context.Context, name string, args ...string) *exec.Cmd {
	return fakeStreamCmd(stdout, stderr, fakeStreamHangSentinel)
}

func fakeStreamCmd(stdout, stderr, exitArg string) func(ctx context.Context, name string, args ...string) *exec.Cmd {
	return func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		return exec.CommandContext(ctx, os.Args[0], "-test.run=^$", "--",
			fakeStreamMarkerArg,
			base64.StdEncoding.EncodeToString([]byte(stdout)),
			base64.StdEncoding.EncodeToString([]byte(stderr)),
			exitArg,
		)
	}
}
