package cli

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// newGloballyTrackedDiagRepo creates a repo with NO repo-level setup and the
// user-global tier enabled, chdirs into it, and writes one line into the
// invisible-routed log file (under the git common dir, never the worktree).
// Fixture for the doctor logs / doctor bundle / doctor trace tests proving
// the diagnostics find routed logs. No t.Parallel in callers: t.Chdir+t.Setenv.
func newGloballyTrackedDiagRepo(t *testing.T, logLine string) {
	t.Helper()
	dir := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		dir = resolved
	}
	testutil.InitRepo(t, dir)
	t.Chdir(dir)
	cfgDir := t.TempDir()
	t.Setenv("ENTIRE_CONFIG_DIR", cfgDir)
	if err := os.WriteFile(filepath.Join(cfgDir, "settings.json"), []byte(`{"global":{"enabled":true}}`), 0o600); err != nil {
		t.Fatalf("write user settings: %v", err)
	}
	paths.ClearWorktreeRootCache()
	paths.ClearInvisibleRuntimeCache()
	t.Cleanup(func() {
		paths.ClearWorktreeRootCache()
		paths.ClearInvisibleRuntimeCache()
	})

	logPath, err := paths.AbsPath(context.Background(), filepath.Join(logging.LogsDir, "entire.log"))
	if err != nil {
		t.Fatalf("resolve routed log path: %v", err)
	}
	if !strings.Contains(logPath, filepath.Join(".git", "entire", "worktree")) {
		t.Fatalf("fixture log path is not routed to the git common dir: %s", logPath)
	}
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		t.Fatalf("mkdir routed logs dir: %v", err)
	}
	if err := os.WriteFile(logPath, []byte(logLine+"\n"), 0o600); err != nil {
		t.Fatalf("write routed log: %v", err)
	}
}

// TestDoctorLogsCmd_FindsRoutedLogsInGloballyTrackedRepo pins that `entire
// doctor logs` resolves the log file through paths.AbsPath: in a globally
// tracked repo the logs live under the git common dir, and a worktree-only
// join would report "No log file" for exactly these repos.
func TestDoctorLogsCmd_FindsRoutedLogsInGloballyTrackedRepo(t *testing.T) {
	newGloballyTrackedDiagRepo(t, "routed-log-line-doctor-logs")

	cmd := newDoctorLogsCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("doctor logs: %v", err)
	}
	if strings.Contains(out.String(), "No log file at") {
		t.Fatalf("doctor logs did not find the routed log file:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "routed-log-line-doctor-logs") {
		t.Fatalf("doctor logs output missing routed log line:\n%s", out.String())
	}
}

func TestReadLastNLines_ShortInput(t *testing.T) {
	t.Parallel()

	r := strings.NewReader("a\nb\nc\n")
	got, err := readLastNLines(r, 5)
	if err != nil {
		t.Fatalf("readLastNLines: %v", err)
	}
	want := []string{"a\n", "b\n", "c\n"}
	if len(got) != len(want) {
		t.Fatalf("got %d lines, want %d (%v)", len(got), len(want), got)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("line %d = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestReadLastNLines_LongInputTruncated(t *testing.T) {
	t.Parallel()

	r := strings.NewReader("a\nb\nc\nd\ne\nf\n")
	got, err := readLastNLines(r, 3)
	if err != nil {
		t.Fatalf("readLastNLines: %v", err)
	}
	want := []string{"d\n", "e\n", "f\n"}
	if len(got) != len(want) {
		t.Fatalf("got %d lines, want %d (%v)", len(got), len(want), got)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("line %d = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestPrintTail_ZeroNCopiesAll(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "log.txt")
	contents := "alpha\nbeta\ngamma\n"
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write log: %v", err)
	}

	var buf bytes.Buffer
	if err := printTail(&buf, path, 0); err != nil {
		t.Fatalf("printTail: %v", err)
	}
	if buf.String() != contents {
		t.Errorf("printTail copy = %q, want %q", buf.String(), contents)
	}
}

func TestPrintTail_TailsLastN(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "log.txt")
	contents := "1\n2\n3\n4\n5\n"
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write log: %v", err)
	}

	var buf bytes.Buffer
	if err := printTail(&buf, path, 2); err != nil {
		t.Fatalf("printTail: %v", err)
	}
	if buf.String() != "4\n5\n" {
		t.Errorf("printTail tail = %q, want \"4\\n5\\n\"", buf.String())
	}
}

func TestFollowFile_ExitsWhenContextCanceled(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "log.txt")
	if err := os.WriteFile(path, []byte("existing\n"), 0o600); err != nil {
		t.Fatalf("write log: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	var buf bytes.Buffer
	if err := followFile(ctx, &buf, path); err != nil {
		t.Fatalf("followFile: %v", err)
	}
	if buf.Len() != 0 {
		t.Fatalf("followFile wrote %q after cancellation", buf.String())
	}
}
