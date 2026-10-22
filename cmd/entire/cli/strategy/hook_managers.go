package strategy

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/osroot"
	"github.com/entireio/cli/cmd/entire/cli/paths"
)

// hookManager describes an external hook manager detected in a repository.
type hookManager struct {
	Name       string // "Husky", "Lefthook", "pre-commit", "Overcommit", "hk"
	ConfigPath string // relative path that triggered detection (e.g., ".husky/")
	// ComposesWithEntire is set for hk when it installs Git's config-based
	// hooks, which git runs alongside .git/hooks/* instead of replacing them.
	ComposesWithEntire bool
}

// detectHookManagers checks the repository root for known hook manager config
// files/directories. Detection is filesystem-only (os.Stat, no file reads).
//
// Every manager listed here overwrites Entire's hooks when it installs them —
// verified against pre-commit 4.6.2 ("Use -f to use only pre-commit", which
// saves a .pre-commit.legacy copy), Overcommit 0.73.0 ("Moving old hooks"), and
// hk 1.58.1 — hk only in its legacy mode, which writes .git/hooks/* shims: on
// Git 2.54+ hk 2.x installs config-based hooks (hook.<name>.command), which git
// runs alongside Entire's hook files (verified with hk 2.1.0 on Git 2.55). What
// makes Lefthook different, and what EnsureLefthookIntegration
// exists for, is that it also reclaims .git/hooks/* at the top of every later
// run, so reinstalling on the next agent turn never wins the race.
func detectHookManagers(repoRoot string) []hookManager {
	var managers []hookManager

	checks := []hookManager{
		{Name: "Husky", ConfigPath: ".husky/"},
		{Name: "pre-commit", ConfigPath: ".pre-commit-config.yaml"},
		{Name: "Overcommit", ConfigPath: ".overcommit.yml"},
	}

	// Lefthook supports {.,}lefthook{,-local}.{yml,yaml,json,toml}
	for _, prefix := range []string{"", "."} {
		for _, variant := range []string{"", "-local"} {
			for _, ext := range []string{"yml", "yaml", "json", "toml"} {
				checks = append(checks, hookManager{Name: LefthookManagerName, ConfigPath: prefix + "lefthook" + variant + "." + ext})
			}
		}
	}

	// hk supports {.config/,}hk{,.local}.pkl
	for _, dir := range []string{"", ".config/"} {
		for _, variant := range []string{"", ".local"} {
			checks = append(checks, hookManager{Name: "hk", ConfigPath: dir + "hk" + variant + ".pkl"})
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
		case "hk":
			if !m.ComposesWithEntire {
				writeOverwritesAtInstallWarning(&b, m)
				break
			}
			fmt.Fprintf(&b, "Note: %s detected (%s)\n\n", m.Name, m.ConfigPath)
			fmt.Fprintf(&b, "  %s uses Git's config-based hooks here (Git 2.54+), which run alongside\n", m.Name)
			fmt.Fprintf(&b, "  Entire's hooks instead of replacing them. No action needed.\n")
			fmt.Fprintf(&b, "  (`%s install --legacy` writes hook files instead, which do replace Entire's.)\n\n", m.Name)
		default:
			writeOverwritesAtInstallWarning(&b, m)
		}
	}

	return b.String()
}

// writeOverwritesAtInstallWarning is the warning for a manager that replaces
// Entire's hook files when it installs its own but does not reclaim them later.
func writeOverwritesAtInstallWarning(b *strings.Builder, m hookManager) {
	fmt.Fprintf(b, "Warning: %s detected (%s)\n\n", m.Name, m.ConfigPath)
	fmt.Fprintf(b, "  %s overwrites Entire's hooks when it installs its own.\n", m.Name)
	fmt.Fprintf(b, "  Entire reinstalls them on the next agent turn, but a commit made in\n")
	fmt.Fprintf(b, "  between is not captured. Run 'entire enable' to restore them now.\n\n")
}

// hkComposesWithEntire reports whether hk's hooks run alongside Entire's here.
// hk 2.x uses Git's config-based hooks on Git 2.54+ and falls back to writing
// .git/hooks/* shims on older Git (or with --legacy); a shim already present
// settles it, since the next `hk install` rewrites those files. An unknown
// version is treated as the old behaviour, so the warning errs toward caution.
func hkComposesWithEntire(legacyShimInstalled bool, gitVersion string) bool {
	if legacyShimInstalled {
		return false
	}
	major, minor, ok := parseGitMajorMinor(gitVersion)
	return ok && (major > 2 || (major == 2 && minor >= 54))
}

// parseGitMajorMinor reads the leading "major.minor" of a version such as
// "2.54.1.windows.1".
func parseGitMajorMinor(version string) (major, minor int, ok bool) {
	parts := strings.SplitN(version, ".", 3)
	if len(parts) < 2 {
		return 0, 0, false
	}
	major, errMajor := strconv.Atoi(parts[0])
	minor, errMinor := strconv.Atoi(parts[1])
	if errMajor != nil || errMinor != nil {
		return 0, 0, false
	}
	return major, minor, true
}

// isHkShim recognises the hook file hk's legacy mode writes
// (`test "${HK:-1}" = "0" || exec hk run <hook> --from-hook "$@"`).
func isHkShim(content string) bool {
	return strings.Contains(content, "hk run ") && strings.Contains(content, "--from-hook")
}

// hkLegacyShimInstalled reports whether any file in the hooks directory is an
// hk shim. Best effort: an unreadable directory reads as no shim.
func hkLegacyShimInstalled(ctx context.Context) bool {
	hooksDir, err := GetHooksDir(ctx)
	if err != nil {
		return false
	}
	root, err := hooksRootForRemoval(hooksDir)
	if err != nil {
		return false
	}
	entries, err := osroot.ReadDir(root, ".")
	if err != nil {
		return false
	}
	for _, e := range entries {
		if !e.Type().IsRegular() {
			continue
		}
		if data, err := osroot.ReadFileNoFollow(root, e.Name()); err == nil && isHkShim(string(data)) {
			return true
		}
	}
	return false
}

// installedGitVersion returns `git --version`'s version token, or "".
func installedGitVersion(ctx context.Context) string {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "git", "--version").Output()
	if err != nil {
		return ""
	}
	fields := strings.Fields(string(out))
	if len(fields) < 3 || fields[0] != "git" || fields[1] != "version" {
		return ""
	}
	return fields[2]
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
	for i := range managers {
		if managers[i].Name == "hk" {
			managers[i].ComposesWithEntire = hkComposesWithEntire(hkLegacyShimInstalled(ctx), installedGitVersion(ctx))
		}
	}
	warning := hookManagerWarning(managers, cmdPrefix, declinedLefthookLocalConfig(ctx, repoRoot))
	if warning != "" {
		fmt.Fprintln(w)
		fmt.Fprint(w, warning)
	}
}
