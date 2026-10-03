package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/entireio/cli/internal/entireclient/userdirs"
	"github.com/stretchr/testify/require"
)

func TestAdoptPathAuthorizer_ReusesHomeTrustButChecksEachPath(t *testing.T) {
	// Historical home: isolate the current home and the per-user registry.
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	worktree := t.TempDir()
	home := t.TempDir()
	rememberAdoptHome(t, home)
	first := filepath.Join(adoptClaudeProject(t, home, worktree), "first.jsonl")
	second := filepath.Join(adoptClaudeProject(t, home, worktree), "second.jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(first), 0o700))
	require.NoError(t, os.WriteFile(first, []byte("{}\n"), 0o600))
	require.NoError(t, os.WriteFile(second, []byte("{}\n"), 0o600))
	authorizer := newAdoptPathAuthorizer(worktree)
	_, _, accepted := authorizer.recordedHomeAcceptsPath(home, agent.AgentTypeClaudeCode, first)
	require.True(t, accepted)
	// Trust is resolved once per operation; subsequent task records do not
	// need to reread the registry. The next operation must resolve it again.
	root, err := userdirs.ConfigRootForRead()
	require.NoError(t, err)
	require.NoError(t, root.Remove("agent_homes.json"))
	_, _, accepted = authorizer.recordedHomeAcceptsPath(home, agent.AgentTypeClaudeCode, second)
	require.True(t, accepted)
	_, _, accepted = newAdoptPathAuthorizer(worktree).recordedHomeAcceptsPath(home, agent.AgentTypeClaudeCode, second)
	require.False(t, accepted)

	victim := filepath.Join(t.TempDir(), "victim.jsonl")
	require.NoError(t, os.WriteFile(victim, []byte("{}\n"), 0o600))
	require.NoError(t, os.Remove(second))
	if err := os.Symlink(victim, second); err != nil {
		t.Skipf("symlink not supported: %v", err)
	}
	_, _, accepted = authorizer.recordedHomeAcceptsPath(home, agent.AgentTypeClaudeCode, second)
	require.False(t, accepted, "cached home trust must not authorize a swapped transcript link")
}

func TestAdoptPathAuthorizer_OverrideCannotDowngradeRecordedHome(t *testing.T) {
	home := t.TempDir()
	rememberAdoptHome(t, home)
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	project := filepath.Join(home, "projects", "project")
	t.Setenv("ENTIRE_TEST_CLAUDE_PROJECT_DIR", project)
	path := filepath.Join(project, "session.jsonl")
	testutil.WriteFile(t, home, "projects/project/session.jsonl", "{}\n")
	outside := t.TempDir()
	testutil.WriteFile(t, outside, "private.jsonl", "private\n")
	require.NoError(t, os.Remove(path))
	testutil.SkipWithoutSymlinks(t)
	require.NoError(t, os.Symlink(filepath.Join(outside, "private.jsonl"), path))
	source := &session.State{AgentType: agent.AgentTypeClaudeCode, AgentHome: home, TranscriptPath: path,
		TaskRecords: []session.TaskRecord{{DeclaredTranscriptPath: path}}}
	require.Error(t, validateAdoptSourceTranscript(source, t.TempDir()))
	newAdoptPathAuthorizer(t.TempDir()).validateTaskRecords(source)
	require.Empty(t, source.TaskRecords[0].DeclaredTranscriptPath)
	source.AgentType = ""
	source.TaskRecords[0].DeclaredTranscriptPath = path
	require.Error(t, validateAdoptSourceTranscript(source, t.TempDir()), "missing agent identity cannot downgrade a recorded home")
	newAdoptPathAuthorizer(t.TempDir()).validateTaskRecords(source)
	require.Empty(t, source.TaskRecords[0].DeclaredTranscriptPath)
}
