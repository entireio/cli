package strategy

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/paths"
	"gopkg.in/yaml.v3"
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

	// Lefthook supports {.config/,}{.,}lefthook{,-local}.{yml,yaml,json,toml}
	for _, prefix := range []string{"", ".", ".config/"} {
		for _, variant := range []string{"", "-local"} {
			for _, ext := range []string{"yml", "yaml", "json", "toml"} {
				name := prefix + "lefthook" + variant + "." + ext
				checks = append(checks, hookManager{"Lefthook", name, false})
			}
		}
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
		switch {
		case m.OverwritesHooks:
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
		case m.Name == "Lefthook":
			fmt.Fprintf(&b, "Note: %s detected (%s)\n", m.Name, m.ConfigPath)
			fmt.Fprintf(&b, "\n")
			if _, ok := lefthookLocalConfigPath(m.ConfigPath); ok {
				fmt.Fprintf(&b, "  Added a local Lefthook pre-push safety net so Entire session pushes still run if Lefthook reinstalls hooks.\n")
			} else {
				fmt.Fprintf(&b, "  If Lefthook reinstalls hooks, run 'entire enable' to restore Entire's hooks.\n")
			}
			fmt.Fprintf(&b, "\n")
		default:
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

const lefthookEntireCommandName = "entire-push-sessions"

func ensureLefthookSafetyNet(ctx context.Context, cmdPrefix string) error {
	repoRoot, localConfigPath, ok := lefthookSafetyNetConfigPath(ctx)
	if !ok {
		return nil
	}

	config, err := readLefthookLocalConfig(localConfigPath)
	if err != nil {
		return err
	}

	prePush := ensureMap(config, "pre-push")
	commands := ensureMap(prePush, "commands")
	commands[lefthookEntireCommandName] = map[string]any{
		"run":      lefthookSafetyNetCommand(cmdPrefix),
		"priority": -100,
	}

	if err := writeLefthookLocalConfig(localConfigPath, config); err != nil {
		return err
	}
	return addLefthookLocalConfigToGitExclude(ctx, repoRoot, localConfigPath)
}

func removeLefthookSafetyNet(ctx context.Context) error {
	_, localConfigPath, ok := lefthookSafetyNetConfigPath(ctx)
	if !ok {
		return nil
	}

	config, err := readLefthookLocalConfig(localConfigPath)
	if err != nil {
		return err
	}

	prePush, ok := config["pre-push"].(map[string]any)
	if !ok {
		return nil
	}
	commands, ok := prePush["commands"].(map[string]any)
	if !ok {
		return nil
	}
	delete(commands, lefthookEntireCommandName)

	if len(commands) == 0 {
		delete(prePush, "commands")
	}
	if len(prePush) == 0 {
		delete(config, "pre-push")
	}
	if len(config) == 0 {
		if err := os.Remove(localConfigPath); err != nil {
			return fmt.Errorf("remove empty Lefthook local config %s: %w", localConfigPath, err)
		}
		return nil
	}
	return writeLefthookLocalConfig(localConfigPath, config)
}

func lefthookSafetyNetConfigPath(ctx context.Context) (repoRoot, localConfigPath string, ok bool) {
	repoRoot, err := paths.WorktreeRoot(ctx)
	if err != nil {
		return "", "", false
	}
	for _, manager := range detectHookManagers(repoRoot) {
		if manager.Name != "Lefthook" {
			continue
		}
		localRel, ok := lefthookLocalConfigPath(manager.ConfigPath)
		if !ok {
			return "", "", false
		}
		return repoRoot, filepath.Join(repoRoot, localRel), true
	}
	return "", "", false
}

func lefthookLocalConfigPath(configPath string) (string, bool) {
	ext := filepath.Ext(configPath)
	if ext != ".yml" && ext != ".yaml" {
		return "", false
	}

	dir := filepath.Dir(configPath)
	base := filepath.Base(configPath)
	if strings.Contains(base, "lefthook-local") {
		return configPath, true
	}
	localBase := strings.Replace(base, "lefthook", "lefthook-local", 1)
	if dir == "." {
		return localBase, true
	}
	return filepath.Join(dir, localBase), true
}

func readLefthookLocalConfig(path string) (map[string]any, error) {
	data, err := os.ReadFile(path) //nolint:gosec // path is derived from repo root + known Lefthook config name
	if err != nil {
		if os.IsNotExist(err) {
			return make(map[string]any), nil
		}
		return nil, fmt.Errorf("read Lefthook local config %s: %w", path, err)
	}
	if strings.TrimSpace(string(data)) == "" {
		return make(map[string]any), nil
	}

	var config map[string]any
	if err := yaml.Unmarshal(data, &config); err != nil {
		return nil, fmt.Errorf("failed to parse %s: %w", path, err)
	}
	if config == nil {
		config = make(map[string]any)
	}
	return config, nil
}

func writeLefthookLocalConfig(path string, config map[string]any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return fmt.Errorf("create Lefthook local config directory: %w", err)
	}
	data, err := yaml.Marshal(config)
	if err != nil {
		return fmt.Errorf("marshal Lefthook local config: %w", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil { //nolint:gosec // local config should be user-readable like normal config files
		return fmt.Errorf("write Lefthook local config %s: %w", path, err)
	}
	return nil
}

func ensureMap(parent map[string]any, key string) map[string]any {
	if child, ok := parent[key].(map[string]any); ok {
		return child
	}
	child := make(map[string]any)
	parent[key] = child
	return child
}

func lefthookSafetyNetCommand(cmdPrefix string) string {
	return fmt.Sprintf(`hook="$(git rev-parse --git-path hooks)/pre-push"
if ! grep -q %s "$hook" 2>/dev/null; then
  %s hooks git pre-push {1} || true
fi`, shellQuote(entireHookMarker), cmdPrefix)
}

func addLefthookLocalConfigToGitExclude(ctx context.Context, repoRoot, localConfigPath string) error {
	relPath, err := filepath.Rel(repoRoot, localConfigPath)
	if err != nil {
		return fmt.Errorf("make Lefthook local config path relative: %w", err)
	}
	relPath = filepath.ToSlash(relPath)

	gitDir, err := GetGitDir(ctx)
	if err != nil {
		return err
	}
	excludePath := filepath.Join(gitDir, "info", "exclude")
	if err := os.MkdirAll(filepath.Dir(excludePath), 0o750); err != nil {
		return fmt.Errorf("create git info directory: %w", err)
	}

	data, err := os.ReadFile(excludePath) //nolint:gosec // path is derived from git dir
	if err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			if strings.TrimSpace(line) == relPath {
				return nil
			}
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("read git exclude file %s: %w", excludePath, err)
	}

	f, err := os.OpenFile(excludePath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644) //nolint:gosec // git info/exclude is a regular text file
	if err != nil {
		return fmt.Errorf("open git exclude file %s: %w", excludePath, err)
	}
	defer f.Close()

	if len(data) > 0 && !strings.HasSuffix(string(data), "\n") {
		if _, err := f.WriteString("\n"); err != nil {
			return fmt.Errorf("append newline to git exclude file %s: %w", excludePath, err)
		}
	}
	if _, err := fmt.Fprintln(f, relPath); err != nil {
		return fmt.Errorf("append Lefthook local config to git exclude file %s: %w", excludePath, err)
	}
	return nil
}
