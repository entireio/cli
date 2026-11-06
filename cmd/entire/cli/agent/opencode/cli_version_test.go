package opencode

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/paths"
)

// useOpenCodeCLI pins the detected OpenCode version for one test.
func useOpenCodeCLI(t *testing.T, legacy bool) {
	t.Helper()
	prev := detectOpenCodeCLI
	detectOpenCodeCLI = func(context.Context) openCodeCLI { return openCodeCLI{legacy: legacy} }
	t.Cleanup(func() { detectOpenCodeCLI = prev })
}

// stubOpenCode puts a shell-script `opencode` first on PATH.
func stubOpenCode(t *testing.T, script string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("stub opencode is a shell script")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "opencode"), []byte("#!/bin/sh\n"+script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+"/usr/bin:/bin")
}

func TestProbeOpenCodeCLI(t *testing.T) {
	// No t.Parallel: t.Setenv.
	for _, tt := range []struct {
		name   string
		script string
		legacy bool
	}{
		{"1.x", "echo 1.18.33\n", true},
		{"2.x", "echo 'opencode v2.1.1'\n", false},
		{"unreadable version", "echo nightly\n", false},
		{"failing probe", "exit 3\n", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			stubOpenCode(t, tt.script)
			cli, cacheable := probeOpenCodeCLI(t.Context())
			if cli.legacy != tt.legacy {
				t.Fatalf("probe = %+v, want legacy=%v", cli, tt.legacy)
			}
			if want := tt.name != "failing probe"; cacheable != want {
				t.Fatalf("cacheable = %v, want %v", cacheable, want)
			}
		})
	}
}

// OpenCode 2 exports with `session export --standalone`.
func TestRunOpenCodeExportToFile_OpenCode2(t *testing.T) {
	// No t.Parallel: t.Setenv.
	useOpenCodeCLI(t, false)
	dir := t.TempDir()
	argsFile := filepath.Join(dir, "args")
	stubOpenCode(t, `echo "$@" > `+argsFile+"\n"+`cat "`+filepath.Join(dir, "export.json")+`"`+"\n")
	if err := os.WriteFile(filepath.Join(dir, "export.json"), loadOpenCode2Fixture(t, "tools"), 0o600); err != nil {
		t.Fatal(err)
	}
	root := mustOpenRoot(t, dir)
	if err := runOpenCodeExportToFile(t.Context(), root, "ses_x", "staged"); err != nil {
		t.Fatal(err)
	}
	args, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(args)) != "session export --standalone ses_x" {
		t.Errorf("opencode args = %q", args)
	}
}

// On OpenCode 2, resuming never deletes the session: one OpenCode still has
// resumes as it is, and a missing one is imported from the OpenCode 2
// messages the stored transcript keeps.
func TestWriteSession_OpenCode2(t *testing.T) {
	// No t.Parallel: t.Setenv.
	useOpenCodeCLI(t, false)
	stored, err := normalizeOpenCodeExport(loadOpenCode2Fixture(t, "tools"))
	if err != nil {
		t.Fatal(err)
	}
	const sessionID = "ses_ea33499d4ffeLqgkWykI2UtQ0S"
	write := func(t *testing.T, script string, data []byte) (string, []string) {
		t.Helper()
		dir := t.TempDir()
		log := filepath.Join(dir, "calls")
		imported := filepath.Join(dir, "imported.json")
		stubOpenCode(t, `echo "$@" >> `+log+"\n"+`case "$2" in import) cp "$4" `+imported+`; pwd >> `+log+`;; esac`+"\n"+script)
		repo := t.TempDir()
		err := (&OpenCodeAgent{}).WriteSession(t.Context(), &agent.AgentSession{SessionID: sessionID, RepoPath: repo, NativeData: data})
		calls, readErr := os.ReadFile(log)
		if readErr != nil {
			t.Fatal(readErr)
		}
		var errText string
		if err != nil {
			errText = err.Error()
		}
		lines := strings.Split(strings.TrimSpace(string(calls)), "\n")
		for _, line := range lines {
			if strings.HasPrefix(line, "session delete") {
				t.Errorf("OpenCode 2 session deleted: %q", line)
			}
		}
		if body, readErr := os.ReadFile(imported); readErr == nil {
			if !strings.Contains(string(body), `"directory":"`+repo+`"`) {
				t.Errorf("import location is not the resuming worktree: %.300s", body)
			}
		}
		return errText, lines
	}

	t.Run("present", func(t *testing.T) {
		errText, calls := write(t, "printf '{}'\n", stored)
		if errText != "" || len(calls) != 1 || calls[0] != "session export --standalone "+sessionID {
			t.Fatalf("err %q, calls %q; want only the existence check", errText, calls)
		}
	})
	t.Run("missing", func(t *testing.T) {
		errText, calls := write(t, `if [ "$2" = export ]; then echo 'Session not found: x' >&2; exit 1; fi`+"\n", stored)
		if errText != "" || len(calls) != 3 || !strings.HasPrefix(calls[1], "session import --standalone ") {
			t.Fatalf("err %q, calls %q; want an existence check and an import", errText, calls)
		}
	})
	t.Run("missing, captured from OpenCode 1", func(t *testing.T) {
		v1 := []byte(`{"info":{"id":"` + sessionID + `"},"messages":[{"info":{"id":"m1","role":"user","time":{"created":1}},"parts":[]}]}`)
		errText, calls := write(t, `if [ "$2" = export ]; then echo 'Session not found: x' >&2; exit 1; fi`+"\n", v1)
		if !strings.Contains(errText, "no OpenCode 2 messages") || len(calls) != 1 {
			t.Fatalf("err %q, calls %q; want a clear OpenCode 1 error and no import", errText, calls)
		}
	})
}

// The capture stores an OpenCode 2 export in the 1.x shape the rest of Entire
// reads.
func TestFetchAndCacheExport_NormalizesOpenCode2(t *testing.T) {
	// Not parallel: t.Chdir and stubExport.
	fixture := loadOpenCode2Fixture(t, "tools") // before t.Chdir: testdata is relative
	t.Chdir(t.TempDir())
	paths.ClearWorktreeRootCache()
	t.Cleanup(paths.ClearWorktreeRootCache)
	stubExport(t, func(_ context.Context, root *os.Root, _, outputName string) error {
		return root.WriteFile(outputName, fixture, 0o600)
	})

	path, err := (&OpenCodeAgent{}).fetchAndCacheExport(t.Context(), "ses_v2")
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if isOpenCodeV2Export(data) || !json.Valid(data) {
		t.Fatalf("cached transcript is not a normalized export: %.200s", data)
	}
	prompts, err := ExtractAllUserPrompts(data)
	if err != nil || len(prompts) != 1 {
		t.Fatalf("prompts = %q, err %v", prompts, err)
	}
}

// On OpenCode 1, resume imports the stored transcript without the session's
// permission rules: they come from checkpoint data, and whoever can push that
// must not change tool approval in the resuming user's OpenCode.
func TestWriteSession_OpenCode1DropsStoredPermissions(t *testing.T) {
	// No t.Parallel: t.Setenv.
	useOpenCodeCLI(t, true)
	dir := t.TempDir()
	imported := filepath.Join(dir, "imported.json")
	stubOpenCode(t, `if [ "$1" = import ]; then cp "$2" `+imported+`; fi`+"\n")
	stored := `{"info":{"id":"ses_1","title":"t","permission":[{"permission":"*","pattern":"*","action":"allow"}]},` +
		`"messages":[{"info":{"id":"m1","role":"user","time":{"created":1}},"parts":[{"type":"text","text":"hi"}]}]}`
	err := (&OpenCodeAgent{}).WriteSession(t.Context(), &agent.AgentSession{SessionID: "ses_1", NativeData: []byte(stored)})
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(imported)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Info     map[string]any `json:"info"`
		Messages []any          `json:"messages"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	if _, ok := doc.Info["permission"]; ok {
		t.Errorf("stored permission rules imported: %s", data)
	}
	if doc.Info["id"] != "ses_1" || doc.Info["title"] != "t" || len(doc.Messages) != 1 {
		t.Errorf("import lost session data: %s", data)
	}
}

// When the version could not be read, the export command is a guess; an
// OpenCode 1 that rejects OpenCode 2's command still gets exported.
func TestRunOpenCodeExportToFile_GuessedVersionFallsBack(t *testing.T) {
	// No t.Parallel: t.Setenv.
	prev := detectOpenCodeCLI
	detectOpenCodeCLI = func(context.Context) openCodeCLI { return openCodeCLI{guessed: true} }
	t.Cleanup(func() { detectOpenCodeCLI = prev })
	dir := t.TempDir()
	// An OpenCode 1: rejects `session export --standalone`, accepts `export <id>`.
	stubOpenCode(t, `if [ "$1" = export ]; then printf '{"info":{"id":"%s"},"messages":[]}' "$2"; else echo 'Unrecognized flag: --standalone' >&2; exit 1; fi`+"\n")
	if err := runOpenCodeExportToFile(t.Context(), mustOpenRoot(t, dir), "ses_x", "staged"); err != nil {
		t.Fatalf("export with a guessed version did not fall back: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "staged"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != `{"info":{"id":"ses_x"},"messages":[]}` {
		t.Fatalf("export = %q; want only the fallback's output", got)
	}
}
