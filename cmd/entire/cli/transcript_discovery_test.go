package cli

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/stretchr/testify/require"
)

func TestActiveDiscovery_SkipsUnreadableTranscript(t *testing.T) {
	setupAttachTestRepo(t)
	isolateAgentHomesRegistry(t)
	for _, env := range agent.CallerSessionEnvVars() {
		t.Setenv(env, "")
	}
	const id = "review-unreadable"
	active, historical := t.TempDir(), t.TempDir()
	ag := newAltHomeAgent(t, active)
	require.NoError(t, agent.RememberAgentHome(ag.Type(), historical))
	valid := writeClaudeTranscriptUnder(t, historical, id)
	bad := filepath.Join(active, "projects", "active-project", id+".jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(bad), 0700))
	require.NoError(t, os.WriteFile(bad, []byte("{}\n"), 0000))
	t.Cleanup(func() { require.NoError(t, os.Chmod(bad, 0600)) })
	if _, err := os.ReadFile(bad); err == nil {
		t.Skip("permissions not enforced")
	}
	path, home, err := resolveAndValidateTranscript(context.Background(), id, ag, lookupLocalOnly)
	require.NoError(t, err)
	require.Equal(t, valid, path)
	require.Equal(t, historical, home)
	_, err = readAttachTranscript(ag, path, home)
	require.NoError(t, err)
	fallback, _, err := searchTranscriptInProjectDirs(id, ag)
	require.NoError(t, err)
	require.Equal(t, valid, fallback)
}
func TestActiveDiscovery_CodexArchive(t *testing.T) {
	setupAttachTestRepo(t)
	isolateAgentHomesRegistry(t)
	for _, env := range agent.CallerSessionEnvVars() {
		t.Setenv(env, "")
	}
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	t.Setenv("ENTIRE_TEST_CODEX_SESSION_DIR", "")
	ag, err := agent.Get(agent.AgentNameCodex)
	require.NoError(t, err)
	const id = "12345678-1234-1234-1234-123456789abc"
	archived := filepath.Join(home, "archived_sessions", "2026", "10", "03", "rollout-2026-10-03-"+id+".jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(archived), 0700))
	require.NoError(t, os.WriteFile(archived, []byte("{}\n"), 0600))
	require.NoError(t, agent.RememberAgentHome(ag.Type(), home))
	path, foundHome, err := resolveAndValidateTranscript(t.Context(), id, ag, lookupLocalOnly)
	require.NoError(t, err)
	require.Equal(t, archived, path)
	require.Empty(t, foundHome)
}
func TestHistoricalDiscovery_PiSkipsUnsafeLatest(t *testing.T) {
	testutil.SkipWithoutSymlinks(t)
	setupAttachTestRepo(t)
	isolateAgentHomesRegistry(t)
	for _, env := range agent.CallerSessionEnvVars() {
		t.Setenv(env, "")
	}
	t.Setenv("PI_CODING_AGENT_DIR", t.TempDir())
	t.Setenv("ENTIRE_TEST_PI_SESSION_DIR", "")
	ag, err := agent.Get(agent.AgentNamePi)
	require.NoError(t, err)
	provider, ok := agent.AsAgentHomeProvider(ag)
	require.True(t, ok)
	home := t.TempDir()
	require.NoError(t, agent.RememberAgentHome(ag.Type(), home))
	const id = "review-pi-multiple"
	project := filepath.Join(provider.SessionBaseDirUnder(home), "--review-project--")
	require.NoError(t, os.MkdirAll(project, 0700))
	valid := filepath.Join(project, "2025-01-01_"+id+".jsonl")
	bad := filepath.Join(project, "2026-01-01_"+id+".jsonl")
	require.NoError(t, os.WriteFile(valid, []byte("{}\n"), 0600))
	outside := filepath.Join(t.TempDir(), "outside.jsonl")
	require.NoError(t, os.WriteFile(outside, []byte("{}\n"), 0600))
	require.NoError(t, os.Symlink(outside, bad))
	got, _, err := searchTranscriptInProjectDirs(id, ag)
	require.NoError(t, err)
	require.Equal(t, valid, got)
}

func TestActiveDiscovery_LinkedSessionDirectoryKeepsLegacyProtocol(t *testing.T) {
	testutil.SkipWithoutSymlinks(t)
	setupAttachTestRepo(t)
	isolateAgentHomesRegistry(t)
	home, relocated := t.TempDir(), t.TempDir()
	ag := newAltHomeAgent(t, home)
	const id = "linked-project-session"
	testutil.WriteFile(t, relocated, filepath.Join("active-project", id+".jsonl"), "{}\n")
	require.NoError(t, os.Symlink(relocated, filepath.Join(home, "projects")))
	path, foundHome, err := resolveAndValidateTranscript(t.Context(), id, ag, lookupLocalOnly)
	require.NoError(t, err)
	require.Equal(t, filepath.Join(home, "projects", "active-project", id+".jsonl"), path)
	require.Empty(t, foundHome)
	require.Empty(t, attachAgentHome(ag, path, foundHome))
	data, err := readAttachTranscript(ag, path, foundHome)
	require.NoError(t, err)
	require.Equal(t, "{}\n", string(data))
}

func TestActiveDiscovery_MultiMatchAgentsSkipUnsafeLatest(t *testing.T) {
	for _, name := range []types.AgentName{agent.AgentNameCodex, agent.AgentNamePi} {
		t.Run(string(name), func(t *testing.T) {
			testutil.SkipWithoutSymlinks(t)
			setupAttachTestRepo(t)
			isolateAgentHomesRegistry(t)
			for _, env := range agent.RelocationEnvVars() {
				t.Setenv(env, "")
			}
			home := t.TempDir()
			t.Setenv("CODEX_HOME", home)
			t.Setenv("PI_CODING_AGENT_DIR", home)
			t.Setenv("ENTIRE_TEST_CODEX_SESSION_DIR", "")
			t.Setenv("ENTIRE_TEST_PI_SESSION_DIR", "")
			ag, err := agent.Get(name)
			require.NoError(t, err)
			const id = "12345678-1234-1234-1234-123456789abc"
			root, err := paths.WorktreeRoot(t.Context())
			require.NoError(t, err)
			dir, err := ag.GetSessionDir(root)
			require.NoError(t, err)
			var oldName, newName string
			if name == agent.AgentNamePi {
				oldName, newName = "2026-10-01_"+id+".jsonl", "2026-10-02_"+id+".jsonl"
			} else {
				oldName, newName = "rollout-2026-10-01-"+id+".jsonl", "rollout-2026-10-02-"+id+".jsonl"
			}
			testutil.WriteFile(t, dir, oldName, "{}\n")
			good := filepath.Join(dir, oldName)
			require.NoError(t, os.Symlink(good, filepath.Join(dir, newName)))
			path, _, err := resolveAndValidateTranscript(t.Context(), id, ag, lookupLocalOnly)
			require.NoError(t, err)
			require.Equal(t, good, path)
		})
	}
}

func TestActiveDiscovery_CursorFallsBackToFlatLayout(t *testing.T) {
	for _, kind := range []string{"missing", "directory", "symlink", "unreadable"} {
		t.Run(kind, func(t *testing.T) {
			setupAttachTestRepo(t)
			dir := t.TempDir()
			t.Setenv("ENTIRE_TEST_CURSOR_PROJECT_DIR", dir)
			const id = "cursor-alternate-layout"
			flat := filepath.Join(dir, id+".jsonl")
			nested := filepath.Join(dir, id, id+".jsonl")
			testutil.WriteFile(t, dir, id+".jsonl", "{}\n")
			require.NoError(t, os.MkdirAll(filepath.Dir(nested), 0o700))
			switch kind {
			case "directory":
				require.NoError(t, os.Mkdir(nested, 0o700))
			case "symlink":
				testutil.SkipWithoutSymlinks(t)
				require.NoError(t, os.Symlink(flat, nested))
			case "unreadable":
				require.NoError(t, os.WriteFile(nested, []byte("{}\n"), 0o000))
				t.Cleanup(func() { require.NoError(t, os.Chmod(nested, 0o600)) })
				if _, err := os.ReadFile(nested); err == nil {
					t.Skip("permissions not enforced")
				}
			}
			ag, err := agent.Get(agent.AgentNameCursor)
			require.NoError(t, err)
			path, err := discoverActiveTranscript(t.Context(), id, ag)
			require.NoError(t, err)
			require.Equal(t, flat, path)
		})
	}
}

func TestActiveDiscovery_PreservesEntireCacheBoundary(t *testing.T) {
	testutil.SkipWithoutSymlinks(t)
	repo, outside := t.TempDir(), t.TempDir()
	testutil.InitRepo(t, repo)
	t.Chdir(repo)
	const id = "protected-cache-session"
	testutil.WriteFile(t, outside, filepath.Join("tmp", "cursor", id+".jsonl"), "private\n")
	require.NoError(t, os.Symlink(outside, filepath.Join(repo, ".entire")))
	t.Setenv("ENTIRE_TEST_CURSOR_PROJECT_DIR", filepath.Join(repo, ".entire", "tmp", "cursor"))
	ag, err := agent.Get(agent.AgentNameCursor)
	require.NoError(t, err)
	path, err := discoverActiveTranscript(t.Context(), id, ag)
	require.NoError(t, err)
	require.Empty(t, path, "discovery must not open a cached transcript through a linked .entire root")
}
