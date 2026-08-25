// Package cloudenv writes the shared Entire CLI bootstrap used by remote
// agent environments (Cursor Cloud Agents today; other hosts can invoke the
// same script).
package cloudenv

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/paths"
)

// InstallCLIScriptName is the committed helper Entire enable writes into
// `.entire/`. Cloud environment install commands should invoke it.
const InstallCLIScriptName = "install-cli.sh"

// InstallCLIStep is the install-command fragment that runs the helper from
// the repository root (where Cursor runs `environment.json` `install`).
const InstallCLIStep = "bash .entire/" + InstallCLIScriptName

const maxScriptScanBytes = 1 << 20

// relativeScriptRe finds repo-relative *.sh paths in a shell command.
var relativeScriptRe = regexp.MustCompile(`(?:^|[[:space:];&|])((?:\./)?(?:[[:alnum:]._-]+/)*[[:alnum:]._-]+\.sh)`)

// WriteInstallScript writes `.entire/install-cli.sh` if it is missing or
// differs from the embedded copy. Idempotent.
func WriteInstallScript(ctx context.Context) error {
	root, err := paths.WorktreeRoot(ctx)
	if err != nil {
		root = "."
	}
	dest := filepath.Join(root, paths.EntireDir, InstallCLIScriptName)
	if err := os.MkdirAll(filepath.Dir(dest), 0o750); err != nil {
		return fmt.Errorf("create .entire directory: %w", err)
	}
	if existing, err := os.ReadFile(dest); err == nil && string(existing) == string(installCLIScript) {
		return nil
	}
	if err := os.WriteFile(dest, installCLIScript, 0o755); err != nil { //nolint:gosec // executable helper, not a secret
		return fmt.Errorf("write %s: %w", dest, err)
	}
	return nil
}

// MentionsEntireInstall reports whether an install command (and any repo-relative
// shell scripts it invokes) already installs the Entire CLI.
func MentionsEntireInstall(install, repoRoot string) bool {
	if commandMentionsEntire(install) {
		return true
	}
	for _, script := range referencedScripts(install) {
		path := filepath.Join(repoRoot, filepath.Clean(script))
		if !isRepoRelative(repoRoot, path) {
			continue
		}
		data, err := os.ReadFile(path) //nolint:gosec // bounded, repo-relative script named by install
		if err != nil {
			continue
		}
		if len(data) > maxScriptScanBytes {
			data = data[:maxScriptScanBytes]
		}
		if commandMentionsEntire(string(data)) {
			return true
		}
	}
	return false
}

func commandMentionsEntire(s string) bool {
	markers := []string{
		"entire.io/install.sh",
		".entire/" + InstallCLIScriptName,
		"/usr/local/bin/entire",
		"command -v entire",
	}
	for _, m := range markers {
		if strings.Contains(s, m) {
			return true
		}
	}
	return false
}

func referencedScripts(install string) []string {
	matches := relativeScriptRe.FindAllStringSubmatch(install, -1)
	if len(matches) == 0 {
		return nil
	}
	out := make([]string, 0, len(matches))
	seen := make(map[string]struct{}, len(matches))
	for _, m := range matches {
		p := strings.TrimPrefix(m[1], "./")
		if strings.Contains(p, ":") { // URLs / Windows drives
			continue
		}
		if _, ok := seen[p]; ok {
			continue
		}
		seen[p] = struct{}{}
		out = append(out, p)
	}
	return out
}

func isRepoRelative(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator))
}

// AppendInstallStep adds step to install with `&&` when install does not
// already cover Entire. Empty install becomes step alone.
func AppendInstallStep(install, step string) string {
	install = strings.TrimSpace(install)
	if install == "" {
		return step
	}
	return strings.TrimRight(install, " \t;&") + " && " + step
}

// StripInstallStep removes a trailing `&& step` or a sole step.
func StripInstallStep(install, step string) string {
	install = strings.TrimSpace(install)
	if install == step {
		return ""
	}
	suffix := " && " + step
	if strings.HasSuffix(install, suffix) {
		return strings.TrimSpace(strings.TrimSuffix(install, suffix))
	}
	return install
}
