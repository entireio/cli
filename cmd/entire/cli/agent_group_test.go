package cli

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

func TestRunAgentList_ListsAvailableAgents(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	if err := runAgentList(context.Background(), &buf, false); err != nil {
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
	if err := runAgentList(context.Background(), &buf, false); err != nil {
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

// TestRunAgentList_ExternalFlagListsAvailablePlugin verifies that an
// available-but-uninstalled external plugin on $PATH is listed under
// `--external`, and that the default (built-in) listing does not surface it.
//
// The default-listing absence check here is filter-driven (agent.List is
// filtered by external.IsExternal), so it does NOT by itself prove the default
// path skips the $PATH scan — TestRunAgentList_DefaultPathDoesNotScanPath owns
// that guarantee via an exec-count assertion.
func TestRunAgentList_ExternalFlagListsAvailablePlugin(t *testing.T) {
	// Cannot use t.Parallel because we modify PATH via t.Setenv and cwd via t.Chdir.
	// The mock agent is a #!/bin/sh script run by the install-state probe, so skip
	// on environments without a POSIX shell (matching sibling external-agent tests).
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}

	// --external registers the mock into the process-global registry; restore it
	// so the temp-binary-backed agent doesn't leak into later package tests.
	t.Cleanup(agent.SnapshotForTesting())

	repoDir := t.TempDir()
	testutil.InitRepo(t, repoDir)
	testutil.WriteFile(t, repoDir, ".entire/settings.json", `{"enabled":true,"external_agents":true}`)
	t.Chdir(repoDir)

	// Unique name so parallel package tests can't collide on $PATH.
	const agentName = "ext-available-test"
	externalDir := t.TempDir()
	writeExternalAgentBinary(t, externalDir, agentName) // are-hooks-installed => false
	t.Setenv("PATH", externalDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	// Default listing is built-ins only — never scans $PATH, so no external.
	var def bytes.Buffer
	if err := runAgentList(context.Background(), &def, false); err != nil {
		t.Fatalf("runAgentList (default): %v", err)
	}
	if strings.Contains(def.String(), agentName) {
		t.Errorf("default listing must not include external agent %q, got:\n%s", agentName, def.String())
	}

	// --external surfaces available external plugins on $PATH.
	var ext bytes.Buffer
	if err := runAgentList(context.Background(), &ext, true); err != nil {
		t.Fatalf("runAgentList (--external): %v", err)
	}
	if !strings.Contains(ext.String(), agentName) {
		t.Errorf("--external listing should include available external agent %q, got:\n%s", agentName, ext.String())
	}
}

// TestRunAgentList_DefaultPathDoesNotScanPath proves the guarantee that the
// filter-driven absence check in TestRunAgentList_ExternalFlagListsAvailablePlugin
// cannot: the default `agent list` path never executes any external plugin on
// $PATH, while `--external` does. It asserts on an exec log the mock plugin
// appends to, so reintroducing an unconditional $PATH scan on the default path
// would fail here even though the plugin is filtered out of the output.
func TestRunAgentList_DefaultPathDoesNotScanPath(t *testing.T) {
	// Cannot use t.Parallel: mutates PATH via t.Setenv and cwd via t.Chdir.
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}
	t.Cleanup(agent.SnapshotForTesting())

	repoDir := t.TempDir()
	testutil.InitRepo(t, repoDir)
	testutil.WriteFile(t, repoDir, ".entire/settings.json", `{"enabled":true,"external_agents":true}`)
	t.Chdir(repoDir)

	const agentName = "ext-execlog-test"
	externalDir := t.TempDir()
	writeExternalAgentBinary(t, externalDir, agentName)
	t.Setenv("PATH", externalDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	// The mock appends each invoked subcommand to this file when the env var is set.
	execLog := filepath.Join(t.TempDir(), "exec.log")
	t.Setenv("ENTIRE_TEST_EXEC_LOG", execLog)

	// Default listing must never exec the plugin: the exec log stays empty.
	var def bytes.Buffer
	if err := runAgentList(context.Background(), &def, false); err != nil {
		t.Fatalf("runAgentList (default): %v", err)
	}
	if data, err := os.ReadFile(execLog); err == nil && len(data) > 0 {
		t.Errorf("default listing executed external plugin (exec log non-empty):\n%s", data)
	} else if err != nil && !os.IsNotExist(err) {
		t.Fatalf("reading exec log after default call: %v", err)
	}

	// --external must scan $PATH, executing the plugin at least once.
	var ext bytes.Buffer
	if err := runAgentList(context.Background(), &ext, true); err != nil {
		t.Fatalf("runAgentList (--external): %v", err)
	}
	data, err := os.ReadFile(execLog)
	if err != nil {
		t.Fatalf("reading exec log after --external call: %v", err)
	}
	if len(data) == 0 {
		t.Errorf("--external listing did not execute external plugin (exec log empty)")
	}
}

// TestRunAgentList_ExternalFlagMarksInstalled verifies that an INSTALLED external
// plugin is listed and marked installed (✓) under `--external`.
func TestRunAgentList_ExternalFlagMarksInstalled(t *testing.T) {
	// Cannot use t.Parallel because we modify PATH via t.Setenv and cwd via t.Chdir.
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}

	// --external registers the mock into the process-global registry; restore it
	// so the installed-reporting mock doesn't leak into later package tests.
	t.Cleanup(agent.SnapshotForTesting())

	repoDir := t.TempDir()
	testutil.InitRepo(t, repoDir)
	testutil.WriteFile(t, repoDir, ".entire/settings.json", `{"enabled":true,"external_agents":true}`)
	t.Chdir(repoDir)

	// Unique name; the mock reports its hooks ARE installed.
	const agentName = "ext-installed-test"
	externalDir := t.TempDir()
	writeExternalAgentBinaryEx(t, externalDir, agentName, true) // are-hooks-installed => true
	t.Setenv("PATH", externalDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	var ext bytes.Buffer
	if err := runAgentList(context.Background(), &ext, true); err != nil {
		t.Fatalf("runAgentList (--external): %v", err)
	}
	if !strings.Contains(ext.String(), "✓ "+agentName) {
		t.Errorf("installed external agent %q should be marked installed (✓) under --external, got:\n%s", agentName, ext.String())
	}
}

// TestRunAgentList_ExternalIsSuperset verifies that `--external` is a superset:
// its output includes external plugins on $PATH AND the built-in agents, while
// the default listing shows built-ins only.
func TestRunAgentList_ExternalIsSuperset(t *testing.T) {
	// Cannot use t.Parallel: mutates PATH via t.Setenv and cwd via t.Chdir.
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}
	t.Cleanup(agent.SnapshotForTesting())

	// A built-in agent name to look for in both listings.
	builtins := agent.StringList()
	if len(builtins) == 0 {
		t.Skip("no built-in agents registered in this build")
	}
	builtin := builtins[0]

	repoDir := t.TempDir()
	testutil.InitRepo(t, repoDir)
	testutil.WriteFile(t, repoDir, ".entire/settings.json", `{"enabled":true,"external_agents":true}`)
	t.Chdir(repoDir)

	const agentName = "ext-superset-test"
	externalDir := t.TempDir()
	writeExternalAgentBinary(t, externalDir, agentName)
	t.Setenv("PATH", externalDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	var ext bytes.Buffer
	if err := runAgentList(context.Background(), &ext, true); err != nil {
		t.Fatalf("runAgentList (--external): %v", err)
	}
	out := ext.String()
	if !strings.Contains(out, agentName) {
		t.Errorf("--external should include external plugin %q, got:\n%s", agentName, out)
	}
	if !strings.Contains(out, builtin) {
		t.Errorf("--external should ALSO include built-in agent %q (superset), got:\n%s", builtin, out)
	}
}

// TestRunAgentList_MarksExternalProvenance verifies that the superset listing
// distinguishes external plugins from built-ins: external agents are arbitrary
// executables resolved from $PATH, so rendering them identically to shipped
// agents would hide a trust-relevant distinction.
func TestRunAgentList_MarksExternalProvenance(t *testing.T) {
	// Cannot use t.Parallel: mutates PATH via t.Setenv and cwd via t.Chdir.
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}
	t.Cleanup(agent.SnapshotForTesting())

	builtins := agent.StringList()
	if len(builtins) == 0 {
		t.Skip("no built-in agents registered in this build")
	}

	repoDir := t.TempDir()
	testutil.InitRepo(t, repoDir)
	testutil.WriteFile(t, repoDir, ".entire/settings.json", `{"enabled":true,"external_agents":true}`)
	t.Chdir(repoDir)

	const agentName = "ext-provenance-test"
	externalDir := t.TempDir()
	writeExternalAgentBinary(t, externalDir, agentName)
	t.Setenv("PATH", externalDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	var ext bytes.Buffer
	if err := runAgentList(context.Background(), &ext, true); err != nil {
		t.Fatalf("runAgentList (--external): %v", err)
	}

	for _, line := range strings.Split(ext.String(), "\n") {
		switch {
		case strings.Contains(line, agentName):
			if !strings.Contains(line, "(external)") {
				t.Errorf("external plugin line should be marked (external), got: %q", line)
			}
		case strings.Contains(line, builtins[0]):
			if strings.Contains(line, "(external)") {
				t.Errorf("built-in agent line must not be marked (external), got: %q", line)
			}
		}
	}
}

// TestRunAgentList_EmptyStateWording verifies the mode-scoped empty-state: the
// default path says "No built-in agents installed" while `--external`, being the
// complete view, says "No agents installed".
func TestRunAgentList_EmptyStateWording(t *testing.T) {
	// Cannot use t.Parallel: mutates cwd via t.Chdir (no agent hooks installed
	// in the fresh repo, so both paths hit the empty-state).
	repoDir := t.TempDir()
	testutil.InitRepo(t, repoDir)
	testutil.WriteFile(t, repoDir, ".entire/settings.json", `{"enabled":true}`)
	t.Chdir(repoDir)

	var def bytes.Buffer
	if err := runAgentList(context.Background(), &def, false); err != nil {
		t.Fatalf("runAgentList (default): %v", err)
	}
	if !strings.Contains(def.String(), "No built-in agents installed.") {
		t.Errorf("default empty-state should say 'No built-in agents installed.', got:\n%s", def.String())
	}

	var ext bytes.Buffer
	if err := runAgentList(context.Background(), &ext, true); err != nil {
		t.Fatalf("runAgentList (--external): %v", err)
	}
	if !strings.Contains(ext.String(), "No agents installed.") {
		t.Errorf("--external empty-state should say 'No agents installed.', got:\n%s", ext.String())
	}
	if strings.Contains(ext.String(), "No built-in agents installed.") {
		t.Errorf("--external empty-state must not scope to built-ins, got:\n%s", ext.String())
	}
}

// externalAgentRepo prepares a repo with settings, puts a mock external plugin
// named agentName on $PATH, and restores the agent registry afterwards. The
// settings deliberately leave external_agents unset: `agent add`/`remove` are
// themselves the opt-in, so they must work without it.
func externalAgentRepo(t *testing.T, agentName string) {
	t.Helper()

	// The mock is a #!/bin/sh script, and $PATH/cwd are process-global.
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}
	t.Cleanup(agent.SnapshotForTesting())

	setupTestRepo(t)
	writeSettings(t, `{"enabled":true}`)

	externalDir := t.TempDir()
	writeExternalAgentBinary(t, externalDir, agentName)
	t.Setenv("PATH", externalDir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// TestAgentAdd_InstallsExternalPluginFromPath is the core of #1928 for `add`:
// a plugin that is only discoverable by scanning $PATH must install through
// `entire agent add <name>`, which previously failed with "unknown agent"
// because the command never ran discovery.
func TestAgentAdd_InstallsExternalPluginFromPath(t *testing.T) {
	// Cannot use t.Parallel: mutates PATH via t.Setenv, cwd via t.Chdir, and the
	// process-global agent registry.
	const agentName = "ext-add-test"
	externalAgentRepo(t, agentName)

	cmd := newAgentAddCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{agentName})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("entire agent add %s: %v\noutput:\n%s", agentName, err, out.String())
	}

	// Installing an external agent flips external_agents on, or the hooks it
	// just wrote would fire for an agent later runs refuse to discover.
	data, err := os.ReadFile(EntireSettingsFile)
	if err != nil {
		t.Fatalf("reading settings after add: %v", err)
	}
	if !strings.Contains(string(data), `"external_agents": true`) &&
		!strings.Contains(string(data), `"external_agents":true`) {
		t.Errorf("expected add of an external agent to enable external_agents, got settings:\n%s", data)
	}
}

// TestAgentRemove_ResolvesExternalPluginFromPath covers the `remove` half of
// #1928: uninstalling must reach the same $PATH-discovered plugin that `add`
// installed, rather than reporting it unknown.
func TestAgentRemove_ResolvesExternalPluginFromPath(t *testing.T) {
	// Cannot use t.Parallel: see TestAgentAdd_InstallsExternalPluginFromPath.
	const agentName = "ext-remove-test"
	externalAgentRepo(t, agentName)

	add := newAgentAddCmd()
	var addOut bytes.Buffer
	add.SetOut(&addOut)
	add.SetErr(&addOut)
	add.SetArgs([]string{agentName})
	if err := add.Execute(); err != nil {
		t.Fatalf("entire agent add %s: %v\noutput:\n%s", agentName, err, addOut.String())
	}

	remove := newAgentRemoveCmd()
	var out bytes.Buffer
	remove.SetOut(&out)
	remove.SetErr(&out)
	remove.SetArgs([]string{agentName})
	if err := remove.Execute(); err != nil {
		t.Fatalf("entire agent remove %s: %v\noutput:\n%s", agentName, err, out.String())
	}
	if strings.Contains(out.String(), "Unknown agent") {
		t.Errorf("remove must resolve the external plugin, got:\n%s", out.String())
	}
}

// TestAgentAdd_UnknownNameReportsSearchedPath pins the miss path: a name that
// matches neither a built-in nor an `entire-agent-<name>` binary falls through
// to the agent listing, and the hint reports the $PATH lookup that already
// happened instead of telling the user to go run it.
func TestAgentAdd_UnknownNameReportsSearchedPath(t *testing.T) {
	// Cannot use t.Parallel: mutates cwd via t.Chdir.
	setupTestRepo(t)
	writeSettings(t, `{"enabled":true}`)

	cmd := newAgentAddCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"no-such-agent"})
	if err := cmd.Execute(); err == nil {
		t.Fatalf("expected an error for an unknown agent, got output:\n%s", out.String())
	}

	if !strings.Contains(out.String(), `Unknown agent "no-such-agent"`) {
		t.Errorf("expected the unknown-agent listing, got:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "was found on your PATH") {
		t.Errorf("expected the hint to report the $PATH search that already ran, got:\n%s", out.String())
	}
	if !strings.Contains(out.String(), agentAddUsage) {
		t.Errorf("expected the `agent add` usage line, got:\n%s", out.String())
	}
}
