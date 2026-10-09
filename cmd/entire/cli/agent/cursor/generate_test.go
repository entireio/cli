package cursor

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// A text-generation run's prompt carries untrusted transcript content. Cursor
// has no "no tools" flag, so the run gets a fresh workspace whose project
// config denies every permission kind, and no --force.
func TestGenerateText_DeniesEveryPermission(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("uses sh")
	}

	var args []string
	var config []byte
	var readErr error
	ag := &CursorAgent{CommandRunner: func(ctx context.Context, _ string, a ...string) *exec.Cmd {
		args = slices.Clone(a)
		if i := slices.Index(a, "--workspace"); i >= 0 && i+1 < len(a) {
			config, readErr = os.ReadFile(filepath.Join(a[i+1], ".cursor", "cli.json"))
		}
		return exec.CommandContext(ctx, "sh", "-c", "pwd -P")
	}}

	cwd, err := ag.GenerateText(context.Background(), "prompt", "")
	if err != nil {
		t.Fatalf("GenerateText: %v", err)
	}
	if slices.Contains(args, "--force") {
		t.Errorf("--force auto-approves shell commands and must not be passed: %v", args)
	}
	i := slices.Index(args, "--workspace")
	if i < 0 || i+1 >= len(args) {
		t.Fatalf("--workspace missing: %v", args)
	}
	workspace, err := filepath.EvalSymlinks(filepath.Dir(args[i+1]))
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(workspace, filepath.Base(args[i+1])); strings.TrimSpace(cwd) != want {
		t.Errorf("ran in %q, want the workspace %q", cwd, want)
	}

	if readErr != nil {
		t.Fatalf("workspace has no .cursor/cli.json during the run: %v", readErr)
	}
	var cfg struct {
		Permissions struct {
			Allow []string `json:"allow"`
			Deny  []string `json:"deny"`
		} `json:"permissions"`
	}
	if err := json.Unmarshal(config, &cfg); err != nil {
		t.Fatalf("cli.json is not valid JSON: %v\n%s", err, config)
	}
	if len(cfg.Permissions.Allow) != 0 {
		t.Errorf("allow = %v, want none", cfg.Permissions.Allow)
	}
	for _, kind := range []string{"Shell(*)", "Read(*)", "Write(*)", "WebFetch(*)", "Mcp(*:*)"} {
		if !slices.Contains(cfg.Permissions.Deny, kind) {
			t.Errorf("deny is missing %s: %v", kind, cfg.Permissions.Deny)
		}
	}

	if _, err := os.Stat(args[i+1]); !os.IsNotExist(err) {
		t.Errorf("workspace %q still exists after the run (stat err = %v)", args[i+1], err)
	}
}
