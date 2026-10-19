package antigravity

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
)

func writeHooksJSON(t *testing.T, dir, content string) string {
	t.Helper()
	path := filepath.Join(dir, ".agents", "hooks.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// An install from before the Stop timeout leaves agy's 30s default in force,
// shorter than the turn-end budget derived from stopHookTimeoutSeconds.
func TestRepairHookTimeouts_AddsMissingStopTimeout(t *testing.T) {
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)
	stopCmd := buildEntireHookConfig().Stop[0].Command
	path := writeHooksJSON(t, tmpDir, fmt.Sprintf(`{
  "mine": {"Stop": [{"type": "command", "command": "my-hook"}]},
  "entire": {
    "futureKey": true,
    "Stop": [{"type": "command", "command": %q, "futureHandlerKey": 1}]
  }
}`, stopCmd))

	changed, err := (&AntigravityAgent{}).RepairHookTimeouts(context.Background())
	if err != nil {
		t.Fatalf("RepairHookTimeouts: %v", err)
	}
	if !changed {
		t.Fatal("changed = false, want true")
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var file map[string]map[string]json.RawMessage
	if err := json.Unmarshal(data, &file); err != nil {
		t.Fatal(err)
	}
	var stop []map[string]any
	if err := json.Unmarshal(file["entire"]["Stop"], &stop); err != nil {
		t.Fatal(err)
	}
	if got := stop[0]["timeout"]; got != float64(stopHookTimeoutSeconds) {
		t.Errorf("Stop timeout = %v, want %d", got, stopHookTimeoutSeconds)
	}
	if stop[0]["futureHandlerKey"] != float64(1) || string(file["entire"]["futureKey"]) != "true" {
		t.Errorf("unmodeled keys in the entire entry were dropped: %s", data)
	}
	if _, ok := file["mine"]["Stop"]; !ok {
		t.Errorf("foreign entry was dropped: %s", data)
	}
	var mine []map[string]any
	if err := json.Unmarshal(file["mine"]["Stop"], &mine); err != nil {
		t.Fatal(err)
	}
	if _, ok := mine[0]["timeout"]; ok {
		t.Errorf("foreign Stop handler gained a timeout: %s", data)
	}
}

func TestRepairHookTimeouts_LeavesFileAloneWhenNothingToAdd(t *testing.T) {
	stopCmd := buildEntireHookConfig().Stop[0].Command
	for _, tc := range []struct {
		name    string
		content string
	}{
		{name: "no_file"},
		{name: "no_entire_entry", content: `{"mine": {"Stop": [{"type": "command", "command": "my-hook"}]}}`},
		{name: "own_timeout_kept", content: fmt.Sprintf(`{"entire": {"Stop": [{"type": "command", "command": %q, "timeout": 45}]}}`, stopCmd)},
		{name: "unparseable_entry", content: `{"entire": {"Stop": "not a handler list"}}`},
		// Entire's handler decoder matches keys case-insensitively, so this is
		// already a timeout.
		{name: "own_timeout_other_case_kept", content: fmt.Sprintf(`{"entire": {"Stop": [{"type": "command", "command": %q, "Timeout": 600}]}}`, stopCmd)},
		{name: "null_handler_only", content: `{"entire": {"Stop": [null]}}`},
		{name: "user_disabled_entry", content: fmt.Sprintf(`{"entire": {"enabled": false, "Stop": [{"type": "command", "command": %q}]}}`, stopCmd)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tmpDir := t.TempDir()
			t.Chdir(tmpDir)
			path := filepath.Join(tmpDir, ".agents", "hooks.json")
			if tc.content != "" {
				writeHooksJSON(t, tmpDir, tc.content)
			}

			changed, err := (&AntigravityAgent{}).RepairHookTimeouts(context.Background())
			if err != nil {
				t.Fatalf("RepairHookTimeouts: %v", err)
			}
			if changed {
				t.Error("changed = true, want false")
			}
			data, err := os.ReadFile(path)
			if tc.content == "" {
				if !os.IsNotExist(err) {
					t.Errorf("repair created hooks.json: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if string(data) != tc.content {
				t.Errorf("hooks.json rewritten:\n%s", data)
			}
		})
	}
}

// repairEntireStop runs the repair over an "entire" entry with the given Stop
// list, expects it to rewrite the file, and returns the rewritten Stop list.
func repairEntireStop(t *testing.T, stopJSON string) []map[string]any {
	t.Helper()
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)
	path := writeHooksJSON(t, tmpDir, `{"entire": {"Stop": `+stopJSON+`}}`)

	changed, err := (&AntigravityAgent{}).RepairHookTimeouts(context.Background())
	if err != nil {
		t.Fatalf("RepairHookTimeouts: %v", err)
	}
	if !changed {
		t.Fatal("changed = false, want true")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var file map[string]map[string]json.RawMessage
	if err := json.Unmarshal(data, &file); err != nil {
		t.Fatal(err)
	}
	var stop []map[string]any
	if err := json.Unmarshal(file["entire"]["Stop"], &stop); err != nil {
		t.Fatal(err)
	}
	return stop
}

// The repair runs without asking, so it may only touch the handler Entire
// wrote; any other handler in the Stop list is the user's.
func TestRepairHookTimeouts_OnlyRepairsEntireHandler(t *testing.T) {
	stopCmd := buildEntireHookConfig().Stop[0].Command
	stop := repairEntireStop(t, fmt.Sprintf(`[
  {"type": "command", "command": "my-hook"},
  {"type": "command"},
  {"type": "command", "command": %q}
]`, stopCmd))

	if len(stop) != 3 {
		t.Fatalf("Stop has %d handlers, want 3: %v", len(stop), stop)
	}
	for _, i := range []int{0, 1} {
		if _, ok := stop[i]["timeout"]; ok {
			t.Errorf("non-Entire Stop handler %d gained a timeout: %v", i, stop[i])
		}
	}
	if got := stop[2]["timeout"]; got != float64(stopHookTimeoutSeconds) {
		t.Errorf("Entire Stop timeout = %v, want %d", got, stopHookTimeoutSeconds)
	}
}

func TestRepairHookTimeouts_PreservesNullHandler(t *testing.T) {
	stopCmd := buildEntireHookConfig().Stop[0].Command
	stop := repairEntireStop(t, fmt.Sprintf(`[null, {"type": "command", "command": %q}]`, stopCmd))

	if len(stop) != 2 || stop[0] != nil {
		t.Fatalf("null handler was not preserved: %v", stop)
	}
	if got := stop[1]["timeout"]; got != float64(stopHookTimeoutSeconds) {
		t.Errorf("Entire Stop timeout = %v, want %d", got, stopHookTimeoutSeconds)
	}
}

// `entire status` learns about an old install only through CheckHookConfig, and
// points the user to `entire doctor`, so it must report outdated exactly when
// doctor has something to fix: a Stop timeout it adds, or a missing
// SessionStart hook it tells the user to reinstall for.
func TestCheckHookConfig_ReportsMissingStopTimeout(t *testing.T) {
	stopCmd := buildEntireHookConfig().Stop[0].Command
	sessionStart := fmt.Sprintf(`"SessionStart": [{"type": "command", "command": %q}], `, buildEntireHookConfig().SessionStart[0].Command)
	current, err := json.Marshal(map[string]HookConfig{"entire": buildEntireHookConfig()})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name    string
		content string
		want    agent.HookConfigState
	}{
		{name: "no_file", want: agent.HooksAbsent},
		{name: "no_entire_entry", content: `{"mine": {"Stop": [{"type": "command", "command": "my-hook"}]}}`, want: agent.HooksAbsent},
		{name: "current_install", content: string(current), want: agent.HooksCurrent},
		{name: "missing_stop_timeout", content: fmt.Sprintf(`{"entire": {%s"Stop": [{"type": "command", "command": %q}]}}`, sessionStart, stopCmd), want: agent.HooksOutdated},
		{name: "own_timeout_kept", content: fmt.Sprintf(`{"entire": {%s"Stop": [{"type": "command", "command": %q, "timeout": 45}]}}`, sessionStart, stopCmd), want: agent.HooksCurrent},
		{name: "own_timeout_other_case_kept", content: fmt.Sprintf(`{"entire": {%s"Stop": [{"type": "command", "command": %q, "Timeout": 600}]}}`, sessionStart, stopCmd), want: agent.HooksCurrent},
		// Doctor leaves the user's handlers and a disabled entry alone, so
		// pointing the user there for them would never clear the warning.
		{name: "foreign_handler_without_timeout", content: fmt.Sprintf(`{"entire": {%s"Stop": [{"type": "command", "command": "my-hook"}, {"type": "command", "command": %q, "timeout": 300}]}}`, sessionStart, stopCmd), want: agent.HooksCurrent},
		// Installs from before SessionStart was added: a reinstall fixes them.
		{name: "missing_session_start", content: fmt.Sprintf(`{"entire": {"Stop": [{"type": "command", "command": %q, "timeout": 300}]}}`, stopCmd), want: agent.HooksOutdated},
		{name: "user_disabled_entry", content: fmt.Sprintf(`{"entire": {"enabled": false, "Stop": [{"type": "command", "command": %q}]}}`, stopCmd), want: agent.HooksCurrent},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tmpDir := t.TempDir()
			t.Chdir(tmpDir)
			if tc.content != "" {
				writeHooksJSON(t, tmpDir, tc.content)
			}

			if got := (&AntigravityAgent{}).CheckHookConfig(context.Background()); got != tc.want {
				t.Errorf("CheckHookConfig = %v, want %v", got, tc.want)
			}
		})
	}
}

// An install from before SessionStart is reported outdated, and the reinstall
// doctor points to (`entire agent add antigravity`) brings it current without
// touching the user's own entries.
func TestCheckHookConfig_ReinstallAddsSessionStart(t *testing.T) {
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)
	cfg := buildEntireHookConfig()
	cfg.SessionStart = nil
	old, err := json.Marshal(map[string]any{"entire": cfg, "mine": map[string]any{"Stop": []any{map[string]any{"type": "command", "command": "my-hook"}}}})
	if err != nil {
		t.Fatal(err)
	}
	path := writeHooksJSON(t, tmpDir, string(old))
	ag := &AntigravityAgent{}
	if got := ag.CheckHookConfig(context.Background()); got != agent.HooksOutdated {
		t.Fatalf("before reinstall: CheckHookConfig = %v, want outdated", got)
	}
	if _, err := ag.InstallHooks(context.Background(), false); err != nil {
		t.Fatalf("InstallHooks: %v", err)
	}
	if got := ag.CheckHookConfig(context.Background()); got != agent.HooksCurrent {
		t.Fatalf("after reinstall: CheckHookConfig = %v, want current", got)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var file map[string]json.RawMessage
	if err := json.Unmarshal(data, &file); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(file["mine"]), "my-hook") {
		t.Errorf("the user's own entry was lost: %s", data)
	}
}
