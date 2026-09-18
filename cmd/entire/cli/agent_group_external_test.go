package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// externalPluginFixture is a repo whose $PATH carries a mock external agent
// plugin that has already installed its hook config, matching the state
// `entire enable --agent <plugin>` leaves behind: hooks written, and the
// external_agents grant persisted into the untracked local settings file.
//
// hookFile is the plugin's own hook configuration. It exists on return and is
// the artifact a removal has to delete — asserting on it is what separates a
// real uninstall from one that merely invoked a subcommand.
type externalPluginFixture struct {
	name     string
	hookFile string
}

// installedExternalPlugin builds that state. grant controls whether
// external_agents is enabled, so a test can cover removal of a plugin whose
// grant is gone — the state that makes a plugin unremovable while its hooks
// keep firing.
//
// setupTestRepo scrubs $PATH down to git and sh, so a real entire-agent-*
// binary on the developer's machine cannot leak into the assertions.
func installedExternalPlugin(t *testing.T, name string, grant bool) externalPluginFixture {
	t.Helper()

	// The mock is a #!/bin/sh script.
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}

	setupTestRepo(t)
	writeSettings(t, testSettingsEnabled)
	if grant {
		// external_agents is honored only from the local file — it grants
		// execution of entire-agent-* binaries found on $PATH.
		writeLocalSettings(t, `{"external_agents":true}`)
	}

	externalDir := t.TempDir()
	writeExternalAgentBinaryEx(t, externalDir, name, false)
	t.Setenv("PATH", externalDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	hookFile := filepath.Join(t.TempDir(), "plugin-hooks.json")
	t.Setenv("ENTIRE_TEST_EXTERNAL_HOOK_FILE", hookFile)
	if err := os.WriteFile(hookFile, []byte(`{"hooks":{"stop":"entire hooks stop"}}`), 0o600); err != nil {
		t.Fatalf("seeding plugin hook config: %v", err)
	}

	return externalPluginFixture{name: name, hookFile: hookFile}
}

// TestRunAgentList_ShowsInstalledExternalAgent pins that an external agent
// plugin whose hooks are installed is listed. `entire agent list` reaches
// agents only through the registry, and nothing on this path used to populate
// it with external plugins, so an installed plugin was reported as not
// existing — under a "No agents installed" line printed over live hooks.
func TestRunAgentList_ShowsInstalledExternalAgent(t *testing.T) {
	// Cannot use t.Parallel: mutates $PATH, cwd, and the agent registry.
	fixture := installedExternalPlugin(t, "ext-list-test", true)

	var buf bytes.Buffer
	if err := runAgentList(context.Background(), &buf); err != nil {
		t.Fatalf("runAgentList: %v", err)
	}
	out := buf.String()

	if !strings.Contains(out, fixture.name) {
		t.Fatalf("installed external agent %q missing from listing:\n%s", fixture.name, out)
	}
	if !strings.Contains(out, "✓ "+fixture.name) {
		t.Errorf("external agent %q listed but not marked installed:\n%s", fixture.name, out)
	}
	if strings.Contains(out, "No agents installed") {
		t.Errorf("listing claims no agents are installed while %q has hooks installed:\n%s", fixture.name, out)
	}
}

// TestRunStatusJSON_ReportsInstalledExternalAgent pins the same for the
// machine-readable status surface, which other tools consume.
func TestRunStatusJSON_ReportsInstalledExternalAgent(t *testing.T) {
	// Cannot use t.Parallel: mutates $PATH, cwd, and the agent registry.
	fixture := installedExternalPlugin(t, "ext-status-test", true)

	var buf bytes.Buffer
	if err := runStatusJSON(context.Background(), &buf); err != nil {
		t.Fatalf("runStatusJSON: %v", err)
	}

	var got statusJSON
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("decoding status JSON %q: %v", buf.String(), err)
	}
	// The mock's info reports its type as "<name> Agent", and status lists
	// display names.
	want := fixture.name + " Agent"
	if !slices.Contains(got.Agents, want) {
		t.Fatalf("status agents %v missing installed external agent %q", got.Agents, want)
	}
}

// TestRunRemoveAgent_RemovesExternalAgentHooks is the sharpest half: a plugin
// that `entire enable` installed must be removable through the CLI. Before the
// fix the command rejected the plugin's own name as an unknown agent and left
// its hook config in place, with `rm -rf` as the only remedy.
func TestRunRemoveAgent_RemovesExternalAgentHooks(t *testing.T) {
	// Cannot use t.Parallel: mutates $PATH, cwd, and the agent registry.
	fixture := installedExternalPlugin(t, "ext-remove-test", true)

	var buf bytes.Buffer
	if err := runRemoveAgent(context.Background(), &buf, fixture.name); err != nil {
		t.Fatalf("runRemoveAgent(%q) error = %v\noutput: %s", fixture.name, err, buf.String())
	}
	if !strings.Contains(buf.String(), "Removed") {
		t.Errorf("remove did not report a removal:\n%s", buf.String())
	}
	if _, err := os.Stat(fixture.hookFile); !os.IsNotExist(err) {
		t.Fatalf("plugin hook config %s survived the removal (stat err = %v)", fixture.hookFile, err)
	}
}

// TestRunRemoveAgent_RemovesExternalAgentWithoutGrant covers removal when the
// external_agents grant is absent — a plugin installed earlier, or a local
// settings file since lost. The grant gates the $PATH sweep; it must not gate
// uninstalling something already installed, or the hooks become permanent.
func TestRunRemoveAgent_RemovesExternalAgentWithoutGrant(t *testing.T) {
	// Cannot use t.Parallel: mutates $PATH, cwd, and the agent registry.
	fixture := installedExternalPlugin(t, "ext-remove-nogrant-test", false)

	var buf bytes.Buffer
	if err := runRemoveAgent(context.Background(), &buf, fixture.name); err != nil {
		t.Fatalf("runRemoveAgent(%q) error = %v\noutput: %s", fixture.name, err, buf.String())
	}
	if _, err := os.Stat(fixture.hookFile); !os.IsNotExist(err) {
		t.Fatalf("plugin hook config %s survived the removal (stat err = %v)", fixture.hookFile, err)
	}
}

// TestRunRemoveAgent_UnknownNameStillRejected pins that making external agents
// resolvable did not turn a typo into a success.
func TestRunRemoveAgent_UnknownNameStillRejected(t *testing.T) {
	// Cannot use t.Parallel: mutates $PATH and cwd.
	setupTestRepo(t)
	writeSettings(t, testSettingsEnabled)

	var buf bytes.Buffer
	if err := runRemoveAgent(context.Background(), &buf, "no-such-agent"); err == nil {
		t.Fatalf("expected an unknown agent name to fail, output:\n%s", buf.String())
	}
}
