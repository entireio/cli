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
	Name            string                     // "Husky", "Lefthook", "pre-commit", "Overcommit"
	ConfigPath      string                     // relative path that triggered detection (e.g., ".husky/")
	OverwritesHooks bool                       // true if the tool will overwrite Entire's hooks on reinstall
	IntegrationKind hookManagerIntegrationKind // how Entire can integrate with the manager
}

type hookManagerIntegrationKind string

const (
	hookManagerIntegrationNone          hookManagerIntegrationKind = ""
	hookManagerIntegrationHookDirectory hookManagerIntegrationKind = "hook-directory"
	hookManagerIntegrationLefthook      hookManagerIntegrationKind = "lefthook"
	lefthookManagerName                                            = "Lefthook"
)

// detectHookManagers checks the repository root for known hook manager config
// files/directories. Detection is filesystem-only (os.Stat, no file reads).
func detectHookManagers(repoRoot string) []hookManager {
	var managers []hookManager

	checks := []hookManager{
		{Name: "Husky", ConfigPath: ".husky/", OverwritesHooks: true, IntegrationKind: hookManagerIntegrationHookDirectory},
		{Name: "pre-commit", ConfigPath: ".pre-commit-config.yaml", IntegrationKind: hookManagerIntegrationNone},
		{Name: "Overcommit", ConfigPath: ".overcommit.yml", IntegrationKind: hookManagerIntegrationNone},
	}

	// Lefthook supports root, dotted-root, and .config variants in YAML,
	// JSON/JSONC, and TOML. Keep advisory detection broad even when the safe
	// local integration rejects a format or location it cannot own.
	for _, name := range append(append([]string{}, lefthookMainConfigNames...), lefthookLocalConfigNames...) {
		checks = append(checks, hookManager{
			Name:            lefthookManagerName,
			ConfigPath:      name,
			OverwritesHooks: true,
			IntegrationKind: hookManagerIntegrationLefthook,
		})
	}

	// hk supports {.config/,}hk{,.local}.pkl
	for _, dir := range []string{"", ".config/"} {
		for _, variant := range []string{"", ".local"} {
			name := dir + "hk" + variant + ".pkl"
			checks = append(checks, hookManager{Name: "hk", ConfigPath: name, IntegrationKind: hookManagerIntegrationNone})
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
// cmdPrefix is the CLI command prefix (e.g., "entire" or an absolute binary path).
func hookManagerWarning(managers []hookManager, cmdPrefix string) string {
	if len(managers) == 0 {
		return ""
	}

	var b strings.Builder

	specs := buildHookSpecs(cmdPrefix)

	for _, m := range managers {
		if m.OverwritesHooks {
			fmt.Fprintf(&b, "Warning: %s detected (%s)\n", m.Name, m.ConfigPath)
			fmt.Fprintf(&b, "\n")
			if m.IntegrationKind != hookManagerIntegrationHookDirectory {
				fmt.Fprintf(&b, "  %s may overwrite hooks installed by Entire when it installs or refreshes hooks.\n", m.Name)
				fmt.Fprintf(&b, "  If %s reinstalls hooks, run 'entire enable' to restore Entire's hooks.\n", m.Name)
				fmt.Fprintf(&b, "\n")
				continue
			}
			fmt.Fprintf(&b, "  %s may overwrite hooks installed by Entire on npm install.\n", m.Name)
			fmt.Fprintf(&b, "  To make Entire hooks permanent, add these lines to your %s hook files:\n", m.Name)
			fmt.Fprintf(&b, "\n")

			// Use the config path as the hook directory prefix for hook files.
			// For Husky, this is typically ".husky/" where hook scripts are stored.
			hookDir := m.ConfigPath

			for _, spec := range specs {
				cmdLine := extractCommandLine(spec.content)
				if cmdLine == "" {
					continue
				}
				fmt.Fprintf(&b, "    %s%s:\n", hookDir, spec.name)
				fmt.Fprintf(&b, "      %s\n", cmdLine)
				fmt.Fprintf(&b, "\n")
			}
		} else {
			fmt.Fprintf(&b, "Note: %s detected (%s)\n", m.Name, m.ConfigPath)
			fmt.Fprintf(&b, "\n")
			fmt.Fprintf(&b, "  If %s reinstalls hooks, run 'entire enable' to restore Entire's hooks.\n", m.Name)
			fmt.Fprintf(&b, "\n")
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
	warning := hookManagerWarning(managers, cmdPrefix)
	if warning != "" {
		fmt.Fprintln(w)
		fmt.Fprint(w, warning)
	}
}
