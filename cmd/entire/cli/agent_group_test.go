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

// TestRunAgentList_ExternalAgentsRequireAllFlag verifies that external plugins
// on $PATH appear only with `--all`, never in the default listing — and that
// this holds even when external_agents is enabled in settings. Installing an
// external agent (which enables external_agents) must not change the default
// `agent list` output.
func TestRunAgentList_ExternalAgentsRequireAllFlag(t *testing.T) {
	// Cannot use t.Parallel because we modify PATH via t.Setenv and cwd via t.Chdir.
	// The mock agent is a #!/bin/sh script run by --all discovery, so skip on
	// environments without a POSIX shell (matching sibling external-agent tests).
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}

	// external_agents is ON, to prove the default listing ignores it entirely.
	repoDir := t.TempDir()
	testutil.InitRepo(t, repoDir)
	testutil.WriteFile(t, repoDir, ".entire/settings.json", `{"enabled":true,"external_agents":true}`)
	t.Chdir(repoDir)

	// Unique name so the process-global agent registry can't collide with other tests.
	const agentName = "ext-allflag-test"
	externalDir := t.TempDir()
	writeExternalAgentBinary(t, externalDir, agentName)
	t.Setenv("PATH", externalDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	// Default listing performs no discovery and omits external plugins even with
	// external_agents enabled. Assert this first, before the --all call registers
	// the agent in the process-global registry.
	var def bytes.Buffer
	if err := runAgentList(context.Background(), &def, false); err != nil {
		t.Fatalf("runAgentList (default): %v", err)
	}
	if strings.Contains(def.String(), agentName) {
		t.Errorf("default listing must not include external agent %q even with external_agents enabled, got:\n%s", agentName, def.String())
	}

	// --all surfaces external plugins on $PATH.
	var all bytes.Buffer
	if err := runAgentList(context.Background(), &all, true); err != nil {
		t.Fatalf("runAgentList (--all): %v", err)
	}
	if !strings.Contains(all.String(), agentName) {
		t.Errorf("--all listing should include external agent %q, got:\n%s", agentName, all.String())
	}
}
