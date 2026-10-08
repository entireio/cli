package antigravity

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/osroot"
)

// GenerateText submits a non-interactive prompt to the Antigravity CLI. The
// binary is `agy`; -p is the short alias for --print (single-prompt mode).
//
// The prompt travels in argv. Earlier releases accepted it on stdin behind a
// single-space -p placeholder (the Gemini CLI convention, verified on agy
// 1.0.16), but agy 1.2.7 ignores stdin in print mode: `-p " "` fails with
// "Error: empty prompt", and `-p -` is answered as the literal message "-"
// (both observed live, trail 444, 2026-09-22), which is how
// `entire dispatch --local --agent antigravity` came to hand agy an empty
// prompt. argv is the only documented route ("Usage: agy --print 'your
// prompt here'").
//
// That makes prompt size this agent's problem in a way it is not for agents
// that use RunIsolatedTextGeneratorCLI's stdin. Linux caps a SINGLE argument
// at MAX_ARG_STRLEN (128 KiB) however large the total ARG_MAX is, so an
// unbounded prompt fails with E2BIG. summarize.maxCondensedTranscriptBytes is
// what keeps summary prompts inside it. Windows' ~32 KiB whole-command-line
// limit is tighter than any useful transcript budget and is not covered; a
// long enough prompt still fails there, loudly.
//
// agy also runs with a home directory of its own (isolatedHomeEnv). It reads
// its window-title command (~/.gemini/antigravity-cli/settings.json), its
// PreInvocation hooks (~/.gemini/config/hooks.json), MCP servers, and skills
// from the user's home, and print mode executes the title command and the
// hooks (observed live on agy 1.3.1). The empty working directory keeps
// workspace configuration out but not that, and agy has no flag to skip it,
// so a summary run, whose prompt carries untrusted transcript content, would
// otherwise execute whatever the user's global configuration names.
func (a *AntigravityAgent) GenerateText(ctx context.Context, prompt string, model string) (string, error) {
	args := []string{"-p", prompt}
	if model != "" {
		args = append(args, "--model", model)
	}
	home, cleanup, err := agent.NewTextGenerationDir()
	if err != nil {
		return "", fmt.Errorf("antigravity text generation failed: %w", err)
	}
	defer cleanup()
	env, err := isolatedHomeEnv(home)
	if err != nil {
		return "", fmt.Errorf("antigravity text generation failed: %w", err)
	}
	result, capturedStderr, stdoutBytes, err := agent.RunIsolatedTextGeneratorCLI(ctx, a.CommandRunner, "agy", "antigravity", args, "", env...)
	if err != nil {
		if strings.Contains(capturedStderr, "Authentication required") {
			err = fmt.Errorf("%w: agy runs summaries with an isolated home so your agy settings, hooks, and MCP servers are not loaded; only its sign-in is carried over (the OS keyring, its file token, application-default credentials, or Gemini API-key mode), and none of them authenticated. Run `agy` to sign in again, or choose another summary provider", err)
		}
		return "", &agent.TextGenerationError{
			Err:         fmt.Errorf("antigravity text generation failed: %w", err),
			Stderr:      capturedStderr,
			StdoutBytes: stdoutBytes,
		}
	}
	return result, nil
}

// isolatedHomeEnv returns the environment overrides that point agy at home
// instead of the user's home directory: HOME (USERPROFILE on Windows, where
// Go's os.UserHomeDir reads it) and the XDG base directories.
//
// This keeps agy from LOADING the user's configuration; it is not a
// filesystem sandbox. agy has no switch to remove its tools, so an injected
// instruction can still name an absolute path, which only the empty working
// directory and agy's own approvals stand against.
//
// Each way agy signs in is carried over, as a credential and nothing else:
//   - keyring: agy keeps its sign-in in the macOS login keychain, which it
//     locates through $HOME, so on macOS the login keychain FILE is linked in
//     (linkLoginKeychain). Elsewhere the keyring (Windows Credential Manager,
//     the Secret Service on Linux) is not under the home directory.
//   - file token: where no keyring is usable (agy detects an SSH session, for
//     one), agy stores its sign-in in antigravity-oauth-token in its config
//     directory, so that one file is linked in (linkFileToken).
//   - application-default credentials: agy's ADC mode (AGY_ADC_AUTH, which is
//     inherited) honours GOOGLE_APPLICATION_CREDENTIALS before looking under
//     $HOME, so the variable is pointed at the user's credentials file
//     (adcCredentialsEnv). On Windows the file is found through %APPDATA%,
//     which is inherited unchanged.
//   - API key: the key comes from the environment, which is inherited
//     unchanged, and the mode from settings.json, which carryAPIKeyMode
//     recreates.
//
// The file locations were observed on agy 1.3.1 by tracing which paths it
// opens under an empty home.
func isolatedHomeEnv(home string) ([]string, error) {
	if runtime.GOOS == "darwin" {
		if err := linkLoginKeychain(home); err != nil {
			return nil, err
		}
	}
	if err := linkFileToken(home); err != nil {
		return nil, err
	}
	if err := carryAPIKeyMode(home); err != nil {
		return nil, err
	}
	env := []string{
		"HOME=" + home,
		"XDG_CONFIG_HOME=" + filepath.Join(home, ".config"),
		"XDG_DATA_HOME=" + filepath.Join(home, ".local", "share"),
		"XDG_STATE_HOME=" + filepath.Join(home, ".local", "state"),
		"XDG_CACHE_HOME=" + filepath.Join(home, ".cache"),
	}
	if runtime.GOOS == "windows" {
		env = append(env, "USERPROFILE="+home)
	}
	return append(env, adcCredentialsEnv()...), nil
}

// agyFileTokenName is the file agy stores its sign-in in, inside its config
// directory, when it does not use the OS keyring.
const agyFileTokenName = "antigravity-oauth-token" //nolint:gosec // a file name, not a credential

// linkFileToken links home/.gemini/antigravity-cli/antigravity-oauth-token to
// the user's file token, when there is one. Only a regular file is linked: a
// symlinked token is refused, as settings.json is, rather than followed to
// wherever it points. The link is to the file, so there is no ".." to walk
// back into the real home, and agy's own config directory, which holds the
// title command, is not reachable through it. Where a symlink cannot be made
// (Windows without the privilege), the token is copied instead; the isolated
// home is removed when the run ends.
func linkFileToken(home string) error {
	root, err := openAgyConfigRoot(false)
	if err != nil {
		return nil // no agy config directory: no file token to carry
	}
	defer root.Close()
	info, err := root.Lstat(agyFileTokenName)
	if err != nil || !info.Mode().IsRegular() {
		return nil // absent, or not a regular file: agy reports the missing sign-in
	}
	configDir, err := agyConfigDir()
	if err != nil {
		return nil // the root above resolved it; nothing to link if it no longer does
	}
	dir := filepath.Join(home, ".gemini", "antigravity-cli")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create isolated home: %w", err)
	}
	dst := filepath.Join(dir, agyFileTokenName)
	if err := os.Symlink(filepath.Join(configDir, agyFileTokenName), dst); err == nil {
		return nil
	}
	data, err := osroot.ReadFileNoFollow(root, agyFileTokenName)
	if err != nil {
		return fmt.Errorf("read agy file token: %w", err)
	}
	if err := os.WriteFile(dst, data, 0o600); err != nil {
		return fmt.Errorf("copy agy file token into isolated home: %w", err)
	}
	return nil
}

// adcCredentialsEnvVar names the application-default credentials file for
// Google client libraries, agy among them.
const adcCredentialsEnvVar = "GOOGLE_APPLICATION_CREDENTIALS" //nolint:gosec // a variable name, not a credential

// adcCredentialsFile is where gcloud writes application-default credentials
// under a Unix home, and where agy looks for them when
// GOOGLE_APPLICATION_CREDENTIALS is unset.
var adcCredentialsFile = filepath.Join(".config", "gcloud", "application_default_credentials.json")

// adcCredentialsEnv points GOOGLE_APPLICATION_CREDENTIALS at the user's
// application-default credentials, which agy would otherwise look for under
// the isolated home. A variable the user already set is inherited as it is,
// and Windows needs nothing because agy finds the file through %APPDATA%.
func adcCredentialsEnv() []string {
	if runtime.GOOS == "windows" || os.Getenv(adcCredentialsEnvVar) != "" {
		return nil
	}
	realHome, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	path := filepath.Join(realHome, adcCredentialsFile)
	if info, err := os.Stat(path); err != nil || !info.Mode().IsRegular() {
		return nil
	}
	return []string{adcCredentialsEnvVar + "=" + path}
}

// linkLoginKeychain links home/Library/Keychains/login.keychain-db to the
// user's login keychain. The link is to the file, not the Keychains
// directory: a directory link would make home/Library/Keychains/../.. the
// real home, while a file has no ".." to walk. The keychain file is
// encrypted and every read goes through securityd, as it does when agy runs
// normally. A user without a resolvable home or login keychain gets no link,
// and agy reports that it is not signed in.
func linkLoginKeychain(home string) error {
	realHome, err := os.UserHomeDir()
	if err != nil {
		return nil //nolint:nilerr // no home to link from: agy reports the missing sign-in
	}
	keychain := filepath.Join(realHome, "Library", "Keychains", "login.keychain-db")
	if _, err := os.Stat(keychain); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("stat login keychain: %w", err)
	}
	dir := filepath.Join(home, "Library", "Keychains")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create isolated home: %w", err)
	}
	if err := os.Symlink(keychain, filepath.Join(dir, "login.keychain-db")); err != nil {
		return fmt.Errorf("link login keychain into isolated home: %w", err)
	}
	return nil
}

// apiKeyModelProvider is the settings.json value that selects agy's Gemini
// API-key mode, without which agy refuses to start on an API key alone.
const apiKeyModelProvider = "gemini"

// carryAPIKeyMode writes {"modelProvider":"gemini"} into the isolated home
// when the user's own agy settings select API-key mode, so the run
// authenticates the way agy does for that user. Nothing else is carried: the
// same file names the window-title command agy executes. The value is matched
// against the one documented provider rather than copied, and an unreadable
// or malformed file selects nothing, leaving agy to report the missing sign-in.
func carryAPIKeyMode(home string) error {
	if !userSelectsAPIKeyMode() {
		return nil
	}
	dir := filepath.Join(home, ".gemini", "antigravity-cli")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create isolated home: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, agySettingsFileName), []byte(`{"modelProvider":"`+apiKeyModelProvider+`"}`), 0o600); err != nil {
		return fmt.Errorf("write isolated agy settings: %w", err)
	}
	return nil
}

// userSelectsAPIKeyMode reports whether the user's agy settings.json selects
// the Gemini API-key provider, read the way the title installer reads it (a
// symlinked file is refused, not read through).
func userSelectsAPIKeyMode() bool {
	settings, err := readAgySettings()
	if err != nil {
		return false
	}
	var provider string
	if err := json.Unmarshal(settings["modelProvider"], &provider); err != nil {
		return false
	}
	return provider == apiKeyModelProvider
}
