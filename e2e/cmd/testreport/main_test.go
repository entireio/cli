package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestReportCommand(t *testing.T) {
	t.Parallel()
	binary := filepath.Join(t.TempDir(), "testreport")
	if runtime.GOOS == "windows" {
		binary += ".exe"
	}
	if out, err := exec.CommandContext(t.Context(), "go", "build", "-o", binary, ".").CombinedOutput(); err != nil {
		t.Fatalf("build reporter: %v\n%s", err, out)
	}

	for _, tc := range []struct {
		name   string
		events string
		strict bool
		code   int
		banner string
	}{
		{"empty", "", true, 1, "NO TESTS RAN"},
		{"package only", "{\"Action\":\"pass\",\"Package\":\"tests\"}\n", true, 1, "NO TESTS RAN"},
		{"malformed only", "not json\n", true, 1, "NO TESTS RAN"},
		{"diagnostic empty", "", false, 0, "NO TESTS RAN"},
		{"pass", "{\"Action\":\"pass\",\"Test\":\"TestFoo\"}\n", true, 0, "ALL 1 TESTS PASSED"},
		{"skip", "{\"Action\":\"skip\",\"Test\":\"TestFoo\"}\n", true, 0, "Skipped: 1"},
		{"children skip", "{\"Action\":\"skip\",\"Test\":\"TestFoo/agent\"}\n{\"Action\":\"pass\",\"Test\":\"TestFoo\"}\n", true, 0, "Total: 1"},
		{"rerun", "{\"Action\":\"fail\",\"Test\":\"TestFoo\"}\n{\"Action\":\"pass\",\"Test\":\"TestFoo\"}\n", true, 0, "ALL 1 TESTS PASSED"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			events := filepath.Join(dir, "events.json")
			if err := os.WriteFile(events, []byte(tc.events), 0o600); err != nil {
				t.Fatal(err)
			}
			args := []string{"-o", filepath.Join(dir, "report.txt")}
			if tc.strict {
				args = append(args, "-fail-on-empty")
			}
			cmd := exec.CommandContext(t.Context(), binary, append(args, events)...)
			out, err := cmd.CombinedOutput()
			if cmd.ProcessState == nil {
				t.Fatalf("start reporter: %v", err)
			}
			if code := cmd.ProcessState.ExitCode(); code != tc.code {
				t.Fatalf("exit = %d, want %d: %s", code, tc.code, out)
			}
			if tc.code != 0 && !strings.Contains(string(out), events) {
				t.Errorf("failure does not name the input: %s", out)
			}
			for _, name := range []string{"report.txt", "report.nocolor.txt"} {
				data, err := os.ReadFile(filepath.Join(dir, name))
				if err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(string(data), tc.banner) {
					t.Errorf("%s missing %q: %s", name, tc.banner, data)
				}
			}
		})
	}
}
