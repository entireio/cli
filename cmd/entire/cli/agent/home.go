package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/entireio/cli/internal/entireclient/userdirs"
)

// relocationEnvVars is every variable a built-in agent honors to relocate its
// per-user state.
//
// Static for the same reason as callerSessionEnvVars: its consumers are test
// harnesses isolating themselves from the developer's real agent homes, and a
// registry-derived list would silently shrink to whatever agents the calling
// binary links. There is no interface to pin it against, so ResolveHome
// enforces it the other way round and refuses a name missing from the list.
var relocationEnvVars = []string{
	"CLAUDE_CONFIG_DIR",
	"CODEX_HOME",
	"COPILOT_HOME",
	"FACTORY_HOME_OVERRIDE",
	"GEMINI_CLI_HOME",
	"PI_CODING_AGENT_DIR",
}

// RelocationEnvVars returns every relocation variable a built-in agent honors,
// complete regardless of which agents the calling binary links. Use it to clear
// the set in a test harness; see relocationEnvVars for why it is static.
func RelocationEnvVars() []string {
	return slices.Clone(relocationEnvVars)
}

// ResolveHome returns the directory an agent keeps its per-user state in:
// $envVar when set, else the user's home joined with defaultRel.
//
// A blank value counts as unset, and a non-blank value is used exactly as
// set, whitespace included, because that is what the agents do: Cursor tests
// e?.trim() and then uses e, Claude reads process.env raw. Trimming the value
// we return would make "/tmp/x " resolve to a directory the agent never wrote
// to. A relative value is refused rather than resolved against the working
// directory: inside a hook that is the repo root, for `session resume` it is
// wherever the user stands, so one environment would name a different
// directory in each process. The refusal reuses userdirs.RequireAbsoluteOverride
// so that rule keeps a single implementation.
//
// defaultRel encodes what the variable replaces, and the agents differ.
// CLAUDE_CONFIG_DIR and CODEX_HOME stand in for the dot-directory, so their
// callers pass ".claude" / ".codex"; GEMINI_CLI_HOME and FACTORY_HOME_OVERRIDE
// stand in for the home itself, so their callers pass "" and append
// ".gemini" / ".factory" to the result. Read the agent's source or shipped
// binary before choosing, and check where the files actually land: Cursor has a
// variable that looks like one and its transcripts do not follow it.
func ResolveHome(envVar, defaultRel string) (string, error) {
	if !slices.Contains(relocationEnvVars, envVar) {
		return "", fmt.Errorf("%s is not listed in agent.relocationEnvVars", envVar)
	}
	if dir := os.Getenv(envVar); strings.TrimSpace(dir) != "" {
		if err := userdirs.RequireAbsoluteOverride(envVar, dir); err != nil {
			return "", err //nolint:wrapcheck // the error already names the override and its value
		}
		return dir, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("failed to get home directory: %w", err)
	}
	return filepath.Join(home, defaultRel), nil
}
