package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os/exec"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

const testMirrorURL = "entire://aws-us-east-2.entire.io/gh/octocat/hello-world"

func addOriginRemote(t *testing.T, dir, url string) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", "remote", "add", "origin", url)
	cmd.Dir = dir
	require.NoError(t, cmd.Run(), "add origin remote")
}

// initRepoWithRemotes creates a temp git repo with the given remotes configured.
func initRepoWithRemotes(t *testing.T, remotes map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	for name, url := range remotes {
		cmd := exec.CommandContext(t.Context(), "git", "remote", "add", name, url)
		cmd.Dir = dir
		require.NoError(t, cmd.Run(), "add remote %q", name)
	}
	return dir
}

func TestDetectMirrorClone(t *testing.T) {
	t.Parallel()

	t.Run("detects an entire:// origin", func(t *testing.T) {
		t.Parallel()
		dir := initRepoWithRemotes(t, map[string]string{"origin": testMirrorURL})
		m := detectMirrorClone(t.Context(), dir)
		require.NotNil(t, m)
		require.Equal(t, "origin", m.Remote)
		require.Equal(t, "aws-us-east-2.entire.io", m.Cluster)
		require.Equal(t, "octocat", m.Owner)
		require.Equal(t, "hello-world", m.Repo)
	})

	t.Run("nil for a forge origin", func(t *testing.T) {
		t.Parallel()
		dir := initRepoWithRemotes(t, map[string]string{"origin": "git@github.com:octocat/hello-world.git"})
		require.Nil(t, detectMirrorClone(t.Context(), dir))
	})

	t.Run("nil when there are no remotes", func(t *testing.T) {
		t.Parallel()
		dir := initRepoWithRemotes(t, nil)
		require.Nil(t, detectMirrorClone(t.Context(), dir))
	})

	t.Run("detects an entire:// side remote when origin is a forge", func(t *testing.T) {
		t.Parallel()
		dir := initRepoWithRemotes(t, map[string]string{
			"origin": "git@github.com:octocat/hello-world.git",
			"entire": testMirrorURL,
		})
		m := detectMirrorClone(t.Context(), dir)
		require.NotNil(t, m)
		require.Equal(t, "entire", m.Remote)
	})

	t.Run("prefers origin when it too is a mirror", func(t *testing.T) {
		t.Parallel()
		dir := initRepoWithRemotes(t, map[string]string{
			"origin": testMirrorURL,
			"entire": "entire://aws-eu-central-1.entire.io/gh/octocat/hello-world",
		})
		m := detectMirrorClone(t.Context(), dir)
		require.NotNil(t, m)
		require.Equal(t, "origin", m.Remote, "origin wins the tie")
		require.Equal(t, "aws-us-east-2.entire.io", m.Cluster)
	})
}

func TestFormatMirrorStatusLine(t *testing.T) {
	t.Parallel()
	sty := newStatusStyles(&bytes.Buffer{}) // color disabled → plain text
	m := &mirrorClone{Remote: "origin", Cluster: "aws-us-east-2.entire.io", Owner: "octocat", Repo: "hello-world", URL: testMirrorURL}

	t.Run("ready status", func(t *testing.T) {
		t.Parallel()
		out := formatMirrorStatusLine(m, "ready", true, sty)
		require.Contains(t, out, "Mirror")
		require.Contains(t, out, "aws-us-east-2.entire.io")
		require.Contains(t, out, "ready")
		require.Contains(t, out, "origin")
	})

	t.Run("unknown when logged out points at login", func(t *testing.T) {
		t.Parallel()
		out := formatMirrorStatusLine(m, mirrorStatusUnknown, false, sty)
		require.Contains(t, out, "unknown")
		require.Contains(t, out, "entire login")
	})

	t.Run("unknown when logged in gives no login hint", func(t *testing.T) {
		t.Parallel()
		out := formatMirrorStatusLine(m, mirrorStatusUnknown, true, sty)
		require.Contains(t, out, "unknown")
		require.NotContains(t, out, "entire login")
	})
}

func TestStatusLoggedIn(t *testing.T) {
	// Not parallel: mutates process env.
	t.Run("env token counts as logged in", func(t *testing.T) {
		t.Setenv("ENTIRE_TOKEN", "some-token")
		require.True(t, statusLoggedIn())
	})

	t.Run("no token and no context is logged out", func(t *testing.T) {
		t.Setenv("ENTIRE_TOKEN", "")
		t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir()) // empty config → no active context
		require.False(t, statusLoggedIn())
	})
}

// stubMirrorStatus replaces the network-backed resolver for a test.
func stubMirrorStatus(t *testing.T, status string, loggedIn bool) {
	t.Helper()
	prev := resolveMirrorStatus
	resolveMirrorStatus = func(context.Context, *mirrorClone) (string, bool) {
		return status, loggedIn
	}
	t.Cleanup(func() { resolveMirrorStatus = prev })
}

func TestRunStatus_ShowsMirror(t *testing.T) {
	dir := setupTestDir(t)
	testutil.InitRepo(t, dir)
	addOriginRemote(t, dir, testMirrorURL)
	writeSettings(t, testSettingsEnabled)
	stubMirrorStatus(t, "ready", true)

	var stdout bytes.Buffer
	require.NoError(t, runStatus(context.Background(), &stdout, false, false))
	out := stdout.String()
	require.Contains(t, out, "Mirror")
	require.Contains(t, out, "aws-us-east-2.entire.io")
	require.Contains(t, out, "ready")
}

func TestRunStatus_HintWhenNotMirrored(t *testing.T) {
	dir := setupTestDir(t)
	testutil.InitRepo(t, dir)
	addOriginRemote(t, dir, "git@github.com:octocat/hello-world.git")
	writeSettings(t, testSettingsEnabled)
	// A forge clone is not pointed at a mirror, so the live-status resolver must
	// never run — the hint is derived purely locally.
	prev := resolveMirrorStatus
	resolveMirrorStatus = func(context.Context, *mirrorClone) (string, bool) {
		t.Fatal("resolveMirrorStatus called for a non-mirror clone")
		return "", false
	}
	t.Cleanup(func() { resolveMirrorStatus = prev })

	var stdout bytes.Buffer
	require.NoError(t, runStatus(context.Background(), &stdout, false, false))
	out := stdout.String()
	require.Contains(t, out, "github.com")
	require.Contains(t, out, "not through an Entire mirror")
	require.Contains(t, out, "entire repo mirror use")
}

func TestRunStatus_NoHintForNonGitHubRemote(t *testing.T) {
	dir := setupTestDir(t)
	testutil.InitRepo(t, dir)
	addOriginRemote(t, dir, "git@gitlab.com:acme/app.git")
	writeSettings(t, testSettingsEnabled)

	var stdout bytes.Buffer
	require.NoError(t, runStatus(context.Background(), &stdout, false, false))
	out := stdout.String()
	require.NotContains(t, out, "mirror use", "mirrors are GitHub-only; no hint for a GitLab remote")
}

func TestMirrorableForgeHost(t *testing.T) {
	t.Parallel()

	t.Run("returns the host for a github forge origin", func(t *testing.T) {
		t.Parallel()
		dir := initRepoWithRemotes(t, map[string]string{"origin": "https://github.com/octocat/hello-world"})
		host, ok := mirrorableForgeHost(t.Context(), dir)
		require.True(t, ok)
		require.Equal(t, "github.com", host)
	})

	t.Run("false for a non-mirrorable host", func(t *testing.T) {
		t.Parallel()
		dir := initRepoWithRemotes(t, map[string]string{"origin": "git@gitlab.com:acme/app.git"})
		_, ok := mirrorableForgeHost(t.Context(), dir)
		require.False(t, ok, "mirrors are GitHub-only today")
	})

	t.Run("false when already a mirror", func(t *testing.T) {
		t.Parallel()
		dir := initRepoWithRemotes(t, map[string]string{"origin": testMirrorURL})
		_, ok := mirrorableForgeHost(t.Context(), dir)
		require.False(t, ok, "an entire:// remote is already a mirror")
	})

	t.Run("false with no remotes", func(t *testing.T) {
		t.Parallel()
		dir := initRepoWithRemotes(t, nil)
		_, ok := mirrorableForgeHost(t.Context(), dir)
		require.False(t, ok)
	})
}

func TestFormatMirrorHint(t *testing.T) {
	t.Parallel()
	sty := newStatusStyles(&bytes.Buffer{})
	out := formatMirrorHint("github.com", sty)
	require.Contains(t, out, "github.com")
	require.Contains(t, out, "not through an Entire mirror")
	require.Contains(t, out, "entire repo mirror use")
}

func TestRunStatusJSON_Mirror(t *testing.T) {
	dir := setupTestDir(t)
	testutil.InitRepo(t, dir)
	addOriginRemote(t, dir, testMirrorURL)
	writeSettings(t, testSettingsEnabled)
	stubMirrorStatus(t, "ready", true)

	var stdout bytes.Buffer
	require.NoError(t, runStatusJSON(context.Background(), &stdout))

	var got statusJSON
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &got))
	require.NotNil(t, got.Mirror)
	require.Equal(t, "origin", got.Mirror.Remote)
	require.Equal(t, "aws-us-east-2.entire.io", got.Mirror.Cluster)
	require.Equal(t, "octocat", got.Mirror.Owner)
	require.Equal(t, "hello-world", got.Mirror.Repo)
	require.Equal(t, "ready", got.Mirror.Status)
	require.True(t, got.Mirror.LoggedIn)
}

func TestRunStatusJSON_NoMirror(t *testing.T) {
	dir := setupTestDir(t)
	testutil.InitRepo(t, dir)
	addOriginRemote(t, dir, "git@github.com:octocat/hello-world.git")
	writeSettings(t, testSettingsEnabled)

	var stdout bytes.Buffer
	require.NoError(t, runStatusJSON(context.Background(), &stdout))
	require.NotContains(t, stdout.String(), "\"mirror\"")
	var got statusJSON
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &got))
	require.Nil(t, got.Mirror)
}
