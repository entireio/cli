package settings

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// loadGlobalOnlyCase writes the committed and local settings files (either may
// be empty, meaning absent), tracks the local file when asked, and loads the
// merged settings. "ROOT" in the user file is replaced with the quoted
// worktree root so a row can key a repos entry by path.
func loadGlobalOnlyCase(t *testing.T, committed, local string, trackLocal bool, user string) *EntireSettings {
	t.Helper()
	root := newNeverEnabledRepo(t)
	project := filepath.Join(root, EntireSettingsFile)
	localPath := filepath.Join(root, EntireSettingsLocalFile)
	if committed != "" || local != "" {
		require.NoError(t, os.MkdirAll(filepath.Join(root, ".entire"), 0o755))
	}
	if committed != "" {
		require.NoError(t, os.WriteFile(project, []byte(committed), 0o644))
	}
	if local != "" {
		require.NoError(t, os.WriteFile(localPath, []byte(local), 0o644))
		if trackLocal {
			testutil.RunGit(t, root, "add", "-f", EntireSettingsLocalFile)
		}
	}
	if user != "" {
		writeUserSettings(t, strings.ReplaceAll(user, "ROOT", jsonString(root)))
	}
	s, err := loadMergedSettings(t.Context(), project, "", localPath)
	require.NoError(t, err)
	return s
}

// A repository is enabled only by global enable when global_enable is on, the
// result is enabled, and nothing the developer wrote for this repository says
// so: no matching repos entry, and no honored local file carrying `enabled`.
// The committed file never counts — anyone can commit it. Neither do the local
// keys Entire writes on its own (commit_linking and the like), which would
// otherwise make the first commit prompt read as the developer opting in.
func TestEnabledOnlyByGlobal(t *testing.T) {
	const global = `{"global_enable": true}`
	cases := map[string]struct {
		committed, local string
		trackLocal       bool
		user             string
		wantGlobalOnly   bool
		wantEnabled      bool
	}{
		"no files":                    {user: global, wantGlobalOnly: true, wantEnabled: true},
		"committed enabled only":      {committed: `{"enabled": true}`, user: global, wantGlobalOnly: true, wantEnabled: true},
		"local enabled true":          {local: `{"enabled": true}`, user: global, wantEnabled: true},
		"local enabled false":         {local: `{"enabled": false}`, user: global},
		"local without enabled":       {local: `{"commit_linking": "always"}`, user: global, wantGlobalOnly: true, wantEnabled: true},
		"tracked local enabled":       {local: `{"enabled": true}`, trackLocal: true, user: global, wantGlobalOnly: true, wantEnabled: true},
		"repos entry":                 {user: `{"global_enable": true, "repos": {ROOT: {"log_level": "debug"}}}`, wantEnabled: true},
		"repos entry null":            {user: `{"global_enable": true, "repos": {ROOT: null}}`, wantGlobalOnly: true, wantEnabled: true},
		"repos entry fails to decode": {user: `{"global_enable": true, "repos": {ROOT: {"nope": 1}}}`, wantGlobalOnly: true, wantEnabled: true},
		"repos enabled false":         {user: `{"global_enable": true, "repos": {ROOT: {"enabled": false}}}`},
		"committed enabled false":     {committed: `{"enabled": false}`, user: global},
		"global off":                  {committed: `{"enabled": true}`, user: `{"global_enable": false}`, wantEnabled: true},
		"no user file":                {committed: `{"enabled": true}`, wantEnabled: true},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			s := loadGlobalOnlyCase(t, tc.committed, tc.local, tc.trackLocal, tc.user)
			assert.Equal(t, tc.wantGlobalOnly, s.EnabledOnlyByGlobal(), "EnabledOnlyByGlobal")
			assert.Equal(t, tc.wantEnabled, s.Enabled, "Enabled")
		})
	}
}

func TestEnabledOnlyByGlobal_NilSafe(t *testing.T) {
	t.Parallel()
	var s *EntireSettings
	assert.False(t, s.EnabledOnlyByGlobal())
	assert.Nil(t, s.CommittedKeysIgnoredForGlobalEnable())
}

// Committed enabled:false is a floor: global enable turns on repositories
// nobody configured, not ones whose own team turned Entire off. A repos entry
// set by this developer can still raise it.
func TestGlobalEnable_CommittedFalseIsAFloor(t *testing.T) {
	s := loadGlobalOnlyCase(t, `{"enabled": false}`, "", false, `{"global_enable": true}`)
	assert.False(t, s.Enabled)

	s = loadGlobalOnlyCase(t, `{"enabled": false}`, "", false,
		`{"global_enable": true, "repos": {ROOT: {"enabled": true}}}`)
	assert.True(t, s.Enabled, "the developer's own repos entry still raises a committed false")
	assert.False(t, s.EnabledOnlyByGlobal())
}

const committedWithIgnorableKeys = `{
  "enabled": true,
  "telemetry": true,
  "summary_generation": {"provider": "codex", "model": "gpt-5"},
  "summary_timeout_seconds": 30,
  "strategy_options": {"summarize": {"enabled": true}, "push_sessions": false},
  "redaction": {"custom_redactions": {"codename": "falcon-[0-9]+"}}
}`

// In a repository enabled only through global enable, the committed file must
// not switch on summarization (which sends transcripts to a model provider)
// or telemetry on the developer's behalf: they never opted in here. Redaction
// is untouched — it only ever removes data.
func TestGlobalOnly_IgnoresCommittedSummarizeAndTelemetry(t *testing.T) {
	s := loadGlobalOnlyCase(t, committedWithIgnorableKeys, "", false, `{"global_enable": true}`)
	require.True(t, s.EnabledOnlyByGlobal())

	assert.False(t, s.IsSummarizeEnabled())
	assert.Nil(t, s.SummaryGeneration)
	assert.Zero(t, s.SummaryTimeoutSeconds)
	assert.Nil(t, s.Telemetry)
	assert.Equal(t, false, s.StrategyOptions["push_sessions"], "other strategy options still apply")
	require.NotNil(t, s.Redaction)
	assert.Equal(t, map[string]string{"codename": "falcon-[0-9]+"}, s.Redaction.CustomRedactions)
	assert.ElementsMatch(t,
		[]string{"strategy_options.summarize", "summary_generation", "summary_timeout_seconds", "telemetry"},
		s.CommittedKeysIgnoredForGlobalEnable())
}

// A committed telemetry:false is kept: it only ever turns something off.
func TestGlobalOnly_KeepsACommittedTelemetryFalse(t *testing.T) {
	s := loadGlobalOnlyCase(t, `{"enabled": true, "telemetry": false}`, "", false, `{"global_enable": true}`)
	require.NotNil(t, s.Telemetry)
	assert.False(t, *s.Telemetry)
	assert.Empty(t, s.CommittedKeysIgnoredForGlobalEnable())
}

// The developer's own layers still apply after the committed keys are set
// aside, including in a global-only repository.
func TestGlobalOnly_UserPreferencesStillApply(t *testing.T) {
	s := loadGlobalOnlyCase(t, committedWithIgnorableKeys, "", false,
		`{"global_enable": true, "preferences": {"summary_generation": {"provider": "claude-code"}, "telemetry": true}}`)
	require.True(t, s.EnabledOnlyByGlobal())
	require.NotNil(t, s.SummaryGeneration)
	assert.Equal(t, "claude-code", s.SummaryGeneration.Provider)
	require.NotNil(t, s.Telemetry)
	assert.True(t, *s.Telemetry)
}

// Once the developer has enabled the repository themselves, or with global
// enable off, the committed file applies in full.
func TestGlobalOnly_CommittedKeysApplyOnceClaimedOrGlobalOff(t *testing.T) {
	for name, tc := range map[string]struct{ local, user string }{
		"claimed":    {local: `{"enabled": true}`, user: `{"global_enable": true}`},
		"global off": {user: `{"global_enable": false}`},
		"no user":    {},
	} {
		t.Run(name, func(t *testing.T) {
			s := loadGlobalOnlyCase(t, committedWithIgnorableKeys, tc.local, false, tc.user)
			assert.False(t, s.EnabledOnlyByGlobal())
			assert.True(t, s.IsSummarizeEnabled())
			require.NotNil(t, s.SummaryGeneration)
			assert.Equal(t, "codex", s.SummaryGeneration.Provider)
			assert.Equal(t, 30, s.SummaryTimeoutSeconds)
			require.NotNil(t, s.Telemetry)
			assert.True(t, *s.Telemetry)
			assert.Empty(t, s.CommittedKeysIgnoredForGlobalEnable())
		})
	}
}

// Claiming and disabling write the worktree's own local file, so they are
// per-worktree: a linked worktree of a claimed repository is still enabled
// only through global enable. A repos entry is the clone-wide claim.
func TestGlobalOnly_ClaimIsPerWorktree(t *testing.T) {
	main := newNeverEnabledRepo(t)
	require.NoError(t, os.WriteFile(filepath.Join(main, "f.txt"), []byte("x"), 0o644))
	testutil.RunGit(t, main, "add", ".")
	testutil.RunGit(t, main, "commit", "-m", "init")
	require.NoError(t, os.MkdirAll(filepath.Join(main, ".entire"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(main, EntireSettingsLocalFile), []byte(`{"enabled": true}`), 0o644))
	writeUserSettings(t, `{"global_enable": true}`)

	linked := filepath.Join(t.TempDir(), "linked")
	testutil.RunGit(t, main, "worktree", "add", "-b", "linked-work", linked)

	load := func(root string) *EntireSettings {
		s, err := loadMergedSettings(t.Context(), filepath.Join(root, EntireSettingsFile), "",
			filepath.Join(root, EntireSettingsLocalFile))
		require.NoError(t, err)
		return s
	}
	assert.False(t, load(main).EnabledOnlyByGlobal())
	assert.True(t, load(linked).EnabledOnlyByGlobal())
}
