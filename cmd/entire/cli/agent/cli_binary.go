package agent

import (
	"os/exec"

	"github.com/entireio/cli/cmd/entire/cli/agent/types"
)

// cliBinaries maps agent registry keys to the CLI binary name users install
// for that agent (e.g. "claude" for Claude Code, "agent" for Cursor's CLI,
// "droid" for Factory AI Droid). `entire doctor` uses these to surface
// installation issues: hooks can be installed for an agent while the agent's
// own CLI is missing from PATH.
var cliBinaries = map[types.AgentName]string{
	AgentNameClaudeCode:     "claude",
	AgentNameCodex:          "codex",
	AgentNameCopilotCLI:     "copilot",
	AgentNameCursor:         "agent",
	AgentNameFactoryAIDroid: "droid",
	AgentNameGemini:         "gemini",
	AgentNameOpenCode:       "opencode",
	AgentNamePi:             "pi",
}

// CLIBinaryName returns the CLI binary name users install for an agent
// (e.g. "claude" for claude-code, "agent" for cursor). Returns "" for agents
// without a known CLI binary.
func CLIBinaryName(name types.AgentName) string {
	return cliBinaries[name]
}

// IsCLIAvailable reports whether the agent's CLI binary is on PATH.
func IsCLIAvailable(name types.AgentName) bool {
	bin := CLIBinaryName(name)
	if bin == "" {
		return false
	}
	_, err := exec.LookPath(bin)
	return err == nil
}

// AvailableCLIBinaries returns the binary names found on PATH for the given
// agents, in input order.
func AvailableCLIBinaries(names []types.AgentName) []string {
	var found []string
	for _, n := range names {
		if IsCLIAvailable(n) {
			found = append(found, CLIBinaryName(n))
		}
	}
	return found
}

// MissingCLIBinaries returns agent names whose CLI binary is not on PATH,
// in input order. Duplicates in names are collapsed.
func MissingCLIBinaries(names []types.AgentName) []types.AgentName {
	var missing []types.AgentName
	seen := make(map[types.AgentName]bool)
	for _, n := range names {
		if seen[n] {
			continue
		}
		seen[n] = true
		if !IsCLIAvailable(n) {
			missing = append(missing, n)
		}
	}
	return missing
}
