package cli

import (
	"context"
	"os"
	"os/exec"
	"testing"
	"time"

	dispatchpkg "github.com/entireio/cli/cmd/entire/cli/dispatch"
	"github.com/entireio/cli/cmd/entire/cli/gitremote"
	"github.com/entireio/cli/cmd/entire/cli/gitrepo"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/settings"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func setupSSHIdentityRepo(t *testing.T, hostname string) (string, func() []string) {
	t.Helper()
	testutil.IsolateGitConfigEnv(t)
	t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())
	calls := testutil.InstallFakeSSH(t, map[string]string{"personal": hostname})
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	testutil.AddRemote(t, dir, "origin", "git@personal:Acme/Widgets.git")
	t.Chdir(dir)
	paths.ClearWorktreeRootCache()
	t.Cleanup(paths.ClearWorktreeRootCache)
	return dir, calls
}

// Nonparallel: fixtures isolate CWD, git configuration and fake SSH.
func TestSSHIdentityCommandBoundaries(t *testing.T) {
	dir, calls := setupSSHIdentityRepo(t, "github.com")
	ctx := gitremote.WithResolutionCache(t.Context())
	forge, owner, repo, err := resolveDefaultSearchRepo(ctx, false)
	require.NoError(t, err)
	assert.Equal(t, []string{"gh", "Acme", "Widgets"}, []string{forge, owner, repo})
	opts := buildCodeSearchOpts("entire search", forge, owner, repo, nil, false, false)
	assert.Equal(t, []string{"gh/Acme/Widgets"}, opts.repoFilters)
	assert.Len(t, calls(), 1, "building code options must reuse the default scope")
	assert.Equal(t, "gh/Acme/Widgets", currentRepoSlugWithForge(ctx))
	assert.Equal(t, "gh/Acme/Widgets", attachLinkRepo(ctx, "origin"))
	assert.True(t, explainRepoIsCurrent(ctx, "gh", "acme", "widgets"))
	forge, owner, repo, err = resolveTrailRemote(ctx)
	require.NoError(t, err)
	assert.Equal(t, []string{"gh", "Acme", "Widgets"}, []string{forge, owner, repo})
	scope, err := currentTrailEnablementScope(ctx)
	require.NoError(t, err)
	assert.True(t, scope.Supported)
	assert.Equal(t, "gh/Acme/Widgets", scope.RepoKey)
	assert.Len(t, calls(), 1, "trail and refresh paths in one operation share discovery")

	slug, err := dispatchpkg.ResolveOriginRepoSlug(ctx, dir, gitrepo.EnvWithoutRepoOverrides(), "git@personal:Acme/Widgets.git")
	require.NoError(t, err)
	assert.Equal(t, "gh/Acme/Widgets", slug)
	ref, err := resolveRemoteRepoRef(ctx, dir, "origin", "")
	require.NoError(t, err)
	assert.Equal(t, mirrorRepoRef{forge: "gh", owner: "acme", repo: "widgets"}, ref)
	assert.Equal(t, slug, discoverRepoSlug(ctx, dir))
	assert.Len(t, calls(), 2, "explicit worktree callers share their contextual resolution")

	// The old unqualified key cannot satisfy the newly qualified scope.
	old := scope
	old.Forge, old.RepoKey = "", "/Acme/Widgets"
	require.NoError(t, saveTrailsEnabledForScope(ctx, old, false, time.Now()))
	assert.Equal(t, trailEnablementCacheUnknown, cachedTrailsEnablementForScope(ctx, scope, time.Now()))
}

func TestSSHIdentityExplicitSearchScope(t *testing.T) {
	_, calls := setupSSHIdentityRepo(t, "github.com")
	forge, owner, repo, err := resolveDefaultSearchRepo(t.Context(), true)
	require.NoError(t, err)
	assert.Empty(t, forge+owner+repo)
	explicit := buildCodeSearchOpts("entire search", forge, owner, repo, []string{"et/project/repo"}, false, false)
	assert.Equal(t, []string{"et/project/repo"}, explicit.repoFilters)
	all := buildCodeSearchOpts("entire search", forge, owner, repo, nil, true, false)
	assert.Empty(t, all.repoFilters)
	assert.Empty(t, calls())
}

func TestSSHIdentityFailedDiscoveryDoesNotDisableTrails(t *testing.T) {
	_, calls := setupSSHIdentityRepo(t, "github.com extra") // malformed ssh -G output
	ctx := gitremote.WithResolutionCache(t.Context())
	forge, owner, repo, err := resolveDefaultSearchRepo(ctx, false)
	require.NoError(t, err, "optional discovery must not create a hard search failure")
	assert.Equal(t, []string{"", "Acme", "Widgets"}, []string{forge, owner, repo})
	_, _, _, err = resolveTrailRemote(ctx)
	require.Error(t, err, "trail commands still reject an unresolved forge")
	_, err = currentTrailEnablementScope(ctx)
	require.Error(t, err, "an unsuccessful lookup is not a definitive unsupported scope")
	require.NoError(t, runTrailEnablementRefresh(ctx)) // background failure remains best effort
	enabled, err := refreshTrailsEnabledCache(ctx, nil, "unused")
	require.Error(t, err)
	assert.False(t, enabled)
	cacheTrailsEnablementAfterEnable(ctx, false)
	prefs, err := settings.LoadClonePreferences(t.Context())
	require.NoError(t, err)
	assert.Nil(t, prefs.TrailsEnabled, "none of the persistence paths may cache transient failure as disabled")
	assert.Len(t, calls(), 1)
}

func TestSSHIdentityInterruptedDiscoveryDoesNotDisableTrails(t *testing.T) {
	setupSSHIdentityRepo(t, "github.com")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := currentTrailEnablementScope(ctx)
	require.ErrorIs(t, err, context.Canceled)
	require.NoError(t, runTrailEnablementRefresh(ctx))
	enabled, err := refreshTrailsEnabledCache(ctx, nil, "unused")
	require.ErrorIs(t, err, context.Canceled)
	assert.False(t, enabled)
	cacheTrailsEnablementAfterEnable(ctx, false)
	prefs, err := settings.LoadClonePreferences(t.Context())
	require.NoError(t, err)
	assert.Nil(t, prefs.TrailsEnabled)
}

func TestSSHIdentityUnsupportedIsDefinitive(t *testing.T) {
	setupSSHIdentityRepo(t, "github.com.evil.example")
	scope, err := currentTrailEnablementScope(t.Context())
	require.NoError(t, err)
	assert.False(t, scope.Supported)
	require.NoError(t, runTrailEnablementRefresh(t.Context()))
	prefs, err := settings.LoadClonePreferences(t.Context())
	require.NoError(t, err)
	require.NotNil(t, prefs.TrailsEnabled)
	assert.False(t, *prefs.TrailsEnabled)
}

func TestSSHIdentityMirrorTargetIgnoresAmbientRepoSelectors(t *testing.T) {
	target, calls := setupSSHIdentityRepo(t, "github.com")
	other := t.TempDir()
	testutil.InitRepo(t, other)
	testutil.AddRemote(t, other, "origin", "https://github.com/wrong/repository")
	testutil.RunGit(t, other, "config", "core.sshCommand", "unsupported-wrapper")
	t.Setenv("GIT_DIR", other+"/.git")
	t.Setenv("GIT_WORK_TREE", other)
	ref, err := resolveRemoteRepoRef(t.Context(), target, "origin", "")
	require.NoError(t, err)
	assert.Equal(t, mirrorRepoRef{forge: "gh", owner: "acme", repo: "widgets"}, ref)
	assert.Len(t, calls(), 1)
}

func TestSSHIdentityTimedOutLookupCannotBecomeDisabled(t *testing.T) {
	setupSSHIdentityRepo(t, "github.com")
	// Replace only the fixture executable, never a system SSH binary.
	fakeSSH, err := exec.LookPath("ssh")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(fakeSSH, []byte("#!/bin/sh\nsleep 30 &\nwait\n"), 0o700))
	operation := gitremote.WithResolutionCache(t.Context())
	lookup, cancel := context.WithTimeout(operation, 100*time.Millisecond)
	defer cancel()
	_, err = currentTrailEnablementScope(lookup)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	// The outer operation is still live. Reusing the failed lookup must carry
	// its transient error, not turn empty Forge into an unsupported scope.
	_, err = currentTrailEnablementScope(operation)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.NoError(t, runTrailEnablementRefresh(operation))
	enabled, err := refreshTrailsEnabledCache(operation, nil, "unused")
	require.ErrorIs(t, err, context.DeadlineExceeded)
	assert.False(t, enabled)
	cacheTrailsEnablementAfterEnable(operation, false)
	prefs, err := settings.LoadClonePreferences(t.Context())
	require.NoError(t, err)
	assert.Nil(t, prefs.TrailsEnabled)
}

func TestSSHIdentityTurnStartUsesSavedScope(t *testing.T) {
	testutil.IsolateGitConfigEnv(t)
	calls := testutil.InstallFakeSSH(t, map[string]string{"personal": "github.com"})
	const sessionID = "ssh-alias-scope-hint"
	setupContextInjectionRepo(t, sessionID, false)
	testutil.RunGit(t, ".", "remote", "set-url", "origin", "git@personal:acme/widgets.git")
	scope, err := currentTrailEnablementScope(t.Context())
	require.NoError(t, err)
	require.NoError(t, saveTrailEnablementScopeHint(t.Context(), sessionID, scope))
	require.NoError(t, saveTrailsEnabledForScope(t.Context(), scope, true, time.Now()))
	require.Len(t, calls(), 1)
	ag := newMockContextInjectorAgent()
	runContextInjectionTurn(t, ag, sessionID)
	runContextInjectionTurn(t, ag, sessionID)
	assert.Len(t, calls(), 1, "prompt handling must use the saved scope, not run SSH")
}
