package agents

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestAntigravityProbeCommandRunsUnderSh pins that the generated probe command
// runs under sh (as agy runs hook commands) and records header, cwd and stdin.
func TestAntigravityProbeCommandRunsUnderSh(t *testing.T) {
	repoDir := t.TempDir()
	hooksPath := filepath.Join(repoDir, ".agents", "hooks.json")
	if err := os.MkdirAll(filepath.Dir(hooksPath), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(hooksPath, []byte(`{"entire":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := antigravityPrepareRepo([]string{"CI=true"}, repoDir); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(hooksPath)
	var hooks map[string]struct{ PreInvocation []struct{ Command string } }
	if err := json.Unmarshal(data, &hooks); err != nil {
		t.Fatal(err)
	}
	cmdStr := hooks["entire-e2e-probe"].PreInvocation[0].Command
	// agy executes the command string via a shell; emulate that.
	cmd := exec.Command("sh", "-c", cmdStr)
	cmd.Dir = filepath.Join(repoDir, ".agents")
	cmd.Stdin = strings.NewReader(`{"conversationId":"c1","workspacePaths":["` + repoDir + `"]}`)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("probe cmd failed: %v\n%s\ncmd=%s", err, out, cmdStr)
	}
	logData, err := os.ReadFile(filepath.Join(repoDir, antigravityHookProbeLog))
	if err != nil {
		t.Fatalf("probe log missing: %v", err)
	}
	s := string(logData)
	wantCwd, _ := filepath.EvalSymlinks(filepath.Join(repoDir, ".agents"))
	if !strings.Contains(s, "--- ") || !strings.Contains(s, "cwd="+wantCwd) || !strings.Contains(s, `"conversationId":"c1"`) {
		t.Fatalf("probe log content unexpected:\n%s\ncmd=%s", s, cmdStr)
	}
}
