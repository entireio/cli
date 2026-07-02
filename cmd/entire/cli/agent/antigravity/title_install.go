package antigravity

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/jsonutil"
)

// agy reads its window-title command from the GLOBAL config
// ~/.gemini/antigravity-cli/settings.json — a single slot:
//
//	{"title": {"type": "command", "command": "<cmd>"}}
//
// We occupy that slot with the title-tee shim (the title script receives the
// same state JSON as the statusline script — agy's only token-usage surface).
// A pre-existing user command is preserved INSIDE the shim invocation via
// --wrap '<original>', making the config self-describing: uninstall restores
// the original without any backup file. Because the slot is global, per-repo
// `entire disable` does NOT uninstall it (other repos may rely on it); only
// agent removal does.

// configDirEnv overrides the agy config directory (tests).
const configDirEnv = "ENTIRE_ANTIGRAVITY_CONFIG_DIR"

const titleTeeMarker = "hooks antigravity title-tee"

type titleConfig struct {
	Type    string `json:"type"`
	Command string `json:"command"`
}

// agyConfigDir returns the agy config directory, honouring the override env var.
func agyConfigDir() (string, error) {
	if dir := os.Getenv(configDirEnv); dir != "" {
		return dir, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("failed to resolve home dir: %w", err)
	}
	return filepath.Join(home, ".gemini", "antigravity-cli"), nil
}

// titleTeeCommand returns the full shell command string for the title-tee shim.
// If original is non-empty, the original command is wrapped via --wrap.
//
// localDev note: unlike the repo-scoped lifecycle hooks (.agents/hooks.json),
// the title command lives in agy's GLOBAL settings.json and is invoked from
// whatever directory agy runs in. A runtime "$(git rev-parse --show-toplevel)"
// would resolve against the wrong repo — or fail entirely outside one — so we
// resolve the repo root at install time and bake in the absolute main.go path.
// If resolution fails, we fall back to the production "entire ..." form, which
// resolves the dev binary via $PATH.
func titleTeeCommand(localDev bool, original string) string {
	base := "entire hooks antigravity title-tee"
	if localDev {
		if mainPath := localDevMainPath(); mainPath != "" {
			base = "go run " + shellSingleQuote(mainPath) + " hooks antigravity title-tee"
		}
	}
	if original == "" {
		return base
	}
	return base + " --wrap " + shellSingleQuote(original)
}

// localDevMainPath resolves the absolute path to cmd/entire/main.go in the
// current repo at call time, or "" if not inside a git repo.
func localDevMainPath() string {
	out, err := exec.CommandContext(context.Background(), "git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return ""
	}
	root := strings.TrimSpace(string(out))
	if root == "" {
		return ""
	}
	return filepath.Join(root, "cmd", "entire", "main.go")
}

// shellSingleQuote wraps s in POSIX single quotes. Embedded single quotes are
// rewritten with the standard close-escape-reopen technique (see the
// strings.ReplaceAll below) so the result is safe inside a single-quoted shell
// argument.
func shellSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// InstallTitleTee installs the title-tee shim into agy's global settings.json.
// If a user's own title command is already present, it is preserved via --wrap.
// The call is idempotent: if our marker is already in the command, it returns nil.
func InstallTitleTee(localDev bool) error {
	cfgDir, err := agyConfigDir()
	if err != nil {
		return err
	}
	settingsPath := filepath.Join(cfgDir, "settings.json")

	rawFile, err := readAgySettings(settingsPath)
	if err != nil {
		return err
	}

	// Parse existing title entry (if any). Unparseable → treat as absent.
	var existing titleConfig
	if raw, ok := rawFile["title"]; ok {
		_ = json.Unmarshal(raw, &existing) //nolint:errcheck // treat unparseable as absent
	}

	// Idempotency: already contains our marker.
	if strings.Contains(existing.Command, titleTeeMarker) {
		return nil
	}

	// Build new title config, wrapping any pre-existing command.
	cfg := titleConfig{
		Type:    "command",
		Command: titleTeeCommand(localDev, existing.Command),
	}

	cfgBytes, err := jsonutil.MarshalWithNoHTMLEscape(cfg)
	if err != nil {
		return fmt.Errorf("failed to marshal title config: %w", err)
	}
	rawFile["title"] = cfgBytes

	return writeAgySettings(rawFile, settingsPath)
}

// TitleTeeInstalled reports whether agy's global settings.json declares a
// title command containing the title-tee marker. It is used by `entire doctor`
// to warn when Antigravity hooks are installed in a repo but the global title
// slot — agy's only token-usage surface — has not been claimed, which would
// leave token counts missing from checkpoints. A missing or unparseable
// settings file reports false.
func TitleTeeInstalled() bool {
	cfgDir, err := agyConfigDir()
	if err != nil {
		return false
	}
	settingsPath := filepath.Join(cfgDir, "settings.json")

	rawFile, err := readAgySettings(settingsPath)
	if err != nil {
		return false
	}

	raw, ok := rawFile["title"]
	if !ok {
		return false
	}

	var existing titleConfig
	if err := json.Unmarshal(raw, &existing); err != nil {
		return false
	}
	return strings.Contains(existing.Command, titleTeeMarker)
}

// UninstallTitleTee removes or restores the title entry in agy's global settings.json:
//   - bare tee (no --wrap)      → delete "title" key
//   - tee with --wrap 'X'       → restore X
//   - any other (foreign) cmd   → leave untouched
//   - missing settings file     → no-op
func UninstallTitleTee() error {
	cfgDir, err := agyConfigDir()
	if err != nil {
		return err
	}
	settingsPath := filepath.Join(cfgDir, "settings.json")

	// Missing file → nothing to uninstall.
	if _, err := os.Stat(settingsPath); os.IsNotExist(err) {
		return nil
	}

	rawFile, err := readAgySettings(settingsPath)
	if err != nil {
		return err
	}

	raw, ok := rawFile["title"]
	if !ok {
		return nil // no title key — nothing to do
	}

	var existing titleConfig
	if err := json.Unmarshal(raw, &existing); err != nil {
		return nil //nolint:nilerr // unparseable title entry — leave it alone rather than destroying user data
	}

	// Not our command → leave untouched.
	if !strings.Contains(existing.Command, titleTeeMarker) {
		return nil
	}

	wrapped, hasWrap := extractWrappedCommand(existing.Command)
	if hasWrap {
		// Restore the original command.
		restored := titleConfig{
			Type:    "command",
			Command: wrapped,
		}
		restoredBytes, err := jsonutil.MarshalWithNoHTMLEscape(restored)
		if err != nil {
			return fmt.Errorf("failed to marshal restored title config: %w", err)
		}
		rawFile["title"] = restoredBytes
	} else {
		// Only delete the key when it is exactly one of the canonical bare-tee
		// strings we would have written ourselves. Anything else containing the
		// marker is either a user-authored wrapper (e.g. "my-wrapper.sh 'entire
		// hooks antigravity title-tee'") or a corrupted command — leaving it
		// alone is always safer than deleting the user's config.
		bare := existing.Command == titleTeeCommand(false, "") ||
			existing.Command == titleTeeCommand(true, "")
		if !bare {
			return nil
		}
		delete(rawFile, "title")
	}

	return writeAgySettings(rawFile, settingsPath)
}

// extractWrappedCommand parses the --wrap '<original>' portion of a title-tee
// command string. It returns the original command and true if found and valid,
// or ("", false) otherwise.
func extractWrappedCommand(command string) (string, bool) {
	const wrapFlag = " --wrap "
	idx := strings.Index(command, wrapFlag)
	if idx < 0 {
		return "", false
	}
	rest := strings.TrimSpace(command[idx+len(wrapFlag):])
	if len(rest) < 2 || rest[0] != '\'' || rest[len(rest)-1] != '\'' {
		return "", false
	}
	// Strip outer single quotes and reverse the '\'' escaping.
	inner := rest[1 : len(rest)-1]
	return strings.ReplaceAll(inner, `'\''`, "'"), true
}

// readAgySettings reads and parses settings.json into a raw map.
// A missing file returns an empty map (not an error).
func readAgySettings(settingsPath string) (map[string]json.RawMessage, error) {
	rawFile := make(map[string]json.RawMessage)
	data, err := os.ReadFile(settingsPath) //nolint:gosec // path is constructed from config dir + fixed filename
	if os.IsNotExist(err) {
		return rawFile, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to read agy settings: %w", err)
	}
	if err := json.Unmarshal(data, &rawFile); err != nil {
		return nil, fmt.Errorf("failed to parse agy settings: %w", err)
	}
	return rawFile, nil
}

// writeAgySettings marshals rawFile and writes it to settingsPath, creating
// parent directories as needed.
func writeAgySettings(rawFile map[string]json.RawMessage, settingsPath string) error {
	return writeJSONMapFile(rawFile, settingsPath, "agy settings")
}
