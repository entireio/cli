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

// TestRunAgentList_AvailableExternalRequiresAllFlag verifies that an
// available-but-uninstalled external plugin on $PATH appears only with `--all`,
// not in the default listing.
func TestRunAgentList_AvailableExternalRequiresAllFlag(t *testing.T) {
	// Cannot use t.Parallel because we modify PATH via t.Setenv and cwd via t.Chdir.
	// The mock agent is a #!/bin/sh script run by discovery, so skip on
	// environments without a POSIX shell (matching sibling external-agent tests).
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}

	// external_agents ON so discovery runs; the mock reports hooks NOT installed,
	// i.e. it is available but not installed.
	repoDir := t.TempDir()
	testutil.InitRepo(t, repoDir)
	testutil.WriteFile(t, repoDir, ".entire/settings.json", `{"enabled":true,"external_agents":true}`)
	t.Chdir(repoDir)

	// Unique name so the process-global agent registry can't collide with other tests.
	const agentName = "ext-available-test"
	externalDir := t.TempDir()
	writeExternalAgentBinary(t, externalDir, agentName) // are-hooks-installed => false
	t.Setenv("PATH", externalDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	// Default listing omits an available (not-installed) external plugin.
	var def bytes.Buffer
	if err := runAgentList(context.Background(), &def, false); err != nil {
		t.Fatalf("runAgentList (default): %v", err)
	}
	if strings.Contains(def.String(), agentName) {
		t.Errorf("default listing must not include available (uninstalled) external agent %q, got:\n%s", agentName, def.String())
	}

	// --all surfaces available external plugins on $PATH.
	var all bytes.Buffer
	if err := runAgentList(context.Background(), &all, true); err != nil {
		t.Fatalf("runAgentList (--all): %v", err)
	}
	if !strings.Contains(all.String(), agentName) {
		t.Errorf("--all listing should include available external agent %q, got:\n%s", agentName, all.String())
	}
}

// TestRunAgentList_ShowsInstalledExternalByDefault verifies that an INSTALLED
// external agent appears (marked installed) in the default listing, without
// needing --all — mirroring how a real `entire agent add <external>` enables
// external_agents and installs the plugin's hooks.
func TestRunAgentList_ShowsInstalledExternalByDefault(t *testing.T) {
	// Cannot use t.Parallel because we modify PATH via t.Setenv and cwd via t.Chdir.
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}

	repoDir := t.TempDir()
	testutil.InitRepo(t, repoDir)
	testutil.WriteFile(t, repoDir, ".entire/settings.json", `{"enabled":true,"external_agents":true}`)
	t.Chdir(repoDir)

	// Unique name; the mock reports its hooks ARE installed.
	const agentName = "ext-installed-test"
	externalDir := t.TempDir()
	writeExternalAgentBinaryEx(t, externalDir, agentName, true) // are-hooks-installed => true
	t.Setenv("PATH", externalDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	var def bytes.Buffer
	if err := runAgentList(context.Background(), &def, false); err != nil {
		t.Fatalf("runAgentList (default): %v", err)
	}
	out := def.String()
	if !strings.Contains(out, agentName) {
		t.Errorf("default listing should include installed external agent %q, got:\n%s", agentName, out)
	}
	if !strings.Contains(out, "✓ "+agentName) {
		t.Errorf("installed external agent %q should be marked installed (✓), got:\n%s", agentName, out)
	}
}
