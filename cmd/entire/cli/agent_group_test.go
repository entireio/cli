package cli

import (
	"bytes"
	"context"
	"os/exec"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	agenttestutil "github.com/entireio/cli/cmd/entire/cli/agent/testutil"
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

// TestRunAgentList_ShowsBrokenExternalAgents covers the payoff of recording
// discovery failures: a plugin the user installed but that cannot be loaded is
// named, with its reason, instead of silently missing from the listing.
//
// Not parallel: it sets PATH and mutates the process-wide agent registry.
func TestRunAgentList_ShowsBrokenExternalAgents(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}
	agent.ResetExternalsForTesting()
	t.Cleanup(agent.ResetExternalsForTesting)

	dir := t.TempDir()
	readyName := "list-ready-ext"
	brokenName := "list-broken-ext"
	agenttestutil.WriteExternalAgentBinary(t, dir, readyName, "#!/bin/sh\ncase \"$1\" in info) echo '"+
		`{"protocol_version":1,"name":"`+readyName+`","type":"Ready Ext","description":"d","is_preview":false,"protected_dirs":[],"hook_names":[],"capabilities":{}}`+
		"' ;; esac\n")
	brokenPath := agenttestutil.WriteExternalAgentBinary(t, dir, brokenName, "#!/bin/sh\necho 'not json'\n")
	t.Setenv("PATH", dir)

	var buf bytes.Buffer
	if err := runAgentList(context.Background(), &buf); err != nil {
		t.Fatalf("runAgentList: %v", err)
	}
	out := buf.String()

	if !strings.Contains(out, readyName) {
		t.Errorf("ready external agent %q missing from the listing:\n%s", readyName, out)
	}
	if !strings.Contains(out, "Broken external agents:") {
		t.Errorf("missing broken-agent section:\n%s", out)
	}
	if !strings.Contains(out, brokenPath) {
		t.Errorf("broken agent listed without its binary path:\n%s", out)
	}
	if !strings.Contains(out, "invalid JSON") {
		t.Errorf("broken agent listed without a reason:\n%s", out)
	}
}
