package settings

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/settings/usersettings"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/entireio/cli/internal/entireclient/userdirs"
)

// newUserTierRepo builds a worktree with a project settings file and points
// the user config directory at a temp dir. It returns the project and local
// settings paths loadMergedSettings takes, and the worktree root.
//
// t.Setenv makes these tests process-global, so none of them call t.Parallel.
func newUserTierRepo(t *testing.T) (root, project, local string) {
	t.Helper()
	root = t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, ".entire"), 0o755))
	project = filepath.Join(root, EntireSettingsFile)
	local = filepath.Join(root, EntireSettingsLocalFile)
	require.NoError(t, os.WriteFile(project, []byte(`{"enabled":true}`), 0o644))

	t.Setenv(userdirs.EnvConfigDir, t.TempDir())
	t.Cleanup(ClearOriginKeyCache)
	return root, project, local
}

func writeUserSettings(t *testing.T, content string) {
	t.Helper()
	dir := os.Getenv(userdirs.EnvConfigDir)
	require.NotEmpty(t, dir, "newUserTierRepo must run first")
	require.NoError(t, os.WriteFile(filepath.Join(dir, usersettings.FileName), []byte(content), 0o600))
}

func TestUserTier_MachineWidePreferencesApply(t *testing.T) {
	_, project, local := newUserTierRepo(t)
	writeUserSettings(t, `{"preferences": {"telemetry": true, "log_level": "debug", "review_fix_agent": "codex"}}`)

	s, err := loadMergedSettings(t.Context(), project, "", local)
	require.NoError(t, err)

	require.NotNil(t, s.Telemetry)
	assert.True(t, *s.Telemetry)
	assert.Equal(t, "debug", s.LogLevel)
	assert.Equal(t, "codex", s.ReviewFixAgent)
	assert.Empty(t, s.UserLayerRejections())
}

// A repos entry keyed by normalized origin applies only to the repository that
// origin names. This is the mechanism that lets one machine-wide file carry
// per-repository settings without a file inside any repository.
func TestUserTier_ReposEntryMatchesByOrigin(t *testing.T) {
	root, project, local := newUserTierRepo(t)
	testutil.InitRepo(t, root)
	testutil.RunGit(t, root, "remote", "add", "origin", "https://github.com/acme/widgets.git")

	writeUserSettings(t, `{
	  "preferences": {"review_fix_agent": "claude-code"},
	  "repos": {
	    "gh/acme/widgets": {"review_fix_agent": "codex"},
	    "gh/other/thing":  {"review_fix_agent": "gemini"}
	  }
	}`)

	s, err := loadMergedSettings(t.Context(), project, "", local)
	require.NoError(t, err)
	assert.Equal(t, "codex", s.ReviewFixAgent,
		"this repository's entry must win over the machine-wide default")
}

func TestUserTier_ReposEntryForAnotherRepoDoesNotApply(t *testing.T) {
	root, project, local := newUserTierRepo(t)
	testutil.InitRepo(t, root)
	testutil.RunGit(t, root, "remote", "add", "origin", "https://github.com/acme/widgets.git")

	writeUserSettings(t, `{
	  "preferences": {"review_fix_agent": "claude-code"},
	  "repos": {"gh/other/thing": {"review_fix_agent": "gemini"}}
	}`)

	s, err := loadMergedSettings(t.Context(), project, "", local)
	require.NoError(t, err)
	assert.Equal(t, "claude-code", s.ReviewFixAgent,
		"a repos entry naming a different origin must not apply here")
}

// A repository with no usable origin keys by absolute worktree path, so the
// mechanism still reaches repositories that have no remote at all.
func TestUserTier_ReposEntryMatchesByPath(t *testing.T) {
	root, project, local := newUserTierRepo(t)
	testutil.InitRepo(t, root)

	writeUserSettings(t, `{"repos": {`+jsonString(root)+`: {"review_fix_agent": "codex"}}}`)

	s, err := loadMergedSettings(t.Context(), project, "", local)
	require.NoError(t, err)
	assert.Equal(t, "codex", s.ReviewFixAgent)
}

// The allowlist is the boundary. An unknown key drops that block alone, with a
// reason recorded — it must never fail the load, because a mistyped review
// preference would then take the whole repository's settings down with it.
func TestUserTier_UnknownKeyDropsOnlyThatBlock(t *testing.T) {
	_, project, local := newUserTierRepo(t)
	writeUserSettings(t, `{"preferences": {"external_agents": true, "telemetry": true}}`)

	s, err := loadMergedSettings(t.Context(), project, "", local)
	require.NoError(t, err, "a bad preference block must not fail the load")

	assert.Nil(t, s.Telemetry, "the whole block is dropped, not merged in part")
	require.Len(t, s.UserLayerRejections(), 1)
	assert.Contains(t, s.UserLayerRejections()[0], "preferences")
	assert.True(t, s.Enabled, "the repository's own settings still applied")
}

// The user tier outranks committed project settings, while the per-worktree
// local file remains the final override.
func TestUserTier_OutranksProjectButLocalWins(t *testing.T) {
	_, project, local := newUserTierRepo(t)
	require.NoError(t, os.WriteFile(project, []byte(`{"enabled":true,"review_fix_agent":"from-project"}`), 0o644))
	writeUserSettings(t, `{"preferences": {"review_fix_agent": "from-user"}}`)

	s, err := loadMergedSettings(t.Context(), project, "", local)
	require.NoError(t, err)
	assert.Equal(t, "from-user", s.ReviewFixAgent, "the user tier outranks the committed project file")

	require.NoError(t, os.WriteFile(local, []byte(`{"review_fix_agent":"from-local"}`), 0o644))
	s, err = loadMergedSettings(t.Context(), project, "", local)
	require.NoError(t, err)
	assert.Equal(t, "from-local", s.ReviewFixAgent,
		"the per-worktree local file remains the final override")
}

// A repos entry and a machine-wide preference can both name a key; the
// repository-specific one wins until the per-worktree local file overrides it.
func TestUserTier_CheckpointRemoteAndEnabledReachEveryWorktree(t *testing.T) {
	root, project, local := newUserTierRepo(t)
	testutil.InitRepo(t, root)
	testutil.RunGit(t, root, "remote", "add", "origin", "https://github.com/acme/widgets.git")

	// The shape that bites today: the committed file names an upstream store,
	// and the developer's own choice used to live in a per-worktree file.
	require.NoError(t, os.WriteFile(project, []byte(
		`{"enabled":true,"strategy_options":{"checkpoint_remote":{"provider":"github","repo":"upstream/app-checkpoints"}}}`), 0o644))
	writeUserSettings(t, `{
	  "repos": {"gh/acme/widgets": {
	    "enabled": true,
	    "checkpoint_remote": {"provider": "github", "repo": "mydev/my-checkpoints"}
	  }}
	}`)

	s, err := loadMergedSettings(t.Context(), project, "", local)
	require.NoError(t, err)

	cr := s.GetCheckpointRemote()
	require.NotNil(t, cr, "a user-tier destination must decode through the existing reader")
	assert.Equal(t, "mydev/my-checkpoints", cr.Repo)
	assert.True(t, s.Enabled, "the user tier configures a worktree without a local override")

	require.NoError(t, os.WriteFile(local, []byte(
		`{"enabled":false,"strategy_options":{"checkpoint_remote":{"provider":"github","repo":"local/checkpoints"}}}`), 0o644))
	s, err = loadMergedSettings(t.Context(), project, "", local)
	require.NoError(t, err)
	assert.False(t, s.Enabled, "the per-worktree local file remains the final override")
	cr = s.GetCheckpointRemote()
	require.NotNil(t, cr)
	assert.Equal(t, "local/checkpoints", cr.Repo,
		"the per-worktree destination remains the final override")
}

// The claim that a user file without a repos block costs no git reads. With
// git unreachable, a load carrying only machine-wide preferences must still
// succeed and apply them.
func TestUserTier_NoReposBlockNeedsNoGit(t *testing.T) {
	_, project, local := newUserTierRepo(t)
	writeUserSettings(t, `{"preferences": {"review_fix_agent": "codex"}}`)
	t.Setenv("PATH", "")

	s, err := loadMergedSettings(t.Context(), project, "", local)
	require.NoError(t, err)
	assert.Equal(t, "codex", s.ReviewFixAgent,
		"machine-wide preferences must not depend on resolving a repository")
}

// A missing user settings file is an unconfigured tier, not a failure.
func TestUserTier_AbsentFileChangesNothing(t *testing.T) {
	_, project, local := newUserTierRepo(t)

	s, err := loadMergedSettings(t.Context(), project, "", local)
	require.NoError(t, err)
	assert.True(t, s.Enabled)
	assert.Empty(t, s.UserLayerRejections())
}

// jsonString quotes a path for embedding in a JSON object key.
func jsonString(s string) string {
	return `"` + filepath.ToSlash(s) + `"`
}

// Symptom A, directly. A linked worktree of a repository configured only
// outside the worktree has no .entire directory of its own: `git worktree add`
// copies no untracked file, and no hook fires when a worktree is created. Its
// git hooks are installed and firing regardless, because they live in the git
// common dir every worktree shares — so if IsSetUpAny answers from the two
// .entire files alone, every one of those hooks is a silent no-op and all
// capture is dropped in that tree.
func TestIsSetUpAny_ReachesTheUserTierInAWorktreeWithNoEntireDir(t *testing.T) {
	root := t.TempDir()
	testutil.InitRepo(t, root)
	testutil.RunGit(t, root, "remote", "add", "origin", "https://github.com/acme/widgets.git")
	t.Setenv(userdirs.EnvConfigDir, t.TempDir())
	t.Cleanup(ClearOriginKeyCache)
	t.Chdir(root)

	require.NoDirExists(t, filepath.Join(root, ".entire"),
		"the premise: a freshly added worktree has no .entire of its own")

	assert.False(t, IsSetUpAny(t.Context()),
		"sanity: with nothing configured anywhere, this repository is not set up")

	writeUserSettings(t, `{"repos": {"gh/acme/widgets": {"enabled": true}}}`)
	ClearOriginKeyCache()

	assert.True(t, IsSetUpAny(t.Context()),
		"a repository configured in the user file is set up in every worktree, including one with no .entire")
}

// The converse: a user file that says nothing about THIS repository must not
// make it read as set up, or enabling one repository would enable every
// repository on the machine.
func TestIsSetUpAny_IgnoresAUserEntryForAnotherRepo(t *testing.T) {
	root := t.TempDir()
	testutil.InitRepo(t, root)
	testutil.RunGit(t, root, "remote", "add", "origin", "https://github.com/acme/widgets.git")
	t.Setenv(userdirs.EnvConfigDir, t.TempDir())
	t.Cleanup(ClearOriginKeyCache)
	t.Chdir(root)

	writeUserSettings(t, `{"repos": {"gh/other/thing": {"enabled": true}}}`)

	assert.False(t, IsSetUpAny(t.Context()),
		"an entry naming a different origin must not activate this repository")
}

// Instruction fields set in the user settings file must survive the agent
// prompt gate. That gate drops any field it cannot attribute to a
// developer-owned layer, and it knew about two: a verified settings.local.json
// and clone preferences. The user file is developer-owned by a stronger
// argument than either — a repository cannot deliver content to ~/.config —
// but an unrecognised source is indistinguishable from an untrusted one, so
// these were dropped with a reason naming two files the developer never used.
func TestUserTier_InstructionFieldsSurviveTheAgentPromptGate(t *testing.T) {
	_, project, local := newUserTierRepo(t)
	writeUserSettings(t, `{
	  "preferences": {
	    "review_profiles": {"mine": {"task": "Only real defects.",
	      "agents": {"claude-code": {"prompt": "Be terse."}}}}
	  }
	}`)

	s, err := loadMergedSettings(t.Context(), project, "", local)
	require.NoError(t, err)

	assert.Empty(t, s.AgentPromptRejections(),
		"nothing from the user's own settings file may be reported as untrusted")
	profile := s.ReviewProfiles["mine"]
	assert.Equal(t, "Only real defects.", profile.Task)
	assert.Equal(t, "Be terse.", profile.Agents["claude-code"].Prompt)
}

// A local profile merges after the user tier, so its provenance must be
// checked before the lower user-owned profile. If the local file is still
// reachable from HEAD after git rm --cached, the deep check fails closed; the
// user tier must not launder the effective local instructions as user-owned.
func TestUserTier_UnverifiableLocalProfileFailsClosed(t *testing.T) {
	root, project, local := newUserTierRepo(t)
	testutil.InitRepo(t, root)
	writeUserSettings(t, `{
	  "preferences": {
	    "review_profiles": {"general": {
	      "task": "User task.",
	      "agents": {"codex": {"prompt": "User worker prompt."}},
	      "judge": {"agent": "claude-code", "prompt": "User judge prompt."}
	    }}
	  }
	}`)
	require.NoError(t, os.WriteFile(local, []byte(`{
	  "review_profiles": {"general": {
	    "task": "Local task.",
	    "agents": {"codex": {"prompt": "Local worker prompt."}},
	    "judge": {"agent": "claude-code", "prompt": "Local judge prompt."}
	  }}
	}`), 0o644))
	testutil.RunGit(t, root, "add", "-f", EntireSettingsLocalFile)
	testutil.RunGit(t, root, "commit", "-m", "carry local settings")
	testutil.RunGit(t, root, "rm", "--cached", EntireSettingsLocalFile)

	s, err := loadMergedSettings(t.Context(), project, "", local)
	require.NoError(t, err)

	profile := s.ReviewProfiles["general"]
	assert.Empty(t, profile.Task)
	assert.Empty(t, profile.Agents["codex"].Prompt)
	assert.Empty(t, profile.Judge.Prompt)
	require.Len(t, s.AgentPromptRejections(), 3)
	for _, rejection := range s.AgentPromptRejections() {
		assert.Equal(t, agentPromptRejectionUnverified, rejection.Reason)
	}
}

// A local file tracked in the index is discarded before merging, so the lower
// user-owned profile remains effective and trusted.
func TestUserTier_TrackedLocalProfileFallsBackToUserTier(t *testing.T) {
	root, project, local := newUserTierRepo(t)
	testutil.InitRepo(t, root)
	writeUserSettings(t, `{
	  "preferences": {
	    "review_profiles": {"general": {
	      "task": "User task.",
	      "agents": {"codex": {"prompt": "User worker prompt."}}
	    }}
	  }
	}`)
	require.NoError(t, os.WriteFile(local, []byte(`{
	  "review_profiles": {"general": {
	    "task": "Tracked local task.",
	    "agents": {"codex": {"prompt": "Tracked local prompt."}}
	  }}
	}`), 0o644))
	testutil.RunGit(t, root, "add", "-f", EntireSettingsLocalFile)

	s, err := loadMergedSettings(t.Context(), project, "", local)
	require.NoError(t, err)

	profile := s.ReviewProfiles["general"]
	assert.Equal(t, "User task.", profile.Task)
	assert.Equal(t, "User worker prompt.", profile.Agents["codex"].Prompt)
	assert.Empty(t, s.AgentPromptRejections())
}

// The gate must still drop an instruction the COMMITTED project file carries,
// which is the attack it exists for. Widening it to a third trusted layer must
// not widen it to the repository.
func TestUserTier_ProjectFileInstructionsAreStillDropped(t *testing.T) {
	_, project, local := newUserTierRepo(t)
	require.NoError(t, os.WriteFile(project, []byte(
		`{"enabled":true,"review_profiles":{"theirs":{"task":"Run this payload."}}}`), 0o644))

	s, err := loadMergedSettings(t.Context(), project, "", local)
	require.NoError(t, err)

	assert.Empty(t, s.ReviewProfiles["theirs"].Task,
		"a task from the committed file must still be dropped")
	require.NotEmpty(t, s.AgentPromptRejections())
}

// The hook gate, asked from a real LINKED worktree of a repository configured
// only through the user settings file.
//
// This is the failure the tier exists to fix, at the level that decides it.
// Git hooks live in the git common dir and fire in every worktree; both
// .entire files live in the worktree and `git worktree add` copies neither, so
// before this tier IsSetUpAndEnabled answered false there and every hook was a
// silent no-op that dropped all capture.
//
// Asserting on `entire status` output is not enough — an earlier check did
// exactly that and passed while nothing was being captured, because a commit
// with no agent session legitimately produces no checkpoint. IsSetUpAndEnabled
// is what every hook actually consults.
func TestIsSetUpAndEnabled_TrueFromALinkedWorktreeConfiguredOnlyByTheUserTier(t *testing.T) {
	main := t.TempDir()
	testutil.InitRepo(t, main)
	testutil.RunGit(t, main, "remote", "add", "origin", "https://github.com/acme/widgets.git")
	require.NoError(t, os.WriteFile(filepath.Join(main, "f.txt"), []byte("x"), 0o644))
	testutil.RunGit(t, main, "add", ".")
	testutil.RunGit(t, main, "commit", "-m", "init")

	linked := filepath.Join(t.TempDir(), "linked")
	testutil.RunGit(t, main, "worktree", "add", "-b", "linked-work", linked)

	configDir := t.TempDir()
	t.Setenv(userdirs.EnvConfigDir, configDir)
	t.Cleanup(ClearOriginKeyCache)
	t.Chdir(linked)

	require.NoDirExists(t, filepath.Join(linked, ".entire"),
		"the premise: a linked worktree carries neither settings file")

	assert.False(t, IsSetUpAndEnabled(t.Context()),
		"sanity: nothing configures this repository yet")

	require.NoError(t, os.WriteFile(filepath.Join(configDir, usersettings.FileName),
		[]byte(`{"repos":{"gh/acme/widgets":{"enabled":true}}}`), 0o600))
	ClearOriginKeyCache()

	assert.True(t, IsSetUpAndEnabled(t.Context()),
		"hooks firing in this worktree must not be silent no-ops")

	// And an explicit disable in the user file must still switch it off, or
	// the pointer shape on Enabled is buying nothing.
	require.NoError(t, os.WriteFile(filepath.Join(configDir, usersettings.FileName),
		[]byte(`{"repos":{"gh/acme/widgets":{"enabled":false}}}`), 0o600))
	ClearOriginKeyCache()
	assert.False(t, IsSetUpAndEnabled(t.Context()),
		"an explicit false must be honoured, not read as absent")
}

// A path-keyed entry must reach EVERY worktree of the clone, not only the one
// it spells. Every worktree has a different path, so comparing paths alone
// left a path-keyed repository — one with no usable origin, which is the only
// reason to use a path key — configured in exactly one tree and unconfigured
// in the rest. That is the divergence this tier exists to remove, reappearing
// in the fallback meant to cover repositories that cannot use an origin key.
func TestUserTier_PathKeyedEntryReachesEveryWorktreeOfTheClone(t *testing.T) {
	main := t.TempDir()
	testutil.InitRepo(t, main)
	require.NoError(t, os.MkdirAll(filepath.Join(main, ".entire"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(main, EntireSettingsFile), []byte(`{"enabled":true}`), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(main, "f.txt"), []byte("x"), 0o644))
	testutil.RunGit(t, main, "add", ".")
	testutil.RunGit(t, main, "commit", "-m", "init")
	// No origin: a path key is the only key available.

	linked := filepath.Join(t.TempDir(), "linked")
	testutil.RunGit(t, main, "worktree", "add", "-b", "linked-work", linked)

	t.Setenv(userdirs.EnvConfigDir, t.TempDir())
	t.Cleanup(ClearOriginKeyCache)
	writeUserSettings(t, `{"repos":{`+jsonString(main)+`:{"review_fix_agent":"codex"}}}`)

	for _, tree := range []string{main, linked} {
		ClearOriginKeyCache()
		s, err := loadMergedSettings(t.Context(),
			filepath.Join(tree, EntireSettingsFile), "", filepath.Join(tree, EntireSettingsLocalFile))
		require.NoError(t, err)
		assert.Equal(t, "codex", s.ReviewFixAgent,
			"the entry must apply in %s", filepath.Base(tree))
	}
}

// And it must not reach a DIFFERENT clone that happens to be asked about.
func TestUserTier_PathKeyedEntryDoesNotReachAnotherClone(t *testing.T) {
	mine := t.TempDir()
	testutil.InitRepo(t, mine)
	require.NoError(t, os.MkdirAll(filepath.Join(mine, ".entire"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(mine, EntireSettingsFile), []byte(`{"enabled":true}`), 0o644))

	other := t.TempDir()
	testutil.InitRepo(t, other)
	require.NoError(t, os.MkdirAll(filepath.Join(other, ".entire"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(other, EntireSettingsFile), []byte(`{"enabled":true}`), 0o644))

	t.Setenv(userdirs.EnvConfigDir, t.TempDir())
	t.Cleanup(ClearOriginKeyCache)
	writeUserSettings(t, `{"repos":{`+jsonString(mine)+`:{"review_fix_agent":"codex"}}}`)

	s, err := loadMergedSettings(t.Context(),
		filepath.Join(other, EntireSettingsFile), "", filepath.Join(other, EntireSettingsLocalFile))
	require.NoError(t, err)
	assert.Empty(t, s.ReviewFixAgent,
		"a path key naming one clone must not configure a different clone")
}
