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

// Settings bodies for withExternalAgentPlugin. The difference is deliberate:
// the listing tests run under the external_agents gate the way a user who
// opted in would, while `agent add`/`remove` must work with the gate unset —
// naming a plugin on those commands is itself the opt-in.
const (
	externalAgentsEnabledSettings = `{"enabled":true,"external_agents":true}`
	externalAgentsUnsetSettings   = `{"enabled":true}`
)

// withExternalAgentPlugin prepares an isolated repo with the given settings and
// puts a mock external agent plugin named agentName on $PATH. hooksInstalled is
// what the mock's are-hooks-installed subcommand reports, so callers can
// simulate an installed plugin as well as an available one.
//
// setupTestRepo scrubs $PATH down to git and sh, so a real entire-agent-*
// binary on the developer's machine cannot leak into the assertions. Discovery
// registers the mock into the process-global registry, so the registry is
// snapshotted and restored — otherwise a mock backed by a t.TempDir binary
// outlives the test and later ones exec a deleted file.
//
// Callers cannot use t.Parallel: this mutates $PATH, cwd, and the registry.
func withExternalAgentPlugin(t *testing.T, agentName, settings string, hooksInstalled bool) {
	t.Helper()

	// The mock is a #!/bin/sh script.
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}
	t.Cleanup(agent.SnapshotForTesting())

	setupTestRepo(t)
	writeSettings(t, settings)

	externalDir := t.TempDir()
	writeExternalAgentBinaryEx(t, externalDir, agentName, hooksInstalled)
	t.Setenv("PATH", externalDir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestRunAgentList_ListsAvailableAgents(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	if err := runAgentList(context.Background(), &buf, false); err != nil {
		t.Fatalf("runAgentList: %v", err)
	}
	out := buf.String()

	// The default listing omits external plugins, so its header is scoped to
	// what it actually shows — see TestRunAgentList_HeaderScopedToMode.
	if !strings.Contains(out, "Built-in agents:") {
		t.Errorf("missing 'Built-in agents:' header in output:\n%s", out)
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
	// Cannot use t.Parallel: see withExternalAgentPlugin.
	// Unique name so parallel package tests can't collide on $PATH.
	const agentName = "ext-available-test"
	withExternalAgentPlugin(t, agentName, externalAgentsEnabledSettings, false)

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
	// Cannot use t.Parallel: see withExternalAgentPlugin.
	const agentName = "ext-execlog-test"
	withExternalAgentPlugin(t, agentName, externalAgentsEnabledSettings, false)

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
	// Cannot use t.Parallel: see withExternalAgentPlugin.
	// Unique name; the mock reports its hooks ARE installed.
	const agentName = "ext-installed-test"
	withExternalAgentPlugin(t, agentName, externalAgentsEnabledSettings, true)

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
	// Cannot use t.Parallel: see withExternalAgentPlugin.
	// A built-in agent name to look for in both listings.
	builtins := agent.StringList()
	if len(builtins) == 0 {
		t.Skip("no built-in agents registered in this build")
	}
	builtin := builtins[0]

	const agentName = "ext-superset-test"
	withExternalAgentPlugin(t, agentName, externalAgentsEnabledSettings, false)

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
	// Cannot use t.Parallel: see withExternalAgentPlugin.
	builtins := agent.StringList()
	if len(builtins) == 0 {
		t.Skip("no built-in agents registered in this build")
	}

	const agentName = "ext-provenance-test"
	withExternalAgentPlugin(t, agentName, externalAgentsEnabledSettings, false)

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

// TestRunAgentList_HeaderScopedToMode verifies that the header names the set
// being listed. The default view is built-ins only, so printing the `--external`
// header over it would let a partial listing read as the complete one — the way
// a user who just installed an external plugin would read it.
func TestRunAgentList_HeaderScopedToMode(t *testing.T) {
	// Cannot use t.Parallel: setupTestRepo mutates cwd and $PATH.
	setupTestRepo(t)
	writeSettings(t, externalAgentsUnsetSettings)

	var def bytes.Buffer
	if err := runAgentList(context.Background(), &def, false); err != nil {
		t.Fatalf("runAgentList (default): %v", err)
	}
	if !strings.Contains(def.String(), "Built-in agents:") {
		t.Errorf("default header should scope itself to built-ins, got:\n%s", def.String())
	}

	var ext bytes.Buffer
	if err := runAgentList(context.Background(), &ext, true); err != nil {
		t.Fatalf("runAgentList (--external): %v", err)
	}
	if strings.Contains(ext.String(), "Built-in agents:") {
		t.Errorf("--external is the complete view, so its header must not scope to built-ins, got:\n%s", ext.String())
	}
	if !strings.Contains(ext.String(), "Agents:") {
		t.Errorf("--external should print the unscoped header, got:\n%s", ext.String())
	}
}

// TestRunAgentList_DefaultFooterNamesEnabledGate verifies that the default
// listing reports when it is omitting something the user opted into. Reading the
// external_agents setting costs one file read and no plugin exec, so the
// no-scan guarantee survives (TestRunAgentList_DefaultPathDoesNotScanPath still
// holds); without it, a user whose `entire agent add <plugin>` turned the gate
// on gets a listing that silently excludes the agent they just installed.
func TestRunAgentList_DefaultFooterNamesEnabledGate(t *testing.T) {
	// Cannot use t.Parallel: setupTestRepo mutates cwd and $PATH.
	setupTestRepo(t)

	writeSettings(t, externalAgentsEnabledSettings)
	var enabled bytes.Buffer
	if err := runAgentList(context.Background(), &enabled, false); err != nil {
		t.Fatalf("runAgentList (gate enabled): %v", err)
	}
	if !strings.Contains(enabled.String(), "External agent plugins are enabled for this repo but are not listed above.") {
		t.Errorf("default listing should report the enabled gate it is omitting, got:\n%s", enabled.String())
	}

	writeSettings(t, externalAgentsUnsetSettings)
	var unset bytes.Buffer
	if err := runAgentList(context.Background(), &unset, false); err != nil {
		t.Fatalf("runAgentList (gate unset): %v", err)
	}
	if strings.Contains(unset.String(), "are enabled for this repo") {
		t.Errorf("gate is unset, so the listing must not claim it is enabled, got:\n%s", unset.String())
	}
	if !strings.Contains(unset.String(), "Run 'entire agent list --external' to also list external plugins on your PATH.") {
		t.Errorf("expected the plain --external pointer when the gate is unset, got:\n%s", unset.String())
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

// TestAgentAdd_InstallsExternalPluginFromPath is the core of #1928 for `add`:
// a plugin that is only discoverable by scanning $PATH must install through
// `entire agent add <name>`, which previously failed with "unknown agent"
// because the command never ran discovery.
func TestAgentAdd_InstallsExternalPluginFromPath(t *testing.T) {
	// Cannot use t.Parallel: see withExternalAgentPlugin. The settings leave
	// external_agents unset on purpose: `add` is itself the opt-in.
	const agentName = "ext-add-test"
	withExternalAgentPlugin(t, agentName, externalAgentsUnsetSettings, false)

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
	// Cannot use t.Parallel: see withExternalAgentPlugin. As with `add`,
	// external_agents stays unset — `remove` must reach the plugin without it.
	const agentName = "ext-remove-test"
	withExternalAgentPlugin(t, agentName, externalAgentsUnsetSettings, false)

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

// TestAgentAdd_PathSeparatorNameReportsUnknownAgent pins that a name which can
// never address a plugin — a mistyped "./claude-code", or a shell-completion
// artifact like "bin/claude-code" — takes the same miss path as any other bad
// name instead of surfacing the raw name-validation error.
func TestAgentAdd_PathSeparatorNameReportsUnknownAgent(t *testing.T) {
	// Cannot use t.Parallel: mutates cwd via t.Chdir.
	setupTestRepo(t)
	writeSettings(t, `{"enabled":true}`)

	cmd := newAgentAddCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"./claude-code"})
	if err := cmd.Execute(); err == nil {
		t.Fatalf("expected an error for a path-separator name, got output:\n%s", out.String())
	}

	if strings.Contains(out.String(), "path separators") {
		t.Errorf("expected the friendly miss path, not the raw validation error, got:\n%s", out.String())
	}
	if !strings.Contains(out.String(), `Unknown agent "./claude-code"`) {
		t.Errorf("expected the unknown-agent listing, got:\n%s", out.String())
	}
	if !strings.Contains(out.String(), agentAddUsage) {
		t.Errorf("expected the `agent add` usage line, got:\n%s", out.String())
	}
}
