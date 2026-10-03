package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/stretchr/testify/require"
)

func TestAdoptRepeated(t *testing.T) {
	for _, kind := range []types.AgentType{agent.AgentTypeClaudeCode, agent.AgentTypeFactoryAIDroid, agent.AgentTypePi} {
		for _, shared := range []bool{false, true} {
			label := string(kind) + "/external"
			if shared {
				label = string(kind) + "/shared"
			}
			t.Run(label, func(t *testing.T) {
				for _, name := range agent.CallerSessionEnvVars() {
					t.Setenv(name, "")
				}
				for _, name := range agent.RelocationEnvVars() {
					t.Setenv(name, "")
				}
				t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())
				first := setupAdoptRepo(t)
				var second, third string
				if shared {
					second, third = filepath.Join(t.TempDir(), "second"), filepath.Join(t.TempDir(), "third")
					runAdoptGit(t, first, "worktree", "add", second, "-b", "second")
					runAdoptGit(t, first, "worktree", "add", third, "-b", "third")
					var err error
					second, err = filepath.EvalSymlinks(second)
					require.NoError(t, err)
					third, err = filepath.EvalSymlinks(third)
					require.NoError(t, err)
				} else {
					second, third = setupAdoptRepo(t), setupAdoptRepo(t)
				}
				home, err := filepath.EvalSymlinks(t.TempDir())
				require.NoError(t, err)
				require.NoError(t, agent.RememberAgentHome(kind, home))
				ag, err := agent.GetByAgentType(kind)
				require.NoError(t, err)
				provider, ok := ag.(agent.WorktreeSessionDirProvider)
				require.True(t, ok)
				const id = "repeat-adopt"
				project := provider.SessionDirUnder(home, first)
				testutil.WriteFile(t, project, id+".jsonl", "{}\n")
				transcript := filepath.Join(project, id+".jsonl")
				task := filepath.Join(project, "task.jsonl")
				store := session.NewStateStoreWithDir(filepath.Join(first, ".git", session.SessionStateDirName))
				last := time.Now()
				require.NoError(t, store.Save(t.Context(), &session.State{SessionID: id, AgentType: kind, AgentHome: home,
					TranscriptPath: transcript, WorktreePath: first, Phase: session.PhaseActive,
					TaskRecords: []session.TaskRecord{{AgentID: "task", DeclaredTranscriptPath: task}},
					StartedAt:   last, LastInteractionTime: &last, BaseCommit: testutil.GetHeadHash(t, first)}))
				t.Chdir(second)
				var output bytes.Buffer
				require.NoError(t, runAdopt(t.Context(), &output, id, adoptOptions{FromWorktree: first, Force: true}))
				t.Chdir(first)
				moved := filepath.Join(t.TempDir(), "moved")
				if shared {
					runAdoptGit(t, first, "worktree", "move", second, moved)
				} else {
					// Main repositories move their .git directory too. Reconcile
					// state as a resumed turn does, without any receipt migration.
					require.NoError(t, os.Rename(second, moved))
					movedStore := session.NewStateStoreWithDir(filepath.Join(moved, ".git", session.SessionStateDirName))
					state, loadErr := movedStore.Load(t.Context(), id)
					require.NoError(t, loadErr)
					state.WorktreePath = moved
					require.NoError(t, movedStore.Save(t.Context(), state))
				}
				second, err = filepath.EvalSymlinks(moved)
				require.NoError(t, err)
				t.Chdir(third)
				require.NoError(t, runAdopt(t.Context(), &output, id, adoptOptions{FromWorktree: second, Force: true}))
				target, _, _, err := stateStoreForWorktree(t.Context(), third)
				require.NoError(t, err)
				state, err := target.Load(t.Context(), id)
				require.NoError(t, err)
				require.Equal(t, third, state.WorktreePath)
				require.Equal(t, transcript, state.TranscriptPath)
				require.Equal(t, task, state.TaskRecords[0].DeclaredTranscriptPath)
				// Hooks may supply the linked spelling of the same independently trusted home.
				alias := filepath.Join(t.TempDir(), "home")
				if os.Symlink(home, alias) == nil {
					rel, err := filepath.Rel(home, transcript)
					require.NoError(t, err)
					state.AgentHome, state.TranscriptPath = alias, filepath.Join(alias, rel)
					require.NoError(t, validateAdoptSourceTranscript(state, third))
					require.Equal(t, home, state.AgentHome)
					require.Equal(t, transcript, state.TranscriptPath)
				}
				// Transcript ownership is independent of the current worktree,
				// but metadata cannot select a different session or arbitrary home.
				other := setupAdoptRepo(t)
				state.WorktreeID = "third"
				require.NoError(t, validateAdoptSourceTranscript(state, other))
				state.TranscriptPath = filepath.Join(project, "other.jsonl")
				require.Error(t, validateAdoptSourceTranscript(state, third))
				state.TranscriptPath = transcript
				state.SessionID = "another-session"
				require.Error(t, validateAdoptSourceTranscript(state, third))
				state.SessionID = id
				state.AgentHome = t.TempDir()
				require.Error(t, validateAdoptSourceTranscript(state, third))
				state.AgentHome = home
				require.NoDirExists(t, filepath.Join(os.Getenv("ENTIRE_CONFIG_DIR"), "adopted-transcripts"))
			})
		}
	}
}
