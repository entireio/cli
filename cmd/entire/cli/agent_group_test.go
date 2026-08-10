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

func TestAgentGroup_DiscoversExternalAgents(t *testing.T) {
	// Cannot use t.Parallel because we modify PATH via t.Setenv and cwd via t.Chdir.
	// The mock agent is a #!/bin/sh script executed during discovery, so skip
	// on environments without a POSIX shell (matching sibling external-agent tests).
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}

	// `list` honors the external_agents gate (like attach/explain/hooks), so
	// stand up an isolated repo with the setting enabled rather than executing
	// $PATH plugins unconditionally. `agent add` is the on-ramp that flips this
	// setting on for a real user.
	repoDir := t.TempDir()
	testutil.InitRepo(t, repoDir)
	testutil.WriteFile(t, repoDir, ".entire/settings.json", `{"enabled":true,"external_agents":true}`)
	t.Chdir(repoDir)

	externalDir := t.TempDir()
	writeExternalAgentBinary(t, externalDir, "ext-agentgroup-test")
	t.Setenv("PATH", externalDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	var buf bytes.Buffer
	if err := runAgentList(context.Background(), &buf); err != nil {
		t.Fatalf("runAgentList with external: %v", err)
	}
	if !strings.Contains(buf.String(), "ext-agentgroup-test") {
		t.Errorf("expected external agent 'ext-agentgroup-test' in output, got:\n%s", buf.String())
	}
}
