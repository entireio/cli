// Package testutil holds shared test helpers for the agent package and
// its sub-packages. Not for production use.
package testutil

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"time"
)

const (
	fakeStreamMarkerArg     = "__entire_test_fake_stream_process__"
	blockingStreamMarkerArg = "__entire_test_blocking_stream_process__"
)

func init() { //nolint:gochecknoinits // child test binaries must intercept before testing.Main runs
	args := os.Args
	if len(args) > 0 && args[len(args)-1] == blockingStreamMarkerArg {
		time.Sleep(time.Hour)
		os.Exit(0)
	}
	if len(args) < 5 || args[len(args)-4] != fakeStreamMarkerArg {
		return
	}
	writeDecodedFixture(os.Stdout, args[len(args)-3])
	writeDecodedFixture(os.Stderr, args[len(args)-2])
	exitCode, err := strconv.Atoi(args[len(args)-1])
	if err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "invalid fake stream exit code:", err)
		os.Exit(125)
	}
	os.Exit(exitCode)
}

// BlockingCmd returns a portable current-test-binary subprocess that remains
// alive until its context is canceled.
func BlockingCmd(ctx context.Context) *exec.Cmd {
	return exec.CommandContext(ctx, os.Args[0], "-test.run=^$", "--", blockingStreamMarkerArg)
}

// ReadCommandStdin rewinds and reads a command's seekable stdin after a fake
// subprocess run so tests can pin the exact prompt transport contract.
func ReadCommandStdin(cmd *exec.Cmd) (string, error) {
	if cmd == nil || cmd.Stdin == nil {
		return "", errors.New("command stdin is nil")
	}
	seeker, ok := cmd.Stdin.(io.ReadSeeker)
	if !ok {
		return "", fmt.Errorf("command stdin %T is not seekable", cmd.Stdin)
	}
	if _, err := seeker.Seek(0, io.SeekStart); err != nil {
		return "", fmt.Errorf("rewind command stdin: %w", err)
	}
	data, err := io.ReadAll(seeker)
	if err != nil {
		return "", fmt.Errorf("read command stdin: %w", err)
	}
	return string(data), nil
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
	return func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		return exec.CommandContext(ctx, os.Args[0], "-test.run=^$", "--",
			fakeStreamMarkerArg,
			base64.StdEncoding.EncodeToString([]byte(stdout)),
			base64.StdEncoding.EncodeToString([]byte(stderr)),
			strconv.Itoa(exitCode),
		)
	}
}
