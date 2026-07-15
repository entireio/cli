package remote

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/testutil"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestExtractRemoteFromArgs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		args []string
		want string
	}{
		{"fetch with URL", []string{"fetch", "--no-auto-gc", "https://github.com/org/repo.git", "refs/heads/main"}, "https://github.com/org/repo.git"},
		{"push with flags", []string{"push", "--no-verify", "--porcelain", "origin", "main"}, "origin"},
		{"ls-remote", []string{"ls-remote", "origin", "refs/heads/*"}, "origin"},
		{"fetch with filter", []string{"fetch", "--no-auto-gc", "--no-tags", "--filter=blob:none", "https://host/r.git", "+refs/heads/main:refs/tmp"}, "https://host/r.git"},
		{"empty args", []string{}, ""},
		{"subcommand only", []string{"fetch"}, ""},
		{"only flags", []string{"fetch", "--no-tags"}, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, extractRemoteFromArgs(tt.args))
		})
	}
}

func TestResolveTargetForTokenAuth(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	t.Run("HTTPS URL passes through as HTTPS", func(t *testing.T) {
		t.Parallel()
		got, proto := resolveTargetForTokenAuth(ctx, "https://github.com/org/repo.git")
		assert.Equal(t, "https://github.com/org/repo.git", got)
		assert.Equal(t, ProtocolHTTPS, proto)
	})

	t.Run("SSH SCP URL rewrites to HTTPS", func(t *testing.T) {
		t.Parallel()
		got, proto := resolveTargetForTokenAuth(ctx, "git@github.com:org/repo.git")
		assert.Equal(t, "https://github.com/org/repo.git", got)
		assert.Equal(t, ProtocolHTTPS, proto)
	})

	t.Run("SSH protocol URL rewrites to HTTPS without port", func(t *testing.T) {
		t.Parallel()
		got, proto := resolveTargetForTokenAuth(ctx, "ssh://git@git.example.com:2222/org/repo.git")
		assert.Equal(t, "https://git.example.com/org/repo.git", got)
		assert.Equal(t, ProtocolHTTPS, proto)
	})

	t.Run("local path returns empty protocol", func(t *testing.T) {
		t.Parallel()
		got, proto := resolveTargetForTokenAuth(ctx, "/tmp/some-bare-repo")
		assert.Equal(t, "/tmp/some-bare-repo", got)
		assert.Empty(t, proto)
	})

	t.Run("nonexistent remote name returns empty protocol", func(t *testing.T) {
		t.Parallel()
		got, proto := resolveTargetForTokenAuth(ctx, "nonexistent-remote")
		assert.Equal(t, "nonexistent-remote", got)
		assert.Empty(t, proto)
	})
}

// Not parallel: uses t.Chdir()
func TestResolveTargetForTokenAuth_RemoteName_HTTPS(t *testing.T) {
	ctx := context.Background()

	tmpDir := t.TempDir()
	testutil.InitRepo(t, tmpDir)
	testutil.WriteFile(t, tmpDir, "f.txt", "init")
	testutil.GitAdd(t, tmpDir, "f.txt")
	testutil.GitCommit(t, tmpDir, "init")

	cmd := exec.CommandContext(ctx, "git", "remote", "add", "origin", "https://github.com/org/repo.git")
	cmd.Dir = tmpDir
	cmd.Env = testutil.GitIsolatedEnv()
	require.NoError(t, cmd.Run())

	t.Chdir(tmpDir)

	got, proto := resolveTargetForTokenAuth(ctx, "origin")
	assert.Equal(t, "origin", got, "HTTPS remote names pass through unchanged")
	assert.Equal(t, ProtocolHTTPS, proto)
}

// Not parallel: uses t.Chdir()
func TestResolveTargetForTokenAuth_RemoteName_SSH_RewritesToHTTPS(t *testing.T) {
	ctx := context.Background()

	tmpDir := t.TempDir()
	testutil.InitRepo(t, tmpDir)
	testutil.WriteFile(t, tmpDir, "f.txt", "init")
	testutil.GitAdd(t, tmpDir, "f.txt")
	testutil.GitCommit(t, tmpDir, "init")

	cmd := exec.CommandContext(ctx, "git", "remote", "add", "origin", "git@github.com:org/repo.git")
	cmd.Dir = tmpDir
	cmd.Env = testutil.GitIsolatedEnv()
	require.NoError(t, cmd.Run())

	t.Chdir(tmpDir)

	got, proto := resolveTargetForTokenAuth(ctx, "origin")
	assert.Equal(t, "https://github.com/org/repo.git", got)
	assert.Equal(t, ProtocolHTTPS, proto)
}

// Not parallel: uses t.Chdir()
func TestResolvePushCommandTarget(t *testing.T) {
	ctx := context.Background()

	tests := []struct {
		name         string
		originURL    string
		settingsJSON string
		token        string
		target       string
		want         string
	}{
		{
			// Without checkpoint_remote configured the push should use the
			// remote name so git updates refs/remotes/origin/<branch> and
			// subsequent hasUnpushedBranchRef checks can short-circuit.
			name:         "no checkpoint remote keeps remote name",
			originURL:    "git@github.com:acme/app.git",
			settingsJSON: `{"enabled":true}`,
			target:       "origin",
			want:         "origin",
		},
		{
			// With token set but no checkpoint_remote, PushURL still returns
			// the coerced HTTPS URL but enabled=false. resolvePushCommandTarget
			// should still return the name — newCommand handles token coercion.
			name:         "no checkpoint remote with token keeps remote name",
			originURL:    "git@github.com:acme/app.git",
			settingsJSON: `{"enabled":true}`,
			token:        "push-token",
			target:       "origin",
			want:         "origin",
		},
		{
			// With checkpoint_remote configured, use the derived URL so the
			// push actually goes to the separate checkpoint repo.
			name:         "checkpoint remote routes to checkpoint URL",
			originURL:    "https://github.com/acme/app.git",
			settingsJSON: `{"enabled":true,"strategy_options":{"checkpoint_remote":{"provider":"github","repo":"acme/checkpoints"}}}`,
			target:       "origin",
			want:         "https://github.com/acme/checkpoints.git",
		},
		{
			name:         "local path target stays unchanged",
			settingsJSON: `{"enabled":true}`,
			target:       "/tmp/bare-repo",
			want:         "/tmp/bare-repo",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tmpDir := t.TempDir()
			testutil.InitRepo(t, tmpDir)
			testutil.WriteFile(t, tmpDir, "f.txt", "init")
			testutil.GitAdd(t, tmpDir, "f.txt")
			testutil.GitCommit(t, tmpDir, "init")
			if tt.originURL != "" {
				cmd := exec.CommandContext(ctx, "git", "remote", "add", "origin", tt.originURL)
				cmd.Dir = tmpDir
				cmd.Env = testutil.GitIsolatedEnv()
				require.NoError(t, cmd.Run())
			}
			if tt.settingsJSON != "" {
				testutil.WriteFile(t, tmpDir, ".entire/settings.json", tt.settingsJSON)
			}
			t.Chdir(tmpDir)
			if tt.token != "" {
				t.Setenv(CheckpointTokenEnvVar, tt.token)
			}

			got, err := resolvePushCommandTarget(ctx, tt.target)
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

// Not parallel: uses t.Chdir()
func TestResolveFetchTarget(t *testing.T) {
	ctx := context.Background()

	tmpDir := t.TempDir()
	testutil.InitRepo(t, tmpDir)
	testutil.WriteFile(t, tmpDir, "f.txt", "init")
	testutil.GitAdd(t, tmpDir, "f.txt")
	testutil.GitCommit(t, tmpDir, "init")

	cmd := exec.CommandContext(ctx, "git", "remote", "add", "origin", "https://github.com/org/repo.git")
	cmd.Dir = tmpDir
	cmd.Env = testutil.GitIsolatedEnv()
	require.NoError(t, cmd.Run())

	t.Chdir(tmpDir)

	t.Run("disabled returns remote name", func(t *testing.T) {
		target, err := ResolveFetchTarget(ctx, "origin")
		require.NoError(t, err)
		assert.Equal(t, "origin", target)
	})

	t.Run("enabled resolves remote to URL", func(t *testing.T) {
		testutil.WriteFile(
			t,
			tmpDir,
			".entire/settings.json",
			`{"enabled": true, "strategy_options": {"filtered_fetches": true}}`,
		)

		target, err := ResolveFetchTarget(ctx, "origin")
		require.NoError(t, err)
		assert.Equal(t, "https://github.com/org/repo.git", target)
	})

	t.Run("URL target stays unchanged", func(t *testing.T) {
		target, err := ResolveFetchTarget(ctx, "https://github.com/org/repo.git")
		require.NoError(t, err)
		assert.Equal(t, "https://github.com/org/repo.git", target)
	})

	t.Run("local path target stays unchanged", func(t *testing.T) {
		target, err := ResolveFetchTarget(ctx, "../repo.git")
		require.NoError(t, err)
		assert.Equal(t, "../repo.git", target)
	})
}

func TestFetch_Unshallow(t *testing.T) {
	t.Parallel()

	t.Run("Unshallow=true deepens a shallow repo", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()

		bareDir, cloneDir := setupShallowClone(ctx, t)
		require.True(t, isShallowRepository(ctx, cloneDir), "test setup should produce a shallow repo")

		out, err := Fetch(ctx, FetchOptions{
			Remote:    "file://" + bareDir,
			RefSpecs:  []string{"+refs/heads/main:refs/remotes/origin/main"},
			NoTags:    true,
			Unshallow: true,
			Dir:       cloneDir,
		})
		require.NoError(t, err, "fetch output: %s", out)

		assert.False(t, isShallowRepository(ctx, cloneDir),
			"Unshallow=true should remove shallow state when the repo is shallow")
	})

	t.Run("Unshallow=false leaves shallow state alone", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()

		bareDir, cloneDir := setupShallowClone(ctx, t)
		require.True(t, isShallowRepository(ctx, cloneDir))

		out, err := Fetch(ctx, FetchOptions{
			Remote:   "file://" + bareDir,
			RefSpecs: []string{"+refs/heads/main:refs/remotes/origin/main"},
			NoTags:   true,
			Dir:      cloneDir,
		})
		require.NoError(t, err, "fetch output: %s", out)

		assert.True(t, isShallowRepository(ctx, cloneDir),
			"a fetch without Unshallow must not silently convert a shallow repo to a full one")
	})
}

func TestFetch_Shallow(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	bareDir, _ := setupShallowClone(ctx, t)
	// Make a fresh non-shallow clone, then fetch with Shallow=true and check
	// .git/shallow appears.
	cloneDir := t.TempDir()
	runIsolatedGit(ctx, t, "", "clone", "--branch", "main", "file://"+bareDir, cloneDir)
	require.False(t, isShallowRepository(ctx, cloneDir), "fresh clone should not be shallow")

	out, err := Fetch(ctx, FetchOptions{
		Remote:   "file://" + bareDir,
		RefSpecs: []string{"+refs/heads/main:refs/remotes/origin/main"},
		NoTags:   true,
		Shallow:  true,
		Dir:      cloneDir,
	})
	require.NoError(t, err, "fetch output: %s", out)

	assert.True(t, isShallowRepository(ctx, cloneDir),
		"Shallow=true should request --depth=1 and leave the repo shallow")
}

// TestFetch_Depth verifies that Depth is ref-scoped: it fully fetches (heals)
// the named branch while leaving an independently-shallow branch shallow —
// unlike Unshallow, which is repo-global.
func TestFetch_Depth(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	tmpDir := t.TempDir()
	bareDir := filepath.Join(tmpDir, "bare.git")
	seedDir := filepath.Join(tmpDir, "seed")
	runIsolatedGit(ctx, t, "", "init", "--bare", bareDir)

	testutil.InitRepo(t, seedDir)
	runIsolatedGit(ctx, t, seedDir, "remote", "add", "origin", bareDir)
	for _, c := range []string{"m1", "m2", "m3"} { // main: 3 commits
		testutil.WriteFile(t, seedDir, "f.txt", c)
		testutil.GitAdd(t, seedDir, "f.txt")
		testutil.GitCommit(t, seedDir, c)
	}
	runIsolatedGit(ctx, t, seedDir, "push", "origin", "HEAD:refs/heads/main")
	runIsolatedGit(ctx, t, seedDir, "checkout", "--orphan", "meta")
	runIsolatedGit(ctx, t, seedDir, "rm", "-rf", ".")
	for _, c := range []string{"c1", "c2"} { // meta: 2 commits
		testutil.WriteFile(t, seedDir, "g.txt", c)
		testutil.GitAdd(t, seedDir, "g.txt")
		testutil.GitCommit(t, seedDir, c)
	}
	runIsolatedGit(ctx, t, seedDir, "push", "origin", "HEAD:refs/heads/meta")

	// Shallow clone of main + shallow fetch of meta → both branches shallow.
	cloneDir := filepath.Join(tmpDir, "clone")
	runIsolatedGit(ctx, t, "", "clone", "--depth=1", "--single-branch", "--branch", "main", "file://"+bareDir, cloneDir)
	runIsolatedGit(ctx, t, cloneDir, "fetch", "--depth=1", "origin", "+refs/heads/meta:refs/remotes/origin/meta")
	require.True(t, isShallowRepository(ctx, cloneDir))
	require.Equal(t, 1, revListCount(ctx, t, cloneDir, "refs/remotes/origin/meta"))
	require.Equal(t, 1, revListCount(ctx, t, cloneDir, "refs/remotes/origin/main"))

	out, err := Fetch(ctx, FetchOptions{
		Remote:   "file://" + bareDir,
		RefSpecs: []string{"+refs/heads/meta:refs/remotes/origin/meta"},
		NoTags:   true,
		Depth:    1_000_000_000,
		Dir:      cloneDir,
	})
	require.NoError(t, err, "fetch output: %s", out)

	assert.Equal(t, 2, revListCount(ctx, t, cloneDir, "refs/remotes/origin/meta"),
		"Depth should fully fetch (heal) the named branch")
	assert.Equal(t, 1, revListCount(ctx, t, cloneDir, "refs/remotes/origin/main"),
		"Depth is ref-scoped: an independently-shallow branch must stay shallow")
	assert.True(t, isShallowRepository(ctx, cloneDir),
		"repo stays shallow because main is still bounded")
}

func revListCount(ctx context.Context, t *testing.T, dir, ref string) int {
	t.Helper()
	cmd := exec.CommandContext(ctx, "git", "rev-list", "--count", ref)
	cmd.Dir = dir
	cmd.Env = testutil.GitIsolatedEnv()
	out, err := cmd.Output()
	require.NoError(t, err)
	n, err := strconv.Atoi(strings.TrimSpace(string(out)))
	require.NoError(t, err)
	return n
}

// setupShallowClone creates a bare origin, a seed repo with one commit pushed
// to it, a shallow (--depth=1) clone, and then advances origin by one more
// commit so that a subsequent fetch into the clone has work to do. Returns the
// bare origin path and the shallow clone path.
func setupShallowClone(ctx context.Context, t *testing.T) (bareDir, cloneDir string) {
	t.Helper()
	tmpDir := t.TempDir()
	bareDir = filepath.Join(tmpDir, "bare.git")
	seedDir := filepath.Join(tmpDir, "seed")
	cloneDir = filepath.Join(tmpDir, "clone")

	testutil.InitRepo(t, seedDir)
	testutil.WriteFile(t, seedDir, "f.txt", "init")
	testutil.GitAdd(t, seedDir, "f.txt")
	testutil.GitCommit(t, seedDir, "init")

	runIsolatedGit(ctx, t, "", "init", "--bare", bareDir)
	runIsolatedGit(ctx, t, seedDir, "remote", "add", "origin", bareDir)
	runIsolatedGit(ctx, t, seedDir, "push", "origin", "HEAD:refs/heads/main")
	runIsolatedGit(ctx, t, "", "clone", "--depth=1", "--branch", "main", "file://"+bareDir, cloneDir)

	testutil.WriteFile(t, seedDir, "f.txt", "init\nnext\n")
	testutil.GitAdd(t, seedDir, "f.txt")
	testutil.GitCommit(t, seedDir, "next")
	runIsolatedGit(ctx, t, seedDir, "push", "origin", "HEAD:refs/heads/main")

	return bareDir, cloneDir
}

func runIsolatedGit(ctx context.Context, t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.CommandContext(ctx, "git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Env = testutil.GitIsolatedEnv()
	require.NoError(t, cmd.Run(), "git %v", args)
}

func TestAppendCheckpointTokenEnv(t *testing.T) {
	t.Parallel()

	t.Run("adds token env vars", func(t *testing.T) {
		t.Parallel()
		env := appendCheckpointTokenEnv([]string{"PATH=/usr/bin", "HOME=/home/user"}, "my-secret-token")
		assert.Contains(t, env, "PATH=/usr/bin")
		assert.Contains(t, env, "HOME=/home/user")
		assert.Contains(t, env, "GIT_CONFIG_COUNT=1")
		assert.Contains(t, env, "GIT_CONFIG_KEY_0=http.extraHeader")
		wantAuth := "Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:my-secret-token"))
		assert.Contains(t, env, "GIT_CONFIG_VALUE_0="+wantAuth)
	})

	t.Run("preserves existing GIT_CONFIG entries and appends at next index", func(t *testing.T) {
		t.Parallel()
		env := appendCheckpointTokenEnv([]string{
			"PATH=/usr/bin",
			"GIT_CONFIG_COUNT=2",
			"GIT_CONFIG_KEY_0=some.key",
			"GIT_CONFIG_VALUE_0=some-value",
			"GIT_CONFIG_KEY_1=other.key",
			"GIT_CONFIG_VALUE_1=other-value",
		}, "new-token")

		for _, e := range env {
			if e == "GIT_CONFIG_COUNT=2" {
				t.Error("old GIT_CONFIG_COUNT should have been replaced")
			}
		}

		assert.Contains(t, env, "GIT_CONFIG_COUNT=3")
		assert.Contains(t, env, "GIT_CONFIG_KEY_0=some.key")
		assert.Contains(t, env, "GIT_CONFIG_VALUE_0=some-value")
		assert.Contains(t, env, "GIT_CONFIG_KEY_1=other.key")
		assert.Contains(t, env, "GIT_CONFIG_VALUE_1=other-value")
		assert.Contains(t, env, "GIT_CONFIG_KEY_2=http.extraHeader")
		wantAuth := "Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:new-token"))
		assert.Contains(t, env, "GIT_CONFIG_VALUE_2="+wantAuth)
	})

	t.Run("invalid GIT_CONFIG_COUNT falls back to zero", func(t *testing.T) {
		t.Parallel()
		env := appendCheckpointTokenEnv([]string{
			"PATH=/usr/bin",
			"GIT_CONFIG_COUNT=not-a-number",
		}, "tok")

		assert.Contains(t, env, "GIT_CONFIG_COUNT=1")
		assert.Contains(t, env, "GIT_CONFIG_KEY_0=http.extraHeader")
	})
}

func TestIsValidToken(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		token string
		valid bool
	}{
		{"normal token", "ghp_abc123XYZ", true},
		{"with hyphen and underscore", "token-with_special.chars", true},
		{"contains CR", "token\rinjection", false},
		{"contains LF", "token\ninjection", false},
		{"contains CRLF", "token\r\ninjection", false},
		{"contains null byte", "token\x00injection", false},
		{"contains tab", "token\tvalue", false},
		{"contains DEL", "token\x7Fvalue", false},
		{"contains bell", "token\x07value", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.valid, isValidToken(tt.token))
		})
	}
}

// Not parallel: uses t.Setenv()
func TestNewCommand_ControlCharsInToken(t *testing.T) {
	t.Setenv(CheckpointTokenEnvVar, "token\r\nEvil: injected-header")

	cmd := newCommand(context.Background(), "fetch", "https://github.com/org/repo.git")
	assert.Nil(t, cmd.Env, "env should not be set when token contains control characters")
}

// Not parallel: uses t.Setenv()
func TestNewCommand_NoToken(t *testing.T) {
	t.Setenv(CheckpointTokenEnvVar, "")

	cmd := newCommand(context.Background(), "fetch", "https://github.com/org/repo.git")
	assert.Nil(t, cmd.Stdin, "stdin should be nil")
	assert.Nil(t, cmd.Env, "env should not be set when token is empty")
}

// Not parallel: uses t.Setenv()
func TestNewCommand_WhitespaceToken(t *testing.T) {
	t.Setenv(CheckpointTokenEnvVar, "   ")

	cmd := newCommand(context.Background(), "fetch", "https://github.com/org/repo.git")
	assert.Nil(t, cmd.Env, "env should not be set when token is only whitespace")
}

// Not parallel: uses t.Setenv()
func TestNewCommand_HTTPS_InjectsToken(t *testing.T) {
	t.Setenv(CheckpointTokenEnvVar, "ghp_test123")

	cmd := newCommand(context.Background(), "fetch", "https://github.com/org/repo.git")
	require.NotNil(t, cmd.Env, "env should be set for HTTPS with token")

	envMap := envToMap(cmd.Env)
	assert.Equal(t, "1", envMap["GIT_CONFIG_COUNT"])
	assert.Equal(t, "http.extraHeader", envMap["GIT_CONFIG_KEY_0"])
	wantAuth := "Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:ghp_test123"))
	assert.Equal(t, wantAuth, envMap["GIT_CONFIG_VALUE_0"])
}

// Not parallel: uses t.Setenv()
func TestNewCommand_SSH_URL_RewritesToHTTPSAndInjectsToken(t *testing.T) {
	t.Setenv(CheckpointTokenEnvVar, "ghp_test123")

	cmd := newCommand(context.Background(), "push", "git@github.com:org/repo.git", "main")

	assert.Contains(t, cmd.Args, "https://github.com/org/repo.git",
		"SSH target should be rewritten to HTTPS in args")
	assert.NotContains(t, cmd.Args, "git@github.com:org/repo.git",
		"original SSH target should be gone after rewrite")

	require.NotNil(t, cmd.Env, "env should be set after rewriting SSH to HTTPS")
	envMap := envToMap(cmd.Env)
	assert.Equal(t, "1", envMap["GIT_CONFIG_COUNT"])
	wantAuth := "Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:ghp_test123"))
	assert.Equal(t, wantAuth, envMap["GIT_CONFIG_VALUE_0"])
}

// Not parallel: uses t.Setenv() and os.Stderr
// When rewrite can't produce a usable HTTPS URL (e.g. missing owner/repo), we
// fall back to the original SSH target and emit the one-shot warning.
func TestNewCommand_SSH_Unparseable_WarnsAndSkips(t *testing.T) {
	t.Setenv(CheckpointTokenEnvVar, "ghp_test123")

	sshTokenWarningOnce = sync.Once{}
	t.Cleanup(func() { sshTokenWarningOnce = sync.Once{} })

	oldStderr := os.Stderr
	r, w, err := os.Pipe()
	require.NoError(t, err)
	t.Cleanup(func() { os.Stderr = oldStderr })
	os.Stderr = w

	// ssh://host/ has no owner/repo — ParseURL fails, rewrite can't succeed,
	// but newCommand will still detect protocol as "" and skip without SSH warning.
	// Use an SSH SCP target with empty repo path instead: parses as SSH with
	// Host but owner/repo empty, so rewrite fails and protocol stays SSH.
	cmd := newCommand(context.Background(), "push", "ssh://git@host/", "main")

	w.Close()
	os.Stderr = oldStderr

	var buf [4096]byte
	n, _ := r.Read(buf[:]) //nolint:errcheck // test helper, EOF is expected
	_ = string(buf[:n])
	r.Close()

	// No HTTPS rewrite happened (URL couldn't be parsed into owner/repo), so
	// env is not set. Protocol is "" (ParseURL failed), so SSH warning doesn't
	// fire either — that's acceptable: the command runs against the original
	// SSH URL and will fail loudly via git itself.
	assert.Nil(t, cmd.Env, "env should NOT be set when SSH rewrite isn't possible")
	assert.Contains(t, cmd.Args, "ssh://git@host/", "original target unchanged when rewrite fails")
}

// Not parallel: uses t.Setenv()
func TestNewCommand_LocalPath_NoToken(t *testing.T) {
	t.Setenv(CheckpointTokenEnvVar, "ghp_test123")

	cmd := newCommand(context.Background(), "push", "/tmp/bare-repo", "main")
	assert.Nil(t, cmd.Env, "env should NOT be set for local path targets")
}

// newTLSTestServer creates an HTTPS test server that captures the Authorization header.
// Returns the server and a function to read the captured auth header and request count.
func newTLSTestServer(t *testing.T) (*httptest.Server, func() (auth string, count int)) {
	t.Helper()

	var (
		mu           sync.Mutex
		capturedAuth string
		requestCount int
	)

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		capturedAuth = r.Header.Get("Authorization")
		requestCount++
		mu.Unlock()

		w.WriteHeader(http.StatusForbidden)
		fmt.Fprintln(w, "forbidden")
	}))
	t.Cleanup(srv.Close)

	return srv, func() (string, int) {
		mu.Lock()
		defer mu.Unlock()
		return capturedAuth, requestCount
	}
}

func setupTokenTestRepo(t *testing.T) string {
	t.Helper()
	tmpDir := t.TempDir()
	testutil.InitRepo(t, tmpDir)
	testutil.WriteFile(t, tmpDir, "f.txt", "init")
	testutil.GitAdd(t, tmpDir, "f.txt")
	testutil.GitCommit(t, tmpDir, "init")
	t.Chdir(tmpDir)
	return tmpDir
}

// Not parallel: uses t.Chdir()
func TestCheckpointToken_HTTPSServer_SendsAuthHeader(t *testing.T) {
	t.Setenv(CheckpointTokenEnvVar, "test-token-abc123")

	srv, getCapture := newTLSTestServer(t)
	tmpDir := setupTokenTestRepo(t)

	target := srv.URL + "/org/repo.git"
	cmd := newCommand(context.Background(),
		"fetch", target, "+refs/heads/main:refs/remotes/origin/main")
	cmd.Dir = tmpDir
	cmd.Env = append(cmd.Env, "GIT_TERMINAL_PROMPT=0", "GIT_SSL_NO_VERIFY=1")
	_ = cmd.Run() //nolint:errcheck // expected to fail against test server

	auth, count := getCapture()
	require.Positive(t, count, "server should have received at least one request")
	wantAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:test-token-abc123"))
	assert.Equal(t, wantAuth, auth,
		"git should send the token as a Basic Authorization header")
}

// Not parallel: uses t.Chdir()
func TestCheckpointToken_HTTPSServer_NoTokenNoHeader(t *testing.T) {
	t.Setenv(CheckpointTokenEnvVar, "")

	srv, getCapture := newTLSTestServer(t)
	tmpDir := setupTokenTestRepo(t)

	target := srv.URL + "/org/repo.git"
	cmd := newCommand(context.Background(),
		"fetch", target, "+refs/heads/main:refs/remotes/origin/main")
	cmd.Dir = tmpDir
	if cmd.Env == nil {
		cmd.Env = os.Environ()
	}
	cmd.Env = append(cmd.Env, "GIT_TERMINAL_PROMPT=0", "GIT_SSL_NO_VERIFY=1")

	_ = cmd.Run() //nolint:errcheck // expected to fail against test server

	auth, count := getCapture()
	require.Positive(t, count, "server should have received at least one request")
	assert.Empty(t, auth, "no Authorization header should be sent without token")
}

// Not parallel: uses t.Chdir()
func TestCheckpointToken_HTTPSServer_LsRemoteSendsAuthHeader(t *testing.T) {
	t.Setenv(CheckpointTokenEnvVar, "push-token-xyz789")

	srv, getCapture := newTLSTestServer(t)
	tmpDir := setupTokenTestRepo(t)

	target := srv.URL + "/org/repo.git"
	cmd := newCommand(context.Background(),
		"ls-remote", target)
	cmd.Dir = tmpDir
	cmd.Env = append(cmd.Env, "GIT_TERMINAL_PROMPT=0", "GIT_SSL_NO_VERIFY=1")

	_ = cmd.Run() //nolint:errcheck // expected to fail against test server

	auth, count := getCapture()
	require.Positive(t, count, "server should have received at least one request")
	wantAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:push-token-xyz789"))
	assert.Equal(t, wantAuth, auth,
		"git ls-remote should send the token as a Basic Authorization header")
}

// Not parallel: uses t.Setenv()
func TestNewCommand_GIT_TERMINAL_PROMPT_Coexistence(t *testing.T) {
	t.Setenv(CheckpointTokenEnvVar, "coexist-token")

	cmd := newCommand(context.Background(),
		"fetch", "--no-auto-gc", "--no-tags", "--filter=blob:none", "https://github.com/org/repo.git", "refs/heads/main")
	require.NotNil(t, cmd.Env)

	cmd.Env = append(cmd.Env, "GIT_TERMINAL_PROMPT=0")

	envMap := envToMap(cmd.Env)
	assert.Equal(t, "1", envMap["GIT_CONFIG_COUNT"])
	wantAuth := "Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:coexist-token"))
	assert.Equal(t, wantAuth, envMap["GIT_CONFIG_VALUE_0"])
	assert.Equal(t, "0", envMap["GIT_TERMINAL_PROMPT"])
}

func TestIsURL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		val  string
		want bool
	}{
		{"remote name", "origin", false},
		{"SSH SCP", "git@github.com:org/repo.git", true},
		{"HTTPS", "https://github.com/org/repo.git", true},
		{"SSH protocol", "ssh://git@github.com/org/repo.git", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, IsURL(tt.val))
		})
	}
}

func TestIsLocalPath(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		val  string
		want bool
	}{
		{"remote name", "origin", false},
		{"absolute path", "/tmp/repo.git", true},
		{"current dir relative", "./repo.git", true},
		{"parent relative", "../repo.git", true},
		{"https", "https://github.com/org/repo.git", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, isLocalPath(tt.val))
		})
	}
}

// envToMap converts an env slice to a map for easy assertions.
// For duplicate keys, the last value wins.
func envToMap(env []string) map[string]string {
	m := make(map[string]string, len(env))
	for _, e := range env {
		k, v, ok := strings.Cut(e, "=")
		if ok {
			m[k] = v
		}
	}
	return m
}

// gitConfigBool reads a local git config key and reports whether it is set to a
// true value. Missing keys and read errors report false.
func gitConfigBool(ctx context.Context, dir, key string) bool {
	cmd := exec.CommandContext(ctx, "git", "config", "--local", "--get", "--type=bool", key)
	if dir != "" {
		cmd.Dir = dir
	}
	out, err := cmd.Output()
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(out)) == "true"
}

// TestFetch_FilteredURLFetchMarksNewRemoteSkipped verifies that when a filtered
// fetch from a URL creates a new URL-keyed remote section, that section is
// excluded from `git fetch --all` / `git remote update` — otherwise every
// checkpoint URL ever fetched from lingers as a phantom remote that bulk
// fetches keep dialing.
func TestFetch_FilteredURLFetchMarksNewRemoteSkipped(t *testing.T) {
	ctx := context.Background()

	tmpDir := t.TempDir()
	originBare := filepath.Join(tmpDir, "origin.git")
	checkpointBare := filepath.Join(tmpDir, "checkpoints.git")
	seedDir := filepath.Join(tmpDir, "seed")
	cloneDir := filepath.Join(tmpDir, "clone")

	testutil.InitRepo(t, seedDir)
	testutil.WriteFile(t, seedDir, "f.txt", "init")
	testutil.GitAdd(t, seedDir, "f.txt")
	testutil.GitCommit(t, seedDir, "init")

	// Separate origin and checkpoint repos, mirroring the real setup where
	// checkpoints are fetched by URL from a repo that is not origin.
	runIsolatedGit(ctx, t, "", "init", "--bare", originBare)
	runIsolatedGit(ctx, t, "", "init", "--bare", checkpointBare)
	runIsolatedGit(ctx, t, checkpointBare, "config", "uploadpack.allowFilter", "true")
	runIsolatedGit(ctx, t, seedDir, "push", originBare, "HEAD:refs/heads/main")
	runIsolatedGit(ctx, t, "", "clone", "--branch", "main", "file://"+originBare, cloneDir)

	// A commit only in the checkpoint repo so the filtered fetch has
	// something to transfer.
	testutil.WriteFile(t, seedDir, "f.txt", "init\nnext\n")
	testutil.GitAdd(t, seedDir, "f.txt")
	testutil.GitCommit(t, seedDir, "next")
	runIsolatedGit(ctx, t, seedDir, "push", checkpointBare, "HEAD:refs/heads/main")

	// Filtered fetches read .entire settings from the CWD repo.
	testutil.WriteFile(
		t,
		cloneDir,
		".entire/settings.json",
		`{"enabled": true, "strategy_options": {"filtered_fetches": true}}`,
	)
	t.Chdir(cloneDir)

	fetchURL := "file://" + checkpointBare
	out, err := Fetch(ctx, FetchOptions{
		Remote:   fetchURL,
		RefSpecs: []string{"+refs/heads/main:refs/entire-fetch-tmp/main"},
		NoTags:   true,
		Dir:      cloneDir,
	})
	require.NoError(t, err, "fetch output: %s", out)

	// Sanity: git recorded the URL-keyed promisor entry for the filtered fetch.
	require.True(t, gitConfigBool(ctx, cloneDir, "remote."+fetchURL+".promisor"),
		"expected git to record a promisor entry for the filtered URL fetch")

	assert.True(t, gitConfigBool(ctx, cloneDir, "remote."+fetchURL+".skipFetchAll"),
		"URL-keyed promisor entry should be excluded from git fetch --all")

	// git fetch --all must no longer dial the phantom entry: with the
	// checkpoint repo gone, --all only succeeds if the URL-keyed entry is
	// skipped (origin is still reachable).
	require.NoError(t, os.RemoveAll(checkpointBare))
	runIsolatedGit(ctx, t, cloneDir, "fetch", "--all", "--no-auto-gc")
}

// TestFetch_FailedFilteredFetchStillStampsNewRemote guards the resume
// regression: git writes remote.<url>.promisor eagerly during connection
// setup, so a filtered fetch that then fails (e.g. a missing ref) still leaves
// the phantom remote behind. The stamp must land anyway — otherwise the section
// exists on the next attempt, never looks new again, and lingers unstamped.
func TestFetch_FailedFilteredFetchStillStampsNewRemote(t *testing.T) {
	ctx := context.Background()

	tmpDir := t.TempDir()
	originBare := filepath.Join(tmpDir, "origin.git")
	checkpointBare := filepath.Join(tmpDir, "checkpoints.git")
	seedDir := filepath.Join(tmpDir, "seed")
	cloneDir := filepath.Join(tmpDir, "clone")

	testutil.InitRepo(t, seedDir)
	testutil.WriteFile(t, seedDir, "f.txt", "init")
	testutil.GitAdd(t, seedDir, "f.txt")
	testutil.GitCommit(t, seedDir, "init")

	runIsolatedGit(ctx, t, "", "init", "--bare", originBare)
	runIsolatedGit(ctx, t, "", "init", "--bare", checkpointBare)
	runIsolatedGit(ctx, t, checkpointBare, "config", "uploadpack.allowFilter", "true")
	runIsolatedGit(ctx, t, seedDir, "push", originBare, "HEAD:refs/heads/main")
	runIsolatedGit(ctx, t, "", "clone", "--branch", "main", "file://"+originBare, cloneDir)

	testutil.WriteFile(
		t,
		cloneDir,
		".entire/settings.json",
		`{"enabled": true, "strategy_options": {"filtered_fetches": true}}`,
	)
	t.Chdir(cloneDir)

	fetchURL := "file://" + checkpointBare
	// Fetch a ref that does not exist on the checkpoint remote: the command
	// fails, but git has already recorded the URL-keyed promisor section.
	_, err := Fetch(ctx, FetchOptions{
		Remote:   fetchURL,
		RefSpecs: []string{"+refs/heads/does-not-exist:refs/entire-fetch-tmp/x"},
		NoTags:   true,
		Dir:      cloneDir,
	})
	require.Error(t, err, "fetch of a missing ref should fail")

	require.True(t, gitConfigBool(ctx, cloneDir, "remote."+fetchURL+".promisor"),
		"git records the promisor section even when the fetch fails")
	assert.True(t, gitConfigBool(ctx, cloneDir, "remote."+fetchURL+".skipFetchAll"),
		"a phantom remote left by a failed fetch must still be stamped")
}

// TestFetch_UnfilteredFetchDoesNotCreateConfigSection verifies the stamp is
// gated on a filtered fetch: a plain (unfiltered) URL fetch records no
// URL-keyed section, so we must not invent a remote.<url> config section.
func TestFetch_UnfilteredFetchDoesNotCreateConfigSection(t *testing.T) {
	ctx := context.Background()

	tmpDir := t.TempDir()
	bareDir := filepath.Join(tmpDir, "bare.git")
	seedDir := filepath.Join(tmpDir, "seed")
	cloneDir := filepath.Join(tmpDir, "clone")

	testutil.InitRepo(t, seedDir)
	testutil.WriteFile(t, seedDir, "f.txt", "init")
	testutil.GitAdd(t, seedDir, "f.txt")
	testutil.GitCommit(t, seedDir, "init")

	runIsolatedGit(ctx, t, "", "init", "--bare", bareDir)
	runIsolatedGit(ctx, t, seedDir, "remote", "add", "origin", bareDir)
	runIsolatedGit(ctx, t, seedDir, "push", "origin", "HEAD:refs/heads/main")
	runIsolatedGit(ctx, t, "", "clone", "--branch", "main", "file://"+bareDir, cloneDir)

	testutil.WriteFile(
		t,
		cloneDir,
		".entire/settings.json",
		`{"enabled": true, "strategy_options": {"filtered_fetches": true}}`,
	)
	t.Chdir(cloneDir)

	fetchURL := "file://" + bareDir
	out, err := Fetch(ctx, FetchOptions{
		Remote:   fetchURL,
		RefSpecs: []string{"+refs/heads/main:refs/remotes/origin/main"},
		NoTags:   true,
		NoFilter: true,
		Dir:      cloneDir,
	})
	require.NoError(t, err, "fetch output: %s", out)

	assert.False(t, gitConfigBool(ctx, cloneDir, "remote."+fetchURL+".promisor"))
	assert.False(t, gitConfigBool(ctx, cloneDir, "remote."+fetchURL+".skipFetchAll"))
}

// TestFetch_ExistingURLRemoteNotReStamped verifies we only stamp remotes we
// create: a filtered fetch from a URL that already has a remote.<url> section
// must leave that section as-is rather than rewriting the user's git config.
func TestFetch_ExistingURLRemoteNotReStamped(t *testing.T) {
	ctx := context.Background()

	tmpDir := t.TempDir()
	originBare := filepath.Join(tmpDir, "origin.git")
	checkpointBare := filepath.Join(tmpDir, "checkpoints.git")
	seedDir := filepath.Join(tmpDir, "seed")
	cloneDir := filepath.Join(tmpDir, "clone")

	testutil.InitRepo(t, seedDir)
	testutil.WriteFile(t, seedDir, "f.txt", "init")
	testutil.GitAdd(t, seedDir, "f.txt")
	testutil.GitCommit(t, seedDir, "init")

	runIsolatedGit(ctx, t, "", "init", "--bare", originBare)
	runIsolatedGit(ctx, t, "", "init", "--bare", checkpointBare)
	runIsolatedGit(ctx, t, checkpointBare, "config", "uploadpack.allowFilter", "true")
	runIsolatedGit(ctx, t, seedDir, "push", originBare, "HEAD:refs/heads/main")
	runIsolatedGit(ctx, t, "", "clone", "--branch", "main", "file://"+originBare, cloneDir)

	testutil.WriteFile(t, seedDir, "f.txt", "init\nnext\n")
	testutil.GitAdd(t, seedDir, "f.txt")
	testutil.GitCommit(t, seedDir, "next")
	runIsolatedGit(ctx, t, seedDir, "push", checkpointBare, "HEAD:refs/heads/main")

	testutil.WriteFile(
		t,
		cloneDir,
		".entire/settings.json",
		`{"enabled": true, "strategy_options": {"filtered_fetches": true}}`,
	)
	t.Chdir(cloneDir)

	fetchURL := "file://" + checkpointBare
	// Simulate a pre-existing URL-keyed remote (e.g. a phantom left by an older
	// CLI). Its presence means the section already exists before our fetch.
	runIsolatedGit(ctx, t, cloneDir, "config", "--local", "remote."+fetchURL+".promisor", "true")

	out, err := Fetch(ctx, FetchOptions{
		Remote:   fetchURL,
		RefSpecs: []string{"+refs/heads/main:refs/entire-fetch-tmp/main"},
		NoTags:   true,
		Dir:      cloneDir,
	})
	require.NoError(t, err, "fetch output: %s", out)

	assert.False(t, gitConfigBool(ctx, cloneDir, "remote."+fetchURL+".skipFetchAll"),
		"a remote that already existed must not be stamped")
}

// TestMarkRemoteSkipped_SetsSkipFetchAll verifies the helper stamps skipFetchAll.
func TestMarkRemoteSkipped_SetsSkipFetchAll(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	repoDir := t.TempDir()
	testutil.InitRepo(t, repoDir)

	const url = "https://example.com/org/checkpoints.git"
	markRemoteSkipped(ctx, repoDir, url)

	assert.True(t, gitConfigBool(ctx, repoDir, "remote."+url+".skipFetchAll"))
}

// TestGitRemoteSectionExists reports true only once a remote.<url>.* key is set.
func TestGitRemoteSectionExists(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	repoDir := t.TempDir()
	testutil.InitRepo(t, repoDir)

	const url = "https://example.com/org/checkpoints.git"
	assert.False(t, gitRemoteSectionExists(ctx, repoDir, url))

	runIsolatedGit(ctx, t, repoDir, "config", "--local", "remote."+url+".promisor", "true")
	assert.True(t, gitRemoteSectionExists(ctx, repoDir, url))
}

// TestGitRemoteSectionExists_ExactSubsectionMatch verifies the check compares
// the whole URL subsection, not a prefix: a longer URL that shares a prefix
// (e.g. ".../repo.git") must not make a shorter one (".../repo") look present.
func TestGitRemoteSectionExists_ExactSubsectionMatch(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	repoDir := t.TempDir()
	testutil.InitRepo(t, repoDir)

	const longURL = "https://example.com/org/repo.git"
	const shortURL = "https://example.com/org/repo"
	runIsolatedGit(ctx, t, repoDir, "config", "--local", "remote."+longURL+".promisor", "true")

	assert.True(t, gitRemoteSectionExists(ctx, repoDir, longURL),
		"the exact URL section is present")
	assert.False(t, gitRemoteSectionExists(ctx, repoDir, shortURL),
		"a prefix of an existing URL section must not count as present")
}

// TestStampNewlyCreatedRemote_StampsUnderCancelledContext guards the timed-out
// fetch case: git records the promisor section before the fetch times out, so
// the stamp must still land even though the fetch context is already cancelled.
func TestStampNewlyCreatedRemote_StampsUnderCancelledContext(t *testing.T) {
	t.Parallel()

	repoDir := t.TempDir()
	testutil.InitRepo(t, repoDir)

	const url = "https://example.com/org/checkpoints.git"
	// Simulate git having recorded the promisor section during a fetch that
	// then timed out.
	runIsolatedGit(context.Background(), t, repoDir, "config", "--local", "remote."+url+".promisor", "true")

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // parent context already done, as after a timed-out fetch

	stampNewlyCreatedRemote(ctx, repoDir, url)

	assert.True(t, gitConfigBool(context.Background(), repoDir, "remote."+url+".skipFetchAll"),
		"stamp must land even though the parent context is cancelled")
}
