package strategy

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/paths"
)

// hookManager describes an external hook manager detected in a repository.
type hookManager struct {
	Name            string // "Husky", "Lefthook", "pre-commit", "Overcommit"
	ConfigPath      string // relative path that triggered detection (e.g., ".husky/")
	OverwritesHooks bool   // true if the tool will overwrite Entire's hooks on reinstall
}

// detectHookManagers checks the repository root for known hook manager config
// files/directories. Detection is filesystem-only (os.Stat, no file reads).
func detectHookManagers(repoRoot string) []hookManager {
	var managers []hookManager

	checks := []hookManager{
		{"Husky", ".husky/", true},
		{"pre-commit", ".pre-commit-config.yaml", false},
		{"Overcommit", ".overcommit.yml", false},
	}

	// Lefthook supports {.,}lefthook{,-local}.{yml,yaml,json,toml}
	for _, prefix := range []string{"", "."} {
		for _, variant := range []string{"", "-local"} {
			for _, ext := range []string{"yml", "yaml", "json", "toml"} {
				name := prefix + "lefthook" + variant + "." + ext
				checks = append(checks, hookManager{"Lefthook", name, false})
			}
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
// cmdPrefix is the CLI command prefix (e.g., "entire" or "go run ./cmd/entire/main.go").
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
func CheckAndWarnHookManagers(w io.Writer) {
	repoRoot, err := paths.RepoRoot()
	if err != nil {
		return
	}

	managers := detectHookManagers(repoRoot)
	if len(managers) == 0 {
		return
	}

	warning := hookManagerWarning(managers, hookCmdPrefix())
	if warning != "" {
		fmt.Fprintln(w)
		fmt.Fprint(w, warning)
	}
}
