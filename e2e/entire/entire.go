package entire

import (
	"encoding/json"
	"log"
	"os"
	"os/exec"
	"strings"
	"testing"
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

// RewindPoint represents a single entry from `entire rewind --list`.
type RewindPoint struct {
	ID             string `json:"id"`
	Message        string `json:"message"`
	MetadataDir    string `json:"metadata_dir"`
	Date           string `json:"date"`
	IsLogsOnly     bool   `json:"is_logs_only"`
	CondensationID string `json:"condensation_id"`
	SessionID      string `json:"session_id"`
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

// RewindList runs `entire rewind --list` and parses the JSON output.
func RewindList(t *testing.T, dir string) []RewindPoint {
	t.Helper()
	out := run(t, dir, "rewind", "--list")

	var points []RewindPoint
	if err := json.Unmarshal([]byte(out), &points); err != nil {
		t.Fatalf("parse rewind list: %v\nraw output: %s", err, out)
	}
	return points
}

// Rewind runs `entire rewind --to <id>`. Returns an error instead of
// failing the test, since callers may test failure cases.
func Rewind(t *testing.T, dir, id string) error {
	t.Helper()
	return runErr(dir, "rewind", "--to", id)
}

// RewindLogsOnly runs `entire rewind --to <id> --logs-only`.
func RewindLogsOnly(t *testing.T, dir, id string) error {
	t.Helper()
	return runErr(dir, "rewind", "--to", id, "--logs-only")
}

// run executes an `entire` subcommand in dir and fails the test on error.
func run(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command(BinPath(), args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "ENTIRE_TEST_TTY=0")

	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("entire %s failed: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// runErr executes an `entire` subcommand in dir and returns any error.
func runErr(dir string, args ...string) error {
	cmd := exec.Command(BinPath(), args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "ENTIRE_TEST_TTY=0")

	out, err := cmd.CombinedOutput()
	if err != nil {
		return &ExecError{
			Args:   args,
			Err:    err,
			Output: string(out),
		}
	}
	return nil
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
	return run(t, dir, "explain", "--checkpoint", checkpointID)
}

// ExplainGenerate runs `entire explain --checkpoint <id> --generate`.
// Returns (output, error) — doesn't fail test since callers may test failure cases.
func ExplainGenerate(dir, checkpointID string) (string, error) {
	return runOutput(dir, "explain", "--checkpoint", checkpointID, "--generate")
}

// ExplainCommit runs `entire explain --commit <ref>`.
// Returns (output, error) — for testing failure cases.
func ExplainCommit(dir, ref string) (string, error) {
	return runOutput(dir, "explain", "--commit", ref)
}

// Resume runs `entire resume <branch> --force` and returns the output.
func Resume(dir, branch string) (string, error) {
	return runOutput(dir, "resume", branch, "--force")
}

// ResumeWithEnv runs `entire resume <branch> --force` with extra env vars.
func ResumeWithEnv(dir, branch string, extraEnv []string) (string, error) {
	return runOutputEnv(dir, extraEnv, "resume", branch, "--force")
}

// runOutput executes an `entire` subcommand and returns (output, error).
func runOutput(dir string, args ...string) (string, error) {
	return runOutputEnv(dir, nil, args...)
}

func runOutputEnv(dir string, extraEnv []string, args ...string) (string, error) {
	cmd := exec.Command(BinPath(), args...)
	cmd.Dir = dir
	cmd.Env = append(append([]string{}, os.Environ()...), "ENTIRE_TEST_TTY=0")
	cmd.Env = append(cmd.Env, extraEnv...)

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
