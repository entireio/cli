package strategy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	toml "github.com/pelletier/go-toml/v2"
	yaml "gopkg.in/yaml.v3"

	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/paths"
)

// hookManager describes an external hook manager detected in a repository.
type hookManager struct {
	Name            string // "Husky", "Lefthook", "pre-commit", "Overcommit"
	ConfigPath      string // relative path that triggered detection (e.g., ".husky/")
	OverwritesHooks bool   // true if the tool will overwrite Entire's hooks on reinstall
}

// lefthookConfigCandidates returns lefthook's supported config filenames in
// precedence order: the main config first, then the -local overlay. lefthook
// accepts {.,}lefthook{,-local}.{yml,yaml,json,toml}.
func lefthookConfigCandidates() (main, local []string) {
	exts := []string{"yml", "yaml", "json", "toml"}
	for _, prefix := range []string{"", "."} {
		for _, ext := range exts {
			main = append(main, prefix+"lefthook."+ext)
			local = append(local, prefix+"lefthook-local."+ext)
		}
	}
	return main, local
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
	mainCfgs, localCfgs := lefthookConfigCandidates()
	for _, name := range append(append([]string{}, mainCfgs...), localCfgs...) {
		checks = append(checks, hookManager{"Lefthook", name, false})
	}

	// hk supports {.config/,}hk{,.local}.pkl
	for _, dir := range []string{"", ".config/"} {
		for _, variant := range []string{"", ".local"} {
			name := dir + "hk" + variant + ".pkl"
			checks = append(checks, hookManager{"hk", name, false})
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
// cmdPrefix is the CLI command prefix (e.g., "entire" or "./scripts/entire-dev").
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
// localDev controls whether the warning references "go run" or the "entire" binary.
// absolutePath embeds the full binary path for GUI git clients.
func CheckAndWarnHookManagers(ctx context.Context, w io.Writer, localDev, absolutePath bool) {
	// External backend = user explicitly opted into a hook manager (Husky / Rush /
	// etc.) and configured Entire to coexist via marker detection. Warning that
	// the manager exists would be noise, and the suggested "add these lines"
	// fix is already the user's own setup path.
	//
	// If settings can't be loaded, skip the warning: whichever caller invoked
	// InstallGitHook (the normal preceding step) will already have surfaced
	// the same load error.
	_, isExternal, err := externalGitHooksDir(ctx)
	if err != nil {
		logging.Warn(ctx, "external git hooks: settings load failed; skipping hook-manager warning",
			"error", err.Error())
		return
	}
	if isExternal {
		return
	}

	repoRoot, err := paths.WorktreeRoot(ctx)
	if err != nil {
		return
	}

	managers := detectHookManagers(repoRoot)
	if len(managers) == 0 {
		return
	}

	cmdPrefix, err := hookCmdPrefix(localDev, absolutePath)
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

// isEntireWiredIntoManager reports whether every managed git hook dispatches
// to Entire through the config-driven hook manager (currently only lefthook).
// Unlike directory-of-scripts detection, this parses the manager's config
// file(s) and confirms each of the 5 managed hooks invokes
// `entire hooks git <hook>`.
func isEntireWiredIntoManager(repoRoot, manager string) (bool, error) {
	switch manager {
	case "lefthook":
		wired, err := lefthookWiredHooks(repoRoot)
		if err != nil {
			return false, err
		}
		for _, hook := range gitHookNames {
			if !wired[hook] {
				return false, nil
			}
		}
		return true, nil
	default:
		return false, fmt.Errorf("unsupported hook manager %q", manager)
	}
}

// lefthookWiredHooks parses the lefthook config (main + -local overlay) and
// returns the set of managed git hooks that invoke `entire hooks git <hook>`.
// A hook counts as wired if any command's `run` value or any scripts entry
// (a script file whose contents call the dispatcher) references the hook's
// dispatch command. Returns an empty (non-nil) set when no config exists.
func lefthookWiredHooks(repoRoot string) (map[string]bool, error) {
	wired := make(map[string]bool)

	mainCfgs, localCfgs := lefthookConfigCandidates()
	// Parse main config first, then overlay -local; a hook wired in either
	// counts (union), matching lefthook's own merge behavior.
	for _, group := range [][]string{mainCfgs, localCfgs} {
		path, ok := firstExisting(repoRoot, group)
		if !ok {
			continue
		}
		cfg, err := parseLefthookConfig(path)
		if err != nil {
			return nil, fmt.Errorf("parse lefthook config %s: %w", filepath.Base(path), err)
		}
		for _, hook := range gitHookNames {
			if wired[hook] {
				continue
			}
			if lefthookHookInvokesEntire(repoRoot, cfg, hook) {
				wired[hook] = true
			}
		}
	}
	return wired, nil
}

// firstExisting returns the first candidate under repoRoot that exists.
func firstExisting(repoRoot string, candidates []string) (string, bool) {
	for _, name := range candidates {
		p := filepath.Join(repoRoot, name)
		if _, err := os.Stat(p); err == nil {
			return p, true
		}
	}
	return "", false
}

// lefthookConfigPath returns the absolute path to the repo's primary lefthook
// config file (ignoring the -local overlay), if one exists.
func lefthookConfigPath(repoRoot string) (string, bool) {
	mainCfgs, _ := lefthookConfigCandidates()
	return firstExisting(repoRoot, mainCfgs)
}

// parseLefthookConfig reads a lefthook config file and decodes it into a
// generic map, dispatching on the file extension (yml/yaml/json/toml).
func parseLefthookConfig(path string) (map[string]any, error) {
	data, err := os.ReadFile(path) //nolint:gosec // path comes from a fixed candidate list joined to repoRoot
	if err != nil {
		return nil, fmt.Errorf("read lefthook config: %w", err)
	}
	out := make(map[string]any)
	switch strings.ToLower(filepath.Ext(path)) {
	case ".yml", ".yaml":
		if err := yaml.Unmarshal(data, &out); err != nil {
			return nil, fmt.Errorf("parse lefthook YAML: %w", err)
		}
	case ".json":
		if err := json.Unmarshal(data, &out); err != nil {
			return nil, fmt.Errorf("parse lefthook JSON: %w", err)
		}
	case ".toml":
		if err := toml.Unmarshal(data, &out); err != nil {
			return nil, fmt.Errorf("parse lefthook TOML: %w", err)
		}
	default:
		return nil, fmt.Errorf("unsupported lefthook config extension %q", filepath.Ext(path))
	}
	return out, nil
}

// lefthookHookInvokesEntire reports whether the given hook's config section
// calls `entire hooks git <hook>`, either directly in a command's run value
// or via a scripts entry whose script file contains the dispatch call.
func lefthookHookInvokesEntire(repoRoot string, cfg map[string]any, hook string) bool {
	section, ok := cfg[hook].(map[string]any)
	if !ok {
		return false
	}
	dispatch := "entire hooks git " + hook

	// commands.<name>.run
	if commands, ok := section["commands"].(map[string]any); ok {
		for _, cmd := range commands {
			if cmdMap, ok := cmd.(map[string]any); ok {
				if run, ok := cmdMap["run"].(string); ok && strings.Contains(run, dispatch) {
					return true
				}
			}
		}
	}

	// scripts.<file>: the dispatch lives in the script file under
	// .lefthook/<hook>/<file> (lefthook's default source_dir). Read it.
	if scripts, ok := section["scripts"].(map[string]any); ok {
		for scriptName := range scripts {
			scriptPath := filepath.Join(repoRoot, ".lefthook", hook, scriptName)
			if data, err := os.ReadFile(scriptPath); err == nil && strings.Contains(string(data), dispatch) { //nolint:gosec // path derived from repo-relative config
				return true
			}
		}
	}

	return false
}
