package entire

import (
	"context"
	"log"
	"os"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/execx"
)

// BinPath returns the path to the entire binary from E2E_ENTIRE_BIN.
// The mise test:e2e tasks set this automatically via `mise run build`.
func BinPath() string {
	p := os.Getenv("E2E_ENTIRE_BIN")
	if p == "" {
		log.Fatal("entire: E2E_ENTIRE_BIN not set — run tests via `mise run test:e2e`")
	}
	return p
}

// Enable runs `entire enable` for the given agent with telemetry disabled.
func Enable(t *testing.T, dir, agent string) {
	t.Helper()
	run(t, dir, "enable", "--agent", agent, "--telemetry=false")
}

// Disable runs `entire disable` in the given directory.
func Disable(t *testing.T, dir string) {
	t.Helper()
	run(t, dir, "disable")
}

// Doctor runs `entire doctor --force` and returns the output.
func Doctor(t *testing.T, dir string) string {
	t.Helper()
	return run(t, dir, "doctor", "--force")
}

// CleanDryRun runs `entire clean --dry-run` and returns the output.
func CleanDryRun(t *testing.T, dir string) string {
	t.Helper()
	return run(t, dir, "clean", "--dry-run")
}

// CleanForce runs `entire clean --force` and returns the output.
func CleanForce(t *testing.T, dir string) string {
	t.Helper()
	return run(t, dir, "clean", "--force")
}

// run executes an `entire` subcommand in dir and fails the test on error.
func run(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := execx.NonInteractive(context.Background(), BinPath(), args...)
	cmd.Dir = dir
	cmd.Env = os.Environ()

	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("entire %s failed: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// ExecError wraps an entire CLI execution failure with its output.
type ExecError struct {
	Args   []string
	Err    error
	Output string
}

func (e *ExecError) Error() string {
	return "entire " + strings.Join(e.Args, " ") + ": " + e.Err.Error() + "\n" + e.Output
}

func (e *ExecError) Unwrap() error {
	return e.Err
}

// Explain runs `entire explain --checkpoint <id>` and returns the output.
func Explain(t *testing.T, dir, checkpointID string) string {
	t.Helper()
	return run(t, dir, "checkpoint", "explain", "--checkpoint", checkpointID)
}

// AttachWithEnv runs `entire attach <session-id> --agent <agent> --force`
// with extra env vars.
func AttachWithEnv(dir string, extraEnv []string, sessionID, agent string) (string, error) {
	return runOutputEnv(dir, extraEnv, "session", "attach", sessionID, "--agent", agent, "--force")
}

// Resume runs `entire resume <branch> --force` and returns the output.
func Resume(dir, branch string) (string, error) {
	return runOutput(dir, "session", "resume", branch, "--force")
}

// ResumeWithEnv runs `entire resume <branch> --force` with extra env vars.
func ResumeWithEnv(dir, branch string, extraEnv []string) (string, error) {
	return runOutputEnv(dir, extraEnv, "session", "resume", branch, "--force")
}

// runOutput executes an `entire` subcommand and returns (output, error).
func runOutput(dir string, args ...string) (string, error) {
	return runOutputEnv(dir, nil, args...)
}

func runOutputEnv(dir string, extraEnv []string, args ...string) (string, error) {
	cmd := execx.NonInteractive(context.Background(), BinPath(), args...)
	cmd.Dir = dir
	cmd.Env = append(append([]string{}, os.Environ()...), extraEnv...)

	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), &ExecError{
			Args:   args,
			Err:    err,
			Output: string(out),
		}
	}
	return strings.TrimSpace(string(out)), nil
}
