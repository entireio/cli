package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/claudecode"
	"github.com/entireio/cli/cmd/entire/cli/agent/geminicli"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/settings"
)

// isolatedUserHome points HOME (and USERPROFILE) at a temp dir so the agents'
// user-level settings files live there; returns the dir.
func isolatedUserHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	return home
}

// pretendAgentBinaries makes userHookAgentPresent see the named binaries on
// PATH without touching the real PATH.
func pretendAgentBinaries(t *testing.T, names ...string) {
	t.Helper()
	previous := userHookLookPath
	userHookLookPath = func(file string) (string, error) {
		for _, n := range names {
			if n == file {
				return "/fake/bin/" + file, nil
			}
		}
		return "", os.ErrNotExist
	}
	t.Cleanup(func() { userHookLookPath = previous })
}

func writeUserSettings(t *testing.T, body string) {
	t.Helper()
	cfg := t.TempDir()
	t.Setenv("ENTIRE_CONFIG_DIR", cfg)
	if body != "" {
		if err := os.WriteFile(filepath.Join(cfg, "settings.json"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func userHooksInstalled(t *testing.T, name string) bool {
	t.Helper()
	ag, err := agent.Get(types.AgentName(name))
	if err != nil {
		t.Fatal(err)
	}
	support, ok := agent.AsUserHookSupport(ag)
	if !ok {
		t.Fatalf("%s does not support user hooks", name)
	}
	installed, err := support.AreUserHooksInstalled(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return installed
}

// TestReconcileUserHooks: user-level hooks follow global.enabled — installed
// for agents present on the machine while the tier is on, removed when the
// tier is configured but off, untouched while the tier is unconfigured.
func TestReconcileUserHooks(t *testing.T) {
	home := isolatedUserHome(t)
	pretendAgentBinaries(t, "claude", "gemini")
	ctx := context.Background()

	// Unconfigured tier: nothing happens, even with agents present.
	writeUserSettings(t, "")
	var out bytes.Buffer
	globalPostRun(ctx, &out)
	if out.Len() != 0 {
		t.Fatalf("unconfigured tier must be silent, got:\n%s", out.String())
	}
	if _, err := os.Stat(filepath.Join(home, ".claude")); !os.IsNotExist(err) {
		t.Fatalf("unconfigured tier must not create agent config (err=%v)", err)
	}

	// Enabled: both supported agents get their inventories.
	writeUserSettings(t, `{"global":{"enabled":true}}`)
	out.Reset()
	globalPostRun(ctx, &out)
	for _, name := range []string{string(agent.AgentNameClaudeCode), string(agent.AgentNameGemini)} {
		if !userHooksInstalled(t, name) {
			t.Errorf("%s user hooks not installed with global tracking on; output:\n%s", name, out.String())
		}
		if !strings.Contains(out.String(), "installed user-level "+name+" hooks") {
			t.Errorf("no install notice for %s in:\n%s", name, out.String())
		}
	}
	claudePath, err := claudecode.UserSettingsPath()
	if err != nil {
		t.Fatal(err)
	}
	geminiPath, err := geminicli.UserSettingsPath()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(claudePath, home) || !strings.HasPrefix(geminiPath, home) {
		t.Fatalf("agent settings escaped the isolated HOME: %s, %s", claudePath, geminiPath)
	}

	// Second run is a no-op.
	out.Reset()
	globalPostRun(ctx, &out)
	if strings.Contains(out.String(), "installed user-level") {
		t.Errorf("already-installed hooks must not be reported again:\n%s", out.String())
	}

	// Configured but disabled: hooks are removed.
	writeUserSettings(t, `{"global":{"enabled":false}}`)
	out.Reset()
	globalPostRun(ctx, &out)
	for _, name := range []string{string(agent.AgentNameClaudeCode), string(agent.AgentNameGemini)} {
		if userHooksInstalled(t, name) {
			t.Errorf("%s user hooks still installed with global tracking off; output:\n%s", name, out.String())
		}
		if !strings.Contains(out.String(), "removed user-level "+name+" hooks") {
			t.Errorf("no removal notice for %s in:\n%s", name, out.String())
		}
	}
}

// TestReconcileUserHooks_SkipsAgentsNotOnThisMachine: no binary and no prior
// Entire entry means no config file is created for that agent.
func TestReconcileUserHooks_SkipsAgentsNotOnThisMachine(t *testing.T) {
	home := isolatedUserHome(t)
	pretendAgentBinaries(t, "claude") // gemini absent
	writeUserSettings(t, `{"global":{"enabled":true}}`)

	var out bytes.Buffer
	globalPostRun(context.Background(), &out)

	if !userHooksInstalled(t, string(agent.AgentNameClaudeCode)) {
		t.Error("claude hooks should be installed")
	}
	if _, err := os.Stat(filepath.Join(home, ".gemini")); !os.IsNotExist(err) {
		t.Errorf("gemini config must not be created when gemini is not installed (err=%v)", err)
	}
}

func TestGlobalPostRun_UnreadableSettingsIsSilent(t *testing.T) {
	isolatedUserHome(t)
	pretendAgentBinaries(t, "claude")
	writeUserSettings(t, `{"global":{"enabled":tru`)

	var out bytes.Buffer
	globalPostRun(context.Background(), &out)
	if out.Len() != 0 {
		t.Fatalf("unreadable user settings must be silent here (doctor reports it), got:\n%s", out.String())
	}
	if _, err := settings.LoadUserSettings(context.Background()); err == nil {
		t.Fatal("fixture should be unreadable")
	}
}
