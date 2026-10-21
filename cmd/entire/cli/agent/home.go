package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
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
	"PI_CODING_AGENT_DIR",
	"PI_CODING_AGENT_SESSION_DIR",
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
// set, whitespace included, because that is what the agents do with a real
// path: Cursor tests e?.trim() and then uses e, Claude reads process.env raw.
// Trimming the value we return would make "/tmp/x " resolve to a directory the
// agent never wrote to. Blank is the one place we knowingly differ: Claude
// resolves an empty CLAUDE_CONFIG_DIR against its working directory (measured
// on 2.1.285), which is almost certainly not what anyone exporting it meant,
// and following it would make the answer depend on the cwd again. The one
// rewrite applied is a leading ~ for the agents in tildeExpandingEnvVars,
// which expand it themselves. A relative value is refused rather than resolved against the working
// directory: inside a hook that is the repo root, for `session resume` it is
// wherever the user stands, so one environment would name a different
// directory in each process. The refusal reuses userdirs.RequireAbsoluteOverride
// so that rule keeps a single implementation.
//
// defaultRel encodes what the variable replaces, and the agents differ.
// CLAUDE_CONFIG_DIR and CODEX_HOME stand in for the dot-directory, so their
// callers pass ".claude" / ".codex"; FACTORY_HOME_OVERRIDE stands in for the
// home itself, so its caller passes "" and appends ".factory" to the result. Read the agent's source or shipped
// binary before choosing, and check where the files actually land: Cursor has a
// variable that looks like one and its transcripts do not follow it.
func ResolveHome(envVar, defaultRel string) (string, error) {
	dir, ok, err := LookupOverride(envVar)
	if err != nil {
		return "", err
	}
	if ok {
		return dir, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("failed to get home directory: %w", err)
	}
	return filepath.Join(home, defaultRel), nil
}

// LookupOverride returns $envVar and true when it is set, under the same policy
// as ResolveHome: blank counts as unset, the value is returned exactly as set,
// a relative value is refused, and envVar must be listed in relocationEnvVars.
//
// Use it where the fallback is not a fixed path under the home: Pi's
// PI_CODING_AGENT_SESSION_DIR replaces a per-repo directory derived from the
// repo path, so its caller derives the fallback itself when ok is false.
func LookupOverride(envVar string) (dir string, ok bool, err error) {
	if !slices.Contains(relocationEnvVars, envVar) {
		return "", false, fmt.Errorf("%s is not listed in agent.relocationEnvVars", envVar)
	}
	dir = os.Getenv(envVar)
	if strings.TrimSpace(dir) == "" {
		return "", false, nil
	}
	if slices.Contains(tildeExpandingEnvVars, envVar) {
		expanded, err := expandLeadingTilde(dir)
		if err != nil {
			return "", false, fmt.Errorf("%s: %w", envVar, err)
		}
		dir = expanded
	}
	if err := userdirs.RequireAbsoluteOverride(envVar, dir); err != nil {
		return "", false, err //nolint:wrapcheck // the error already names the override and its value
	}
	return dir, true, nil
}

// RefusedRelocationEnvVars returns one error for every relocation variable that
// is set to a value ResolveHome refuses, in relocationEnvVars order.
//
// Several callers fail open on that refusal (transcript-owner matching, Codex
// hook-root and trust inspection) and log it only at debug, so without a place
// that reports it a refused variable looks exactly like an unset one until
// `session resume` fails. `entire status` prints this list for that reason.
func RefusedRelocationEnvVars() []error {
	var refused []error
	for _, envVar := range relocationEnvVars {
		if _, _, err := LookupOverride(envVar); err != nil {
			refused = append(refused, err)
		}
	}
	return refused
}

// tildeExpandingEnvVars are the relocation variables whose agent expands a
// leading ~ itself, so Entire has to as well: Pi runs both of its variables
// through normalizePath, which maps "~" and "~/..." (and "~\..." on Windows)
// onto the home directory (@earendil-works/pi-coding-agent 0.99.1,
// utils/paths.js). The other agents are not listed because none was found to
// expand it; for them "~/x" stays a relative path and is refused.
var tildeExpandingEnvVars = []string{
	"PI_CODING_AGENT_DIR",
	"PI_CODING_AGENT_SESSION_DIR",
}

// expandLeadingTilde maps "~" and "~/rest" onto the user's home the way Pi's
// normalizePath does, and returns anything else unchanged. "~user/..." is not
// expanded, by Pi or here.
func expandLeadingTilde(value string) (string, error) {
	var rest string
	switch {
	case value == "~":
	case strings.HasPrefix(value, "~/"):
		rest = value[2:]
	case runtime.GOOS == "windows" && strings.HasPrefix(value, `~\`):
		rest = value[2:]
	default:
		return value, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("expand ~: %w", err)
	}
	return filepath.Join(home, rest), nil
}
