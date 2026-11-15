package gitremote

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/execx"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Nonparallel: fake executables and git configuration are process-scoped.
func TestResolveInfoAliases(t *testing.T) {
	testutil.IsolateGitConfigEnv(t)
	calls := testutil.InstallFakeSSHWithCalls(t, map[string]string{"github_personal.com": "GitHub.com"})
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	ctx := WithResolutionCache(t.Context())
	for _, raw := range []string{
		"git@github_personal.com:Acme/Widgets.git",
		"ssh://git@github_personal.com/Acme/Widgets.git",
		"git+ssh://git@github_personal.com/Acme/Widgets.git",
		"ssh+git://git@github_personal.com/Acme/Widgets.git",
	} {
		parsed, err := ParseURL(raw)
		require.NoError(t, err)
		info, err := ResolveInfo(ctx, dir, nil, raw)
		require.NoError(t, err)
		assert.Empty(t, parsed.Forge, "parsing is pure")
		parsed.Forge = ForgeGitHub
		assert.Equal(t, parsed, info, "only identity may change")
		repo, ok := ResolveRepository(ctx, dir, nil, raw)
		require.True(t, ok)
		assert.Equal(t, Repository{ForgeGitHub, "acme", "widgets"}, repo)
	}
	require.Len(t, calls(), 1, "all equivalent endpoints share one operation lookup")
	assert.Equal(t, []string{"-G", "-l", "git", "github_personal.com"}, calls()[0].Args)
	actualDir, err := filepath.EvalSymlinks(dir)
	require.NoError(t, err)
	assert.Equal(t, actualDir, calls()[0].Dir)

	raw := "ssh://another_user@github_personal.com:2222/Acme/Widgets.git"
	info, err := ResolveInfo(ctx, dir, nil, raw)
	require.NoError(t, err)
	assert.Equal(t, "github_personal.com:2222", info.HostPort())
	assert.Equal(t, []string{"-G", "-l", "another_user", "-p", "2222", "github_personal.com"}, calls()[1].Args)
	testutil.AddRemote(t, dir, "origin", raw)
	t.Chdir(dir)
	forge, owner, repo, err := ResolveRemoteRepo(t.Context(), "origin")
	require.NoError(t, err)
	assert.Equal(t, []string{"gh", "Acme", "Widgets"}, []string{forge, owner, repo})
	unchanged, err := GetRemoteURL(t.Context(), "origin")
	require.NoError(t, err)
	assert.Equal(t, raw, unchanged)
}

func TestResolveInfoNoDiscovery(t *testing.T) {
	// Neither git config nor SSH may run. The directory contains no binaries.
	t.Setenv("PATH", t.TempDir())
	for _, raw := range []string{
		"git@github.com:Acme/Widgets.git",
		"https://github.com/Acme/Widgets.git",
		"entire://cell/gh/Acme/Widgets",
		"entire://cell/et/Acme/Widgets",
		"https://github_personal.com/Acme/Widgets.git",
		"bogus+ssh://github_personal.com/Acme/Widgets.git",
		"bogus+ssh://github.com/Acme/Widgets.git",
		"helper::github_personal.com/Acme/Widgets.git",
	} {
		parsed, err := ParseURL(raw)
		require.NoError(t, err)
		info, err := ResolveInfo(t.Context(), "", nil, raw)
		require.NoError(t, err, raw)
		assert.Equal(t, parsed, info)
		_, want := parsed.Repository()
		_, got := ResolveRepository(t.Context(), "", nil, raw)
		assert.Equal(t, want, got, raw)
	}
}

func TestResolveInfoUnsupportedAndFailures(t *testing.T) {
	testutil.IsolateGitConfigEnv(t)
	calls := testutil.InstallFakeSSH(t, map[string]string{
		"lookalike": "github.com.evil.example",
		"malformed": "github.com extra",
		"missing":   "",
	})
	for _, host := range []string{"unknown.example", "lookalike", "github.com.evil.example"} {
		info, err := ResolveInfo(t.Context(), "", nil, "git@"+host+":acme/widgets")
		require.NoError(t, err)
		assert.Empty(t, info.Forge)
	}
	for _, host := range []string{"malformed", "missing"} {
		info, err := ResolveInfo(t.Context(), "", nil, "git@"+host+":acme/widgets")
		require.Error(t, err)
		assert.Empty(t, info.Forge)
		_, ok := ResolveRepository(t.Context(), "", nil, "git@"+host+":acme/widgets")
		assert.False(t, ok)
	}
	before := len(calls())
	for _, raw := range []string{
		"git@-option:acme/widgets", "-user@alias:acme/widgets",
		"git@bad\nhost:acme/widgets", "ssh://bad%0Auser@alias/acme/widgets",
		"ssh://git@alias:65536/acme/widgets", "ssh://git@alias:0/acme/widgets",
		"ssh://git@alias:bad/acme/widgets", "ssh://git@alias:-1/acme/widgets",
		"git@bad$(cmd):acme/widgets",
	} {
		_, err := ResolveInfo(t.Context(), "", nil, raw)
		require.Error(t, err, raw)
	}
	assert.Len(t, calls(), before, "invalid argv values must not reach SSH")
}

func TestResolveInfoCustomCommandsAndFreshOperations(t *testing.T) {
	testutil.IsolateGitConfigEnv(t)
	calls := testutil.InstallFakeSSH(t, map[string]string{"alias": "github.com"})
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	const raw = "git@alias:acme/widgets"
	ctx := WithResolutionCache(t.Context())
	info, err := ResolveInfo(ctx, dir, nil, raw)
	require.NoError(t, err)
	assert.Equal(t, ForgeGitHub, info.Forge)
	testutil.RunGit(t, dir, "config", "core.sshCommand", "wrapper --secret")
	// A new operation must see the new config, even with identical URL and env.
	info, err = ResolveInfo(WithResolutionCache(t.Context()), dir, nil, raw)
	require.NoError(t, err)
	assert.Empty(t, info.Forge)
	testutil.RunGit(t, dir, "config", "--unset", "core.sshCommand")
	for _, key := range []string{"GIT_SSH_COMMAND", "GIT_SSH"} {
		t.Run(key, func(t *testing.T) {
			t.Setenv(key, "wrapper --secret")
			info, err := ResolveInfo(ctx, dir, nil, raw)
			require.NoError(t, err)
			assert.Empty(t, info.Forge)
		})
	}
	require.Len(t, calls(), 1)
	// Same operation, a different explicit worktree must also get its own lookup.
	other := t.TempDir()
	testutil.InitRepo(t, other)
	_, err = ResolveInfo(ctx, other, nil, raw)
	require.NoError(t, err)
	assert.Len(t, calls(), 2)
}

func installDiscoveryProgram(t *testing.T, name, body string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(path, []byte("#!/bin/sh\n[ \"$1\" = --warmup ] && exit 0\n"+body), 0o700))
	// Avoid charging macOS's fresh-executable scanning to cancellation tests.
	require.NoError(t, execx.NonInteractive(t.Context(), path, "--warmup").Run())
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestResolveInfoCommandFailure(t *testing.T) {
	testutil.IsolateGitConfigEnv(t)
	testutil.InstallFakeSSH(t, nil)
	installDiscoveryProgram(t, "ssh", "exit 42\n")
	info, err := ResolveInfo(t.Context(), "", nil, "git@alias:acme/widgets")
	require.Error(t, err)
	assert.Empty(t, info.Forge)
	info, err = ResolveInfoBestEffort(t.Context(), "", nil, "git@alias:acme/widgets")
	require.NoError(t, err)
	assert.Equal(t, "acme", info.Owner)
	_, ok := ResolveRepository(t.Context(), "", nil, "git@alias:acme/widgets")
	assert.False(t, ok)
}

func TestResolveInfoMissingSSH(t *testing.T) {
	testutil.IsolateGitConfigEnv(t)
	t.Setenv("GIT_SSH_COMMAND", "ssh") // bypass git config so this tests missing SSH specifically
	t.Setenv("PATH", t.TempDir())
	info, err := ResolveInfo(t.Context(), "", nil, "git@alias:acme/widgets")
	require.NoError(t, err)
	assert.Empty(t, info.Forge)
}

func TestResolveInfoCancellation(t *testing.T) {
	testutil.IsolateGitConfigEnv(t)
	calls := testutil.InstallFakeSSH(t, nil)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := ResolveInfoBestEffort(ctx, "", nil, "git@alias:acme/widgets")
	require.ErrorIs(t, err, context.Canceled)
	assert.Empty(t, calls())
}

func TestResolveInfoDeadlineIncludesConfigAndDescendants(t *testing.T) {
	for _, program := range []string{"git", "ssh"} {
		t.Run(program, func(t *testing.T) {
			testutil.IsolateGitConfigEnv(t)
			calls := testutil.InstallFakeSSH(t, nil)
			// A background descendant retains output pipes after its parent exits.
			installDiscoveryProgram(t, program, "sleep 30 &\nwait\n")
			ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
			defer cancel()
			start := time.Now()
			_, err := ResolveInfoBestEffort(ctx, "", nil, "git@alias:acme/widgets")
			require.ErrorIs(t, err, context.DeadlineExceeded)
			assert.Less(t, time.Since(start), 2*time.Second, "cleanup must not add execx's ten-second wait")
			assert.Empty(t, calls())
		})
	}
}

func TestResolveInfoOwnBudgetAndRetry(t *testing.T) {
	testutil.IsolateGitConfigEnv(t)
	testutil.InstallFakeSSH(t, map[string]string{"alias": "github.com"})
	installDiscoveryProgram(t, "git", "sleep 30 &\nwait\n")
	ctx := WithResolutionCache(t.Context())
	start := time.Now()
	info, err := ResolveInfo(ctx, "", nil, "git@alias:acme/widgets")
	require.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Empty(t, info.Forge)
	elapsed := time.Since(start)
	assert.GreaterOrEqual(t, elapsed, 4*time.Second, "push discovery retains a generous budget")
	assert.Less(t, elapsed, 7*time.Second)
	// Replace the timed-out fixture with an absent-config result, keeping the
	// same environment and operation. A healthy retry must run discovery again.
	git, err := exec.LookPath("git")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(git, []byte("#!/bin/sh\nexit 1\n"), 0o700))
	info, err = ResolveInfo(ctx, "", nil, "git@alias:acme/widgets")
	require.NoError(t, err)
	assert.Equal(t, ForgeGitHub, info.Forge)
}

func TestResolveInfoCacheWaitHonorsCancellation(t *testing.T) {
	t.Parallel()
	ctx := WithResolutionCache(t.Context())
	cache, ok := ctx.Value(resolutionCacheKey{}).(*resolutionCache)
	require.True(t, ok)
	target := sshTarget{host: "alias"}
	cache.results[sshLookupKey{target: target, env: strings.Join([]string{"A=B"}, "\x00")}] = &sshLookupResult{ready: make(chan struct{})}
	ctx, cancel := context.WithCancel(ctx)
	cancel()
	_, err := cachedSSHHostname(ctx, "", []string{"A=B"}, target)
	require.ErrorIs(t, err, context.Canceled)
}

func TestResolveInfoSubprocessCounts(t *testing.T) {
	testutil.IsolateGitConfigEnv(t)
	calls := testutil.InstallFakeSSH(t, map[string]string{"alias": "github.com"})
	git, err := exec.LookPath("git")
	require.NoError(t, err)
	log := filepath.Join(t.TempDir(), "config-calls")
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }
	installDiscoveryProgram(t, "git", "echo called >> "+quote(log)+"\nexec "+quote(git)+" \"$@\"\n")
	ctx := WithResolutionCache(t.Context())
	for _, raw := range []string{"git@alias:a/b", "ssh://git@alias/c/d", "git+ssh://git@alias/a/b"} {
		_, err := ResolveInfo(ctx, "", nil, raw)
		require.NoError(t, err)
	}
	data, err := os.ReadFile(log)
	require.NoError(t, err)
	assert.Len(t, strings.Fields(string(data)), 1)
	assert.Len(t, calls(), 1)
	_, err = ResolveInfo(WithResolutionCache(t.Context()), "", nil, "git@alias:a/b")
	require.NoError(t, err)
	data, err = os.ReadFile(log)
	require.NoError(t, err)
	assert.Len(t, strings.Fields(string(data)), 2)
	assert.Len(t, calls(), 2)
}

func TestResolveInfoGitConfigFailure(t *testing.T) {
	testutil.IsolateGitConfigEnv(t)
	calls := testutil.InstallFakeSSH(t, nil)
	installDiscoveryProgram(t, "git", "exit 128\n")
	info, err := ResolveInfo(t.Context(), "", nil, "git@alias:a/b")
	require.Error(t, err)
	assert.Empty(t, info.Forge)
	assert.Empty(t, calls(), "failed config lookup must not assume plain ssh")
}

func TestResolveInfoExitedParentWithInheritedPipes(t *testing.T) {
	testutil.IsolateGitConfigEnv(t)
	testutil.InstallFakeSSH(t, nil)
	installDiscoveryProgram(t, "ssh", "sleep 1 &\necho hostname github.com\n")
	started := time.Now()
	info, err := ResolveInfo(t.Context(), "", nil, "git@alias:a/b")
	require.NoError(t, err)
	assert.Equal(t, ForgeGitHub, info.Forge)
	assert.Less(t, time.Since(started), 2*time.Second)
}

func TestResolveInfoCallerDeadlineDoesNotPoisonCache(t *testing.T) {
	testutil.IsolateGitConfigEnv(t)
	testutil.InstallFakeSSH(t, nil)
	installDiscoveryProgram(t, "ssh", "sleep 0.2\necho hostname github.com\n")
	operation := WithResolutionCache(t.Context())
	lookup, cancel := context.WithTimeout(operation, 50*time.Millisecond)
	defer cancel()
	_, err := ResolveInfo(lookup, "", nil, "git@alias:acme/widgets")
	require.ErrorIs(t, err, context.DeadlineExceeded)
	info, err := ResolveInfo(operation, "", nil, "git@alias:acme/widgets")
	require.NoError(t, err)
	assert.Equal(t, ForgeGitHub, info.Forge)
}
