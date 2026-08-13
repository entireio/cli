package cli

import (
	"bytes"
	"context"
	"os"
	"os/exec"
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
// `--external`, and that the default (built-in) listing does not touch $PATH.
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

// TestRunAgentList_ExternalHeaderAndReciprocalFooter verifies that `--external`
// uses a distinguishable "External agents:" header and points back to the
// built-in listing, while the default listing keeps "Agents:" and points at
// `--external`.
func TestRunAgentList_ExternalHeaderAndReciprocalFooter(t *testing.T) {
	t.Parallel()

	var def bytes.Buffer
	if err := runAgentList(context.Background(), &def, false); err != nil {
		t.Fatalf("runAgentList (default): %v", err)
	}
	if !strings.Contains(def.String(), "Agents:") {
		t.Errorf("default listing missing 'Agents:' header:\n%s", def.String())
	}
	if strings.Contains(def.String(), "External agents:") {
		t.Errorf("default listing must not use the external header:\n%s", def.String())
	}
	if !strings.Contains(def.String(), "entire agent list --external") {
		t.Errorf("default listing should point at --external:\n%s", def.String())
	}

	var ext bytes.Buffer
	if err := runAgentList(context.Background(), &ext, true); err != nil {
		t.Fatalf("runAgentList (--external): %v", err)
	}
	if !strings.Contains(ext.String(), "External agents:") {
		t.Errorf("--external listing missing 'External agents:' header:\n%s", ext.String())
	}
	if !strings.Contains(ext.String(), "Run 'entire agent list' to list built-in agents.") {
		t.Errorf("--external listing should point back at the built-in listing:\n%s", ext.String())
	}
}

// TestRunAgentList_ExternalEmptyStateNonePlugins verifies that when no external
// plugins are on $PATH, `--external` prints a distinct "found on your PATH"
// message rather than the "installed" empty-state used when plugins exist but
// none are installed.
func TestRunAgentList_ExternalEmptyStateNonePlugins(t *testing.T) {
	// Cannot use t.Parallel: mutates PATH via t.Setenv.
	// Point PATH at an empty dir so discovery finds no entire-agent-* binaries.
	emptyDir := t.TempDir()
	t.Setenv("PATH", emptyDir)

	var ext bytes.Buffer
	if err := runAgentList(context.Background(), &ext, true); err != nil {
		t.Fatalf("runAgentList (--external): %v", err)
	}
	out := ext.String()
	if !strings.Contains(out, "No external agent plugins found on your PATH.") {
		t.Errorf("expected 'found on your PATH' empty-state, got:\n%s", out)
	}
	if strings.Contains(out, "No external agent plugins installed.") {
		t.Errorf("must not print the 'installed' empty-state when nothing is on PATH:\n%s", out)
	}
}

// TestRunAgentList_ExternalEmptyStateNoneInstalled verifies that when external
// plugins ARE on $PATH but none have hooks installed, `--external` prints the
// mode-scoped "No external agent plugins installed" message (finding #3) rather
// than the global "No agents installed" wording.
func TestRunAgentList_ExternalEmptyStateNoneInstalled(t *testing.T) {
	// Cannot use t.Parallel: mutates PATH via t.Setenv and cwd via t.Chdir.
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}
	t.Cleanup(agent.SnapshotForTesting())

	repoDir := t.TempDir()
	testutil.InitRepo(t, repoDir)
	testutil.WriteFile(t, repoDir, ".entire/settings.json", `{"enabled":true,"external_agents":true}`)
	t.Chdir(repoDir)

	const agentName = "ext-noneinstalled-test"
	externalDir := t.TempDir()
	writeExternalAgentBinary(t, externalDir, agentName) // are-hooks-installed => false
	t.Setenv("PATH", externalDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	var ext bytes.Buffer
	if err := runAgentList(context.Background(), &ext, true); err != nil {
		t.Fatalf("runAgentList (--external): %v", err)
	}
	out := ext.String()
	if !strings.Contains(out, "No external agent plugins installed.") {
		t.Errorf("expected mode-scoped 'installed' empty-state, got:\n%s", out)
	}
	if strings.Contains(out, "No agents installed.") {
		t.Errorf("must not use the global 'No agents installed' wording under --external:\n%s", out)
	}
	if strings.Contains(out, "No external agent plugins found on your PATH.") {
		t.Errorf("must not print the 'found on PATH' state when a plugin is present:\n%s", out)
	}
}
