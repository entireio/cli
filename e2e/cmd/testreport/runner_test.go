package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// Stub only external commands: execute the actual shell runner to pin status
// precedence, regex quoting, native credential forwarding, and deterministic retry policy.
func TestSharedRunner(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("POSIX executable stubs; reporter command tests also run on Windows")
	}
	runner, err := filepath.Abs("../../../scripts/e2e-run.sh")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name      string
		agent     string
		testRC    int
		reportRC  int
		bootstrap int
		want      int
		external  bool
	}{
		{"success without version file", "claude-code", 0, 0, 0, 0, false},
		{"empty report", "claude-code", 0, 1, 0, 1, false},
		{"test failure wins", "claude-code", 7, 1, 0, 7, false},
		{"bootstrap failure wins", "claude-code", 0, 1, 9, 9, false},
		{"deterministic", "roger-roger", 0, 0, 0, 0, false},
		{"nightly reporter", "claude-code", 0, 0, 0, 0, true},
		{"nightly empty report", "claude-code", 0, 1, 0, 1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			bin := filepath.Join(dir, "bin")
			if err := os.Mkdir(bin, 0o755); err != nil {
				t.Fatal(err)
			}
			write := func(name, body string) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\nset -eu\n"+body), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			write("gotestsum", `printf '%s\n' "$@" > "$TRACE/args"
[ "$E2E_CHECKPOINT_STORE" = git-refs ]
if [ "$E2E_AGENT" = claude-code ]; then
  [ "$ANTHROPIC_API_KEY" = fixture-token ]
fi
exit "$TEST_RC"
`)
			write("go", `if [ "$2" = ./e2e/bootstrap ]; then exit "$BOOTSTRAP_RC"; fi
[ -z "${E2E_TESTREPORT_BIN:-}" ]
[ "$2" = ./e2e/cmd/testreport ]
[ "$3" = -fail-on-empty ]
echo report
exit "$REPORT_RC"
`)
			write("external reporter", `[ "$1" = -fail-on-empty ]
echo report
exit "$REPORT_RC"
`)
			write("roger-roger", "exit 0\n")
			write("entire-agent-roger-roger", "exit 0\n")
			filter := `^TestFoo/(a b|c)$; $(echo unsafe)`
			cmd := exec.CommandContext(t.Context(), "sh", runner, tc.agent, filter)
			cmd.Dir = dir
			cmd.Env = append(os.Environ(),
				"PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
				"E2E_ENTIRE_BIN="+filepath.Join(dir, "entire"),
				"E2E_ARTIFACT_DIR="+filepath.Join(dir, "artifacts"),
				"TRACE="+dir, "E2E_BOOTSTRAP=1", "E2E_REQUIRE_CREDENTIALS=1",
				fmt.Sprintf("TEST_RC=%d", tc.testRC),
				fmt.Sprintf("REPORT_RC=%d", tc.reportRC),
				fmt.Sprintf("BOOTSTRAP_RC=%d", tc.bootstrap),
				"E2E_REPORT_SUFFIX=", "E2E_TESTREPORT_BIN=", "E2E_CHECKPOINT_STORE=",
			)
			if tc.agent == "claude-code" {
				cmd.Env = append(cmd.Env, "ANTHROPIC_API_KEY=fixture-token")
			}
			if tc.external {
				cmd.Env = append(cmd.Env, "E2E_TESTREPORT_BIN="+filepath.Join(bin, "external reporter"))
			}
			out, err := cmd.CombinedOutput()
			if cmd.ProcessState == nil {
				t.Fatalf("start runner: %v", err)
			}
			if code := cmd.ProcessState.ExitCode(); code != tc.want {
				t.Fatalf("exit = %d, want %d: %s", code, tc.want, out)
			}
			if !strings.Contains(string(out), "report\n") || !strings.HasSuffix(string(out), "artifacts: "+filepath.Join(dir, "artifacts")+"\n") {
				t.Errorf("missing report or triage tail: %s", out)
			}
			if strings.Contains(string(out), "fixture-token") {
				t.Error("token appeared in output")
			}
			args, err := os.ReadFile(filepath.Join(dir, "args"))
			if tc.bootstrap != 0 {
				if !os.IsNotExist(err) {
					t.Fatal("tests ran after bootstrap failed")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(args), "-run\n"+filter+"\n") {
				t.Errorf("filter was not passed literally: %s", args)
			}
			if retries := strings.Contains(string(args), "--rerun-fails=1"); retries != (tc.agent != "roger-roger") {
				t.Errorf("wrong retry policy: %s", args)
			}
		})
	}
}
