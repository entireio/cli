package strategy

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/paths"
)

// hookManager describes an external hook manager detected in a repository.
type hookManager struct {
	Name       string // "Husky", "Lefthook", "pre-commit", "Overcommit", "hk"
	ConfigPath string // relative path that triggered detection (e.g., ".husky/")
}

// detectHookManagers checks the repository root for known hook manager config
// files/directories. Detection is filesystem-only (os.Stat, no file reads).
//
// Every manager listed here overwrites Entire's hooks when it installs them —
// verified against pre-commit 4.6.2 ("Use -f to use only pre-commit", which
// saves a .pre-commit.legacy copy), Overcommit 0.73.0 ("Moving old hooks"), and
// hk 1.58.1. What makes Lefthook different, and what EnsureLefthookIntegration
// exists for, is that it also reclaims .git/hooks/* at the top of every later
// run, so reinstalling on the next agent turn never wins the race.
func detectHookManagers(repoRoot string) []hookManager {
	var managers []hookManager

	checks := []hookManager{
		{"Husky", ".husky/"},
		{"pre-commit", ".pre-commit-config.yaml"},
		{"Overcommit", ".overcommit.yml"},
	}

	// Lefthook supports {.,}lefthook{,-local}.{yml,yaml,json,toml}
	for _, prefix := range []string{"", "."} {
		for _, variant := range []string{"", "-local"} {
			for _, ext := range []string{"yml", "yaml", "json", "toml"} {
				checks = append(checks, hookManager{LefthookManagerName, prefix + "lefthook" + variant + "." + ext})
			}
		}
	}

	// hk supports {.config/,}hk{,.local}.pkl
	for _, dir := range []string{"", ".config/"} {
		for _, variant := range []string{"", ".local"} {
			checks = append(checks, hookManager{"hk", dir + "hk" + variant + ".pkl"})
		}
	}

	seen := make(map[string]bool)
	for _, c := range checks {
		path := filepath.Join(repoRoot, c.ConfigPath)
		if _, err := os.Stat(path); err == nil {
			if seen[c.Name] {
				continue // e.g., lefthook.yml and .lefthook.yml both present
			}
			seen[c.Name] = true
			managers = append(managers, c)
		}
	}

	return managers
}

// hookManagerWarning builds a warning string for detected hook managers.
// cmdPrefix is the CLI command prefix (e.g., "entire" or an absolute binary
// path); declined is the phrase from declinedLefthookLocalConfig, empty when
// Entire is able to register with Lefthook.
func hookManagerWarning(managers []hookManager, cmdPrefix, declined string) string {
	if len(managers) == 0 {
		return ""
	}

	var b strings.Builder

	for _, m := range managers {
		switch m.Name {
		case LefthookManagerName:
			if declined != "" {
				// "No action needed" is true only when Entire can register.
				// Where it has declined, the repo is on native hooks that
				// Lefthook reclaims — telling the user otherwise is the false
				// advice this integration was meant to remove (#2263).
				fmt.Fprintf(&b, "Warning: %s detected (%s)\n\n", m.Name, m.ConfigPath)
				fmt.Fprintf(&b, "  Entire could not register in %s's configuration: %s,\n", m.Name, declined)
				fmt.Fprintf(&b, "  and Entire will not modify it. Entire's own hooks are used instead,\n")
				fmt.Fprintf(&b, "  and %s reclaims the hooks it manages — Entire reinstalls them on\n", m.Name)
				fmt.Fprintf(&b, "  the next agent turn, so a commit made in between is not captured.\n\n")
				break
			}
			// Entire registers itself in Lefthook's own config, so Lefthook's
			// regenerations no longer remove it and there is nothing for the
			// user to do. See EnsureLefthookIntegration.
			fmt.Fprintf(&b, "Note: %s detected (%s)\n\n", m.Name, m.ConfigPath)
			fmt.Fprintf(&b, "  %s regenerates Git hooks whenever its config changes or it runs.\n", m.Name)
			fmt.Fprintf(&b, "  Entire registers itself in %s's own configuration instead of owning\n", m.Name)
			fmt.Fprintf(&b, "  the hook files, so those regenerations no longer remove it.\n")
			fmt.Fprintf(&b, "  No action needed.\n\n")
		case "Husky":
			// Husky's hooks are hand-editable shell scripts it does not
			// rewrite, so the durable fix is to add Entire's line to them.
			fmt.Fprintf(&b, "Warning: %s detected (%s)\n\n", m.Name, m.ConfigPath)
			fmt.Fprintf(&b, "  %s may overwrite hooks installed by Entire on npm install.\n", m.Name)
			fmt.Fprintf(&b, "  To make Entire hooks permanent, add these lines to your %s hook files:\n\n", m.Name)
			for _, spec := range buildHookSpecs(cmdPrefix) {
				cmdLine := extractCommandLine(spec.content)
				if cmdLine == "" {
					continue
				}
				fmt.Fprintf(&b, "    %s%s:\n", m.ConfigPath, spec.name)
				fmt.Fprintf(&b, "      %s\n\n", cmdLine)
			}
		default:
			fmt.Fprintf(&b, "Warning: %s detected (%s)\n\n", m.Name, m.ConfigPath)
			fmt.Fprintf(&b, "  %s overwrites Entire's hooks when it installs its own.\n", m.Name)
			fmt.Fprintf(&b, "  Entire reinstalls them on the next agent turn, but a commit made in\n")
			fmt.Fprintf(&b, "  between is not captured. Run 'entire enable' to restore them now.\n\n")
		}
	}

	return b.String()
}

// extractCommandLine returns the first non-shebang, non-comment, non-empty line
// from a hook script. This is the actual command invocation line.
func extractCommandLine(hookContent string) string {
	for _, line := range strings.Split(hookContent, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		return trimmed
	}
	return ""
}

// CheckAndWarnHookManagers detects external hook managers and writes a warning
// to w if any are found.
// absolutePath embeds the full binary path for GUI git clients.
func CheckAndWarnHookManagers(ctx context.Context, w io.Writer, absolutePath bool) {
	repoRoot, err := paths.WorktreeRoot(ctx)
	if err != nil {
		return
	}

	managers := detectHookManagers(repoRoot)
	if len(managers) == 0 {
		return
	}

	cmdPrefix, err := hookCmdPrefix(absolutePath)
	if err != nil {
		// Best-effort: hook manager warnings are advisory, skip on resolution failure
		return
	}
	warning := hookManagerWarning(managers, cmdPrefix, declinedLefthookLocalConfig(ctx, repoRoot))
	if warning != "" {
		fmt.Fprintln(w)
		fmt.Fprint(w, warning)
	}
}
