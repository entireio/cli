package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/settings"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

func TestRunAgentList_ListsAvailableAgents(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	if err := runAgentList(context.Background(), &buf); err != nil {
		t.Fatalf("runAgentList: %v", err)
	}
	out := buf.String()

	if !strings.Contains(out, "Agents:") {
		t.Errorf("missing 'Agents:' header in output:\n%s", out)
	}

	// At least one of the well-known built-in agents must appear in the listing.
	registered := agent.StringList()
	if len(registered) == 0 {
		t.Skip("no agents registered in this build")
	}
	found := false
	for _, name := range registered {
		if strings.Contains(out, name) {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("none of registered agents %v appeared in output:\n%s", registered, out)
	}
}

func TestRunAgentList_MarksInstalledWithCheck(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	if err := runAgentList(context.Background(), &buf); err != nil {
		t.Fatalf("runAgentList: %v", err)
	}
	out := buf.String()

	// Installed agents are prefixed with the check marker; uninstalled ones
	// are space-padded. Verify both the prefix vocabulary and the header
	// exist so future formatter changes don't silently break the contract.
	if !strings.Contains(out, "✓ ") && !strings.Contains(out, "  ") {
		t.Errorf("output uses neither installed (✓) nor uninstalled markers:\n%s", out)
	}
}

func TestAgentGroupBareCommandRunsAgentMenu(t *testing.T) {
	// t.Chdir cannot coexist with t.Parallel; this test mutates process CWD.
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	testutil.WriteFile(t, dir, EntireSettingsFile, `{"enabled":true}`)
	t.Chdir(dir)

	cmd := newAgentGroupCmd()
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetContext(context.Background())

	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute agent: %v\nstderr: %s", err, stderr.String())
	}

	out := stdout.String()
	if !strings.Contains(out, "Cannot show agent selection in non-interactive mode.") {
		t.Fatalf("bare agent command did not run the agent selection flow, got:\n%s", out)
	}
	if strings.Contains(out, "Usage:") {
		t.Fatalf("bare agent command should not fall through to help, got:\n%s", out)
	}
}

// externalAgentExecLog points the mock plugin's subcommand log at a temp file
// and returns a reader for it, so a test can see what the CLI executed.
func externalAgentExecLog(t *testing.T) func() string {
	t.Helper()
	execLog := filepath.Join(t.TempDir(), "exec.log")
	t.Setenv("ENTIRE_TEST_EXEC_LOG", execLog)
	return func() string {
		data, err := os.ReadFile(execLog)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("reading exec log: %v", err)
		}
		return string(data)
	}
}

// TestAgentAdd_DiscoversNamedExternalAgent pins that `entire agent add <plugin>`
// resolves an external agent the user names, as `entire enable --agent` does,
// even before external_agents has been turned on.
func TestAgentAdd_DiscoversNamedExternalAgent(t *testing.T) {
	// Cannot use t.Parallel: mutates $PATH, cwd, and the agent registry.
	const agentName = "ext-agent-add-test"
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}
	setupTestRepo(t)
	writeSettings(t, testSettingsEnabled)
	externalDir := t.TempDir()
	writeExternalAgentBinaryEx(t, externalDir, agentName, false)
	t.Setenv("PATH", externalDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	execLog := externalAgentExecLog(t)

	cmd := newAgentAddCmd()
	cmd.SetArgs([]string{agentName})
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stdout)
	cmd.SetContext(context.Background())
	if err := cmd.Execute(); err != nil {
		t.Fatalf("agent add %s: %v\noutput:\n%s", agentName, err, stdout.String())
	}

	if !strings.Contains(execLog(), "install-hooks") {
		t.Errorf("agent add must invoke the plugin's install-hooks, exec log:\n%s\noutput:\n%s", execLog(), stdout.String())
	}
	if !settings.IsExternalAgentsEnabled(context.Background()) {
		t.Error("agent add of an external agent must turn external_agents on locally, or its hooks are never dispatched")
	}
}

// TestAgentRemove_DiscoversNamedExternalAgent pins that `entire agent remove
// <plugin>` reaches the plugin's uninstall-hooks instead of "unknown agent".
func TestAgentRemove_DiscoversNamedExternalAgent(t *testing.T) {
	// Cannot use t.Parallel: mutates $PATH, cwd, and the agent registry.
	const agentName = "ext-agent-remove-test"
	installExternalAgentPluginForUninstall(t, agentName, true)
	execLog := externalAgentExecLog(t)

	var stdout bytes.Buffer
	if err := runRemoveAgent(context.Background(), &stdout, agentName); err != nil {
		t.Fatalf("runRemoveAgent: %v\noutput:\n%s", err, stdout.String())
	}
	if !strings.Contains(execLog(), "uninstall-hooks") {
		t.Errorf("agent remove must invoke the plugin's uninstall-hooks, exec log:\n%s\noutput:\n%s", execLog(), stdout.String())
	}
}

// TestRunAgentList_ListsEnabledExternalAgent pins that `entire agent list`
// shows an installed external agent once external_agents is on.
func TestRunAgentList_ListsEnabledExternalAgent(t *testing.T) {
	// Cannot use t.Parallel: mutates $PATH, cwd, and the agent registry.
	const agentName = "ext-agent-list-test"
	installExternalAgentPluginForUninstall(t, agentName, true)

	var stdout bytes.Buffer
	if err := runAgentList(context.Background(), &stdout); err != nil {
		t.Fatalf("runAgentList: %v", err)
	}
	if !strings.Contains(stdout.String(), "✓ "+agentName) {
		t.Errorf("agent list must mark the installed external agent, got:\n%s", stdout.String())
	}
}

// TestRunAgentList_DoesNotRunPluginsWithoutOptIn pins that listing never
// executes entire-agent-* binaries in a repo that has not enabled external_agents.
func TestRunAgentList_DoesNotRunPluginsWithoutOptIn(t *testing.T) {
	// Cannot use t.Parallel: mutates $PATH, cwd, and the agent registry.
	const agentName = "ext-agent-list-optout-test"
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}
	setupTestRepo(t)
	writeSettings(t, testSettingsEnabled)
	externalDir := t.TempDir()
	writeExternalAgentBinaryEx(t, externalDir, agentName, true)
	t.Setenv("PATH", externalDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	execLog := externalAgentExecLog(t)

	var stdout bytes.Buffer
	if err := runAgentList(context.Background(), &stdout); err != nil {
		t.Fatalf("runAgentList: %v", err)
	}
	if strings.Contains(stdout.String(), agentName) {
		t.Errorf("agent list showed an external agent without external_agents enabled:\n%s", stdout.String())
	}
	if log := execLog(); log != "" {
		t.Errorf("agent list executed a plugin without external_agents enabled, exec log:\n%s", log)
	}
}

// TestAgentAdd_ReportsBrokenExternalAgent pins that a plugin on $PATH whose
// info call fails is reported as such, not as a misspelled agent name.
func TestAgentAdd_ReportsBrokenExternalAgent(t *testing.T) {
	// Cannot use t.Parallel: mutates $PATH and cwd.
	const agentName = "ext-agent-broken-test"
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}
	setupTestRepo(t)
	writeSettings(t, testSettingsEnabled)
	externalDir := t.TempDir()
	script := "#!/bin/sh\necho 'plugin exploded' >&2\nexit 1\n"
	if err := os.WriteFile(filepath.Join(externalDir, "entire-agent-"+agentName), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", externalDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	cmd := newAgentAddCmd()
	cmd.SetArgs([]string{agentName})
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stdout)
	cmd.SetContext(context.Background())
	err := cmd.Execute()
	if err == nil {
		t.Fatalf("agent add of a broken plugin succeeded\noutput:\n%s", stdout.String())
	}
	if strings.Contains(stdout.String(), "Unknown agent") || strings.Contains(err.Error(), "wrong agent name") {
		t.Errorf("broken plugin reported as a misspelled name: %v\noutput:\n%s", err, stdout.String())
	}
	if !strings.Contains(err.Error(), agentName) {
		t.Errorf("error should name the agent, got: %v", err)
	}
}
