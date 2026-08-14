package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/auth"
	"github.com/entireio/cli/cmd/entire/cli/versioninfo"

	"github.com/stretchr/testify/require"
)

// setDoctorVersion overrides the package version var for the duration of a
// test so the CLI-version doctor check exercises its non-dev-build paths
// (the default "dev" sentinel skips the update fetch entirely).
func setDoctorVersion(t *testing.T, v string) {
	t.Helper()
	orig := versioninfo.Version
	versioninfo.Version = v
	t.Cleanup(func() { versioninfo.Version = orig })
}

// stubDoctorUpdateCheck overrides the version-fetch seam used by the CLI
// version doctor check so tests don't reach the network.
func stubDoctorUpdateCheck(t *testing.T, latest string, outdated bool, err error) {
	t.Helper()
	orig := checkForUpdate
	checkForUpdate = func(context.Context, string) (string, bool, error) {
		return latest, outdated, err
	}
	t.Cleanup(func() { checkForUpdate = orig })
}

func TestRunDoctorChecks_HealthyRepo(t *testing.T) {
	dir := setupGitRepoForPhaseTest(t)
	t.Chdir(dir)

	cmd, stdout := newTestCmd(t)
	issues := runDoctorChecks(cmd)

	out := stdout.String()
	require.Contains(t, out, "Entire Doctor")
	require.Contains(t, out, "[✓] Entire CLI: dev (dev build — update check skipped)")
	require.Contains(t, out, "[✓] Git: git version")
	require.Contains(t, out, "[✓] Repository: ")
	require.Contains(t, out, "(Entire enabled)")
	require.Contains(t, out, "[!] Agent hooks: none installed (run 'entire enable' or 'entire agent add <name>')")
	require.Contains(t, out, "[✓] Configuration: valid")
	require.Contains(t, out, "[!] Authentication: not logged in (run 'entire login')")
	require.Contains(t, out, "Doctor found 2 issue(s)")
	require.Equal(t, 2, issues)
}

func TestRunDoctorChecks_NotInARepo(t *testing.T) {
	// t.TempDir() is not a git repository.
	t.Chdir(t.TempDir())

	cmd, stdout := newTestCmd(t)
	issues := runDoctorChecks(cmd)

	out := stdout.String()
	require.Contains(t, out, "[!] Repository: not inside a git repository (run 'entire enable' from a repo)")
	// Outside a repo, settings still load with defaults, so configuration
	// itself stays valid.
	require.Contains(t, out, "[✓] Configuration: valid")
	require.Equal(t, 3, issues) // repository + agent hooks + authentication
}

func TestRunDoctorChecks_UpdateAvailable(t *testing.T) {
	dir := setupGitRepoForPhaseTest(t)
	t.Chdir(dir)

	setDoctorVersion(t, "0.5.0")
	stubDoctorUpdateCheck(t, "v9.9.9", true, nil)

	cmd, stdout := newTestCmd(t)
	issues := runDoctorChecks(cmd)

	out := stdout.String()
	require.Contains(t, out, "[!] Entire CLI: 0.5.0 (update available: 9.9.9")
	require.Contains(t, out, "run '")
	require.GreaterOrEqual(t, issues, 1)
}

func TestRunDoctorChecks_UpdateCheckError(t *testing.T) {
	dir := setupGitRepoForPhaseTest(t)
	t.Chdir(dir)

	setDoctorVersion(t, "0.5.0")
	stubDoctorUpdateCheck(t, "", false, errors.New("network down"))

	cmd, stdout := newTestCmd(t)
	issues := runDoctorChecks(cmd)

	out := stdout.String()
	require.Contains(t, out, "[!] Entire CLI: 0.5.0 (could not check for updates: network down)")
	require.GreaterOrEqual(t, issues, 1)
}

func TestRunDoctorChecks_UpToDate(t *testing.T) {
	dir := setupGitRepoForPhaseTest(t)
	t.Chdir(dir)

	setDoctorVersion(t, "0.5.0")
	stubDoctorUpdateCheck(t, "v0.5.0", false, nil)

	cmd, stdout := newTestCmd(t)
	issues := runDoctorChecks(cmd)

	out := stdout.String()
	require.Contains(t, out, "[✓] Entire CLI: 0.5.0 (up to date)")
	require.Equal(t, 2, issues) // repository-only issues: agent hooks + auth
}

func TestRunDoctorChecks_DisabledRepo(t *testing.T) {
	dir := setupGitRepoForPhaseTest(t)
	t.Chdir(dir)

	require.NoError(t, os.MkdirAll(filepath.Join(dir, ".entire"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".entire", "settings.json"), []byte(`{"enabled": false}`), 0o600))

	cmd, stdout := newTestCmd(t)
	issues := runDoctorChecks(cmd)

	out := stdout.String()
	require.Contains(t, out, "[!] Repository: ")
	require.Contains(t, out, "(Entire is disabled — run 'entire enable')")
	require.Contains(t, out, "[✓] Configuration: valid")
	require.Equal(t, 3, issues) // disabled + agent hooks + auth
}

func TestRunDoctorChecks_InvalidSettings(t *testing.T) {
	dir := setupGitRepoForPhaseTest(t)
	t.Chdir(dir)

	require.NoError(t, os.MkdirAll(filepath.Join(dir, ".entire"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".entire", "settings.json"), []byte(`{not json`), 0o600))

	cmd, stdout := newTestCmd(t)
	issues := runDoctorChecks(cmd)

	out := stdout.String()
	require.Contains(t, out, "[✗] Repository: ")
	require.Contains(t, out, "(could not load settings:")
	require.Contains(t, out, "[✗] Configuration: invalid —")
	require.Contains(t, out, "Fix the JSON in .entire/settings.json or .entire/settings.local.json")
	require.Equal(t, 4, issues) // repo + config + agent hooks + auth
}

func TestRunDoctorChecks_InvalidLogLevel(t *testing.T) {
	dir := setupGitRepoForPhaseTest(t)
	t.Chdir(dir)

	require.NoError(t, os.MkdirAll(filepath.Join(dir, ".entire"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".entire", "settings.json"), []byte(`{"enabled": true, "log_level": "bogus"}`), 0o600))

	cmd, stdout := newTestCmd(t)
	issues := runDoctorChecks(cmd)

	out := stdout.String()
	require.Contains(t, out, `[!] Configuration: log_level "bogus" is not one of debug/info/warn/error`)
	require.Equal(t, 3, issues) // log level + agent hooks + auth
}

func TestRunDoctorChecks_AgentCLIMissing(t *testing.T) {
	dir := setupGitRepoForPhaseTest(t)
	codexDir := filepath.Join(dir, ".codex")
	require.NoError(t, os.MkdirAll(codexDir, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(codexDir, "hooks.json"), []byte(canonicalCodexHooksJSON()), 0o600))
	t.Chdir(dir)

	// Wipe PATH so no agent CLI resolves — the missing-binary finding is
	// deterministic regardless of what's installed on the host.
	t.Setenv("PATH", t.TempDir())

	cmd, stdout := newTestCmd(t)
	issues := runDoctorChecks(cmd)

	out := stdout.String()
	require.Contains(t, out, "Agent hooks installed but agent CLI missing: codex (CLI 'codex' not found on PATH)")
	require.NotContains(t, out, "CLI found")
	require.GreaterOrEqual(t, issues, 1)
}

func TestRunDoctorChecks_AgentCLIFound(t *testing.T) {
	dir := setupGitRepoForPhaseTest(t)
	codexDir := filepath.Join(dir, ".codex")
	require.NoError(t, os.MkdirAll(codexDir, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(codexDir, "hooks.json"), []byte(canonicalCodexHooksJSON()), 0o600))
	t.Chdir(dir)

	// Fake a codex binary on PATH so the CLI-presence check passes.
	binDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(binDir, "codex"), []byte("#!/bin/sh\nexit 0\n"), 0o755))
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	cmd, stdout := newTestCmd(t)
	issues := runDoctorChecks(cmd)

	out := stdout.String()
	require.Contains(t, out, "[✓] Agent hooks: codex (CLI found)")
	require.NotContains(t, out, "not found on PATH")
	require.Equal(t, 1, issues) // agent hooks OK; auth is the remaining finding
}

func TestRunDoctorChecks_LoggedIn(t *testing.T) {
	dir := setupGitRepoForPhaseTest(t)
	t.Chdir(dir)

	configDir := t.TempDir()
	contextsJSON := `{"current_context":"dev","contexts":[{"name":"dev","core_url":"https://core.example","handle":"alice","keychain_service":"entire-core:https://core.example"}]}`
	require.NoError(t, os.WriteFile(filepath.Join(configDir, "contexts.json"), []byte(contextsJSON), 0o600))
	t.Setenv("ENTIRE_CONFIG_DIR", configDir)

	cmd, stdout := newTestCmd(t)
	issues := runDoctorChecks(cmd)

	out := stdout.String()
	require.Contains(t, out, "[✓] Authentication: logged in (1 context(s) saved)")
	require.Equal(t, 1, issues) // only agent hooks
}

func TestRunDoctorChecks_EnvToken(t *testing.T) {
	dir := setupGitRepoForPhaseTest(t)
	t.Chdir(dir)

	t.Setenv(auth.EnvTokenVar, "not.a.real.jwt")

	cmd, stdout := newTestCmd(t)
	issues := runDoctorChecks(cmd)

	out := stdout.String()
	require.Contains(t, out, "[✓] Authentication: ENTIRE_TOKEN environment variable set")
	require.Equal(t, 1, issues) // only agent hooks
}

func TestRunDoctorChecks_CorruptContexts(t *testing.T) {
	dir := setupGitRepoForPhaseTest(t)
	t.Chdir(dir)

	configDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(configDir, "contexts.json"), []byte(`{not json`), 0o600))
	t.Setenv("ENTIRE_CONFIG_DIR", configDir)

	cmd, stdout := newTestCmd(t)
	issues := runDoctorChecks(cmd)

	out := stdout.String()
	require.Contains(t, out, "[✗] Authentication: could not read login contexts:")
	require.Equal(t, 2, issues) // agent hooks + corrupt contexts
}
