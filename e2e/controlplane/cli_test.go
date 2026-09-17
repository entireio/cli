//go:build e2e

package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/execx"
	"github.com/entireio/cli/e2e/entire"
	"github.com/stretchr/testify/require"
)

// runEntire runs the entire binary in dir, detached from any terminal, and
// returns the two streams separately: --json output goes to stdout while
// progress notes such as "Cloning …" go to stderr.
func runEntire(t *testing.T, dir string, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	return runEntireWithTimeout(t, dir, 2*time.Minute, args...)
}

func runEntireWithTimeout(t *testing.T, dir string, timeout time.Duration, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	// Each call gets a fresh deadline, including cleanup after a timed-out step.
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := execx.NonInteractive(ctx, entire.BinPath(), args...)
	// A git transport descendant can keep the output pipes open after the
	// direct child is killed. Bound that wait too, so cleanup can still run.
	cmd.WaitDelay = execx.KillWaitDelay
	cmd.Dir = dir
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	err = cmd.Run()
	if ctx.Err() != nil {
		err = errors.Join(err, ctx.Err())
	}
	return out.String(), errOut.String(), err
}

// mustRunEntire is runEntire for a step the test cannot continue without.
func mustRunEntire(t *testing.T, dir string, args ...string) (stdout, stderr string) {
	t.Helper()
	stdout, stderr, err := runEntire(t, dir, args...)
	require.NoError(t, err, "entire %s\nstdout:\n%s\nstderr:\n%s", strings.Join(args, " "), stdout, stderr)
	return stdout, stderr
}

// decodeJSON fails with the raw stdout when it is not the expected JSON, so a
// command that exits 0 with a message on stdout is reported rather than read
// back as an empty struct.
func decodeJSON[T any](t *testing.T, stdout string) T {
	t.Helper()
	var v T
	require.NoError(t, json.Unmarshal([]byte(stdout), &v), "stdout is not JSON:\n%s", stdout)
	return v
}
