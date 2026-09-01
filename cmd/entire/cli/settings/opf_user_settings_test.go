package settings

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/settings/repopolicy"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// These tests point ENTIRE_CONFIG_DIR at a per-test directory (t.Setenv), so
// they cannot run in parallel. The parallel tests in opf_command_trust_test.go
// read the shared testdirs config dir, which nobody writes.

// setUserSettingsFile isolates the user tier and writes body as
// ~/.config/entire/settings.json; an empty body leaves the file absent.
func setUserSettingsFile(t *testing.T, body string) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("ENTIRE_CONFIG_DIR", dir)
	if body != "" {
		require.NoError(t, os.WriteFile(filepath.Join(dir, repopolicy.UserSettingsFileName), []byte(body), 0o600))
	}
}

func userOPFSettings(command string) string {
	return `{"redaction":{"openai_privacy_filter":{"command":"` + command + `"}}}`
}

// The supported configuration going forward: the command lives in the user's
// own settings file, outside every repository, so nothing about the clone has
// to be verified before it is honored.
func TestOPFCommandTrust_UserFileCommandIsHonored(t *testing.T) {
	setUserSettingsFile(t, userOPFSettings(trustedCommand))
	_, project, local := newOPFRepo(t)
	writeSettingsFile(t, project, opfSettings(""))

	opf := loadedOPF(t, project, local)
	assert.Equal(t, trustedCommand, opf.Command)
	assert.Equal(t, OPFCommandSourceUser, opf.CommandSource())
	_, _, rejected := opf.CommandRejection()
	assert.False(t, rejected, "a user-file command must not be reported as rejected")
}

// The user file wins over the local file, and it wins without the ownership
// probe: a TRACKED local file — which the probe rejects and which drops the
// whole local layer — must make no difference to the user-file command.
func TestOPFCommandTrust_UserFileCommandWinsOverTrackedLocalCommand(t *testing.T) {
	setUserSettingsFile(t, userOPFSettings(trustedCommand))
	root, project, local := newOPFRepo(t)
	writeSettingsFile(t, project, opfSettings(""))
	writeSettingsFile(t, local, localOPFSettings(attackerCommand))
	testutil.RunGit(t, root, "add", "-f", EntireSettingsLocalFile)
	testutil.RunGit(t, root, "commit", "-m", "carry local settings")

	opf := loadedOPF(t, project, local)
	assert.Equal(t, trustedCommand, opf.Command, "the user file is not repository content; a tracked local file cannot displace it")
	assert.Equal(t, OPFCommandSourceUser, opf.CommandSource())
}

// A developer who has both set stays on the user file; the local value is the
// deprecated location and is superseded, not merged.
func TestOPFCommandTrust_UserFileCommandWinsOverUntrackedLocalCommand(t *testing.T) {
	setUserSettingsFile(t, userOPFSettings(trustedCommand))
	_, project, local := newOPFRepo(t)
	writeSettingsFile(t, project, opfSettings(""))
	writeSettingsFile(t, local, localOPFSettings("/elsewhere/opf"))

	opf := loadedOPF(t, project, local)
	assert.Equal(t, trustedCommand, opf.Command)
	assert.Equal(t, OPFCommandSourceUser, opf.CommandSource())
}

// Ownership of the settings file is one gate; the binary's location is the
// other. A user-file command that resolves inside the worktree is still a
// repo-deliverable executable and is rejected exactly like a local one.
func TestOPFCommandTrust_UserFileCommandInsideWorktreeIsRejected(t *testing.T) {
	root, project, local := newOPFRepo(t)
	setUserSettingsFile(t, userOPFSettings(filepath.ToSlash(filepath.Join(root, "tools", "opf"))))
	writeSettingsFile(t, project, opfSettings(""))

	opf := loadedOPF(t, project, local)
	assert.Empty(t, opf.Command)
	_, reason, rejected := opf.CommandRejection()
	assert.True(t, rejected)
	assert.Contains(t, reason, "worktree")
}

// The local-file path keeps working for one release, tagged so the pre-push
// consumer can point at the new location.
func TestOPFCommandTrust_LocalCommandIsTaggedAsDeprecatedSource(t *testing.T) {
	setUserSettingsFile(t, "")
	_, project, local := newOPFRepo(t)
	writeSettingsFile(t, project, opfSettings(""))
	writeSettingsFile(t, local, localOPFSettings(trustedCommand))

	opf := loadedOPF(t, project, local)
	assert.Equal(t, trustedCommand, opf.Command)
	assert.Equal(t, OPFCommandSourceLocal, opf.CommandSource())
}

// The user file says how OPF runs here, not whether it runs: with no OPF block
// in the repository's settings there is nothing to attach the command to, and
// the user file must not conjure one.
func TestOPFUserSettings_DoNotEnableOPFOnTheirOwn(t *testing.T) {
	setUserSettingsFile(t, userOPFSettings(trustedCommand))
	_, project, local := newOPFRepo(t)
	writeSettingsFile(t, project, `{"enabled":true}`)

	s, err := loadMergedSettings(t.Context(), project, "", local)
	require.NoError(t, err)
	assert.Nil(t, s.Redaction, "a user-file OPF block must not create a redaction block the repo never configured")
}

// timeout_seconds and prompt_default are ordinary configuration and layer
// between the project and the local file — project < user < local — so the
// pre-push prompt's "Always" (which writes prompt_default to the local file)
// still takes effect over a user-file "ask". Only command is user-wins.
func TestOPFUserSettings_TimeoutAndPromptLayerBetweenProjectAndLocal(t *testing.T) {
	setUserSettingsFile(t, `{"redaction":{"openai_privacy_filter":{"timeout_seconds":90,"prompt_default":"never"}}}`)
	_, project, local := newOPFRepo(t)
	writeSettingsFile(t, project, opfSettings("")) // prompt_default "always", no timeout

	opf := loadedOPF(t, project, local)
	assert.Equal(t, 90, opf.TimeoutSeconds, "user file beats the project file")
	assert.Equal(t, OPFPromptNever, opf.PromptDefault, "user file beats the project file")

	writeSettingsFile(t, local, `{"redaction":{"openai_privacy_filter":{"prompt_default":"always"}}}`)
	opf = loadedOPF(t, project, local)
	assert.Equal(t, OPFPromptAlways, opf.PromptDefault, "the local file beats the user file")
	assert.Equal(t, 90, opf.TimeoutSeconds, "keys the local file does not set keep the user value")
}

// A developer enabling OPF only for themselves creates the OPF block from the
// local file alone. The user file's keys must still layer under it — the
// overlay runs after the local merge and yields only to keys the local file
// set explicitly.
func TestOPFUserSettings_LayerUnderALocalFileThatCreatesTheBlock(t *testing.T) {
	setUserSettingsFile(t, `{"redaction":{"openai_privacy_filter":{"timeout_seconds":90,"prompt_default":"never"}}}`)
	_, project, local := newOPFRepo(t)
	writeSettingsFile(t, project, `{"enabled":true}`)
	writeSettingsFile(t, local, `{"redaction":{"openai_privacy_filter":{"enabled":true,"prompt_default":"always"}}}`)

	opf := loadedOPF(t, project, local)
	assert.True(t, opf.Enabled)
	assert.Equal(t, 90, opf.TimeoutSeconds, "the user file fills a key the local file left unset")
	assert.Equal(t, OPFPromptAlways, opf.PromptDefault, "the local file keeps a key it set explicitly")
}

// A malformed user file is the user's own problem to fix and turns the global
// tier off; it must never take repository settings down with it. The overlay
// is skipped and the pre-existing local path still applies.
func TestOPFUserSettings_MalformedUserFileIsSkipped(t *testing.T) {
	setUserSettingsFile(t, `{not json`)
	_, project, local := newOPFRepo(t)
	writeSettingsFile(t, project, opfSettings(""))
	writeSettingsFile(t, local, localOPFSettings(trustedCommand))

	opf := loadedOPF(t, project, local)
	assert.Equal(t, trustedCommand, opf.Command)
	assert.Equal(t, OPFCommandSourceLocal, opf.CommandSource())
}
