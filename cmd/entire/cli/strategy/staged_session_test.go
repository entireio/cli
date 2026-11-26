package strategy

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/entiredir"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/stretchr/testify/require"
)

func TestCondenseSessionByID_PreservesNewerStagedCopy(t *testing.T) {
	// Serial: fixture changes cwd and swaps the existing telemetry seam.
	for _, scenario := range []string{"pending step", "transcript before step", "identical transcript", "prompt only", "resumed turn"} {
		t.Run(scenario, func(t *testing.T) {
			const sessionID = "cleanup-next-turn"
			repo, state := setupCondensableSessionWithTranscript(t, sessionID)
			defer repo.Close()
			dir, err := paths.WorktreeRoot(t.Context())
			require.NoError(t, err)
			testutil.WriteFile(t, dir, ".entire/settings.json", `{"enabled":true,"telemetry":true}`)
			metadata := paths.SessionMetadataDirFromSessionID(sessionID)
			// A real skill invocation drives the existing post-save emitter seam.
			const transcript = "{\"type\":\"assistant\",\"uuid\":\"a1\",\"message\":{\"content\":[{\"type\":\"tool_use\",\"id\":\"toolu_cleanup\",\"name\":\"Skill\",\"input\":{\"skill\":\"entire:search\"}}]}}\n"
			testutil.WriteFile(t, dir, filepath.Join(metadata, paths.TranscriptFileName), transcript)
			testutil.WriteFile(t, dir, filepath.Join(metadata, paths.PromptFileName), "old prompt")
			state.AgentType = agent.AgentTypeClaudeCode
			require.NoError(t, SaveSessionState(t.Context(), state))
			wantTranscript, wantPrompt := transcript, "old prompt"
			interleaved := false
			old := emitSkillTelemetry
			emitSkillTelemetry = func(_ []agent.SkillEvent, _ bool, _ string) {
				if scenario == "resumed turn" || scenario == "pending step" {
					require.NoError(t, MutateSessionState(t.Context(), sessionID, func(s *SessionState) error {
						if scenario == "resumed turn" {
							s.TurnID = "next-turn"
							s.Phase = session.PhaseActive
						} else {
							s.StepCount = 1
							s.FilesTouched = []string{"next-turn.go"}
						}
						return nil
					}))
				}
				if scenario != "resumed turn" {
					require.NoError(t, WithSessionMetadata(t.Context(), sessionID, func(root *os.Root) error {
						name, content := paths.TranscriptFileName, transcript
						if scenario == "prompt only" {
							name, content = paths.PromptFileName, "next prompt"
							wantPrompt = content
						} else if scenario != "identical transcript" {
							content = "next turn transcript not yet checkpointed\n"
							wantTranscript = content
						}
						return entiredir.WriteFile(root, entiredir.MustName(metadata)+"/"+name, []byte(content), 0o600)
					}))
				}
				interleaved = true
			}
			t.Cleanup(func() { emitSkillTelemetry = old })
			require.NoError(t, (&ManualCommitStrategy{}).CondenseSessionByID(t.Context(), sessionID))
			require.True(t, interleaved, "fixture must enter the post-save/pre-cleanup window")
			for name, want := range map[string]string{paths.TranscriptFileName: wantTranscript, paths.PromptFileName: wantPrompt} {
				got, err := os.ReadFile(filepath.Join(dir, metadata, name))
				require.NoError(t, err, "a newer publication/turn must keep the staging buffer")
				require.Equal(t, want, string(got))
			}
			current, err := LoadSessionState(t.Context(), sessionID)
			require.NoError(t, err)
			require.False(t, current.LastCheckpointID.IsEmpty(), "the older checkpoint remains recorded")
			if scenario == "pending step" {
				require.Contains(t, current.FilesTouched, "next-turn.go")
			}
		})
	}
}

func TestCondenseSessionByID_ReleaseRequiresOuterSave(t *testing.T) {
	// Serial: condensation resolves from cwd. Nested condensation must not
	// release files before the outer mutation decides whether to save.
	for _, outcome := range []string{"success", "skip", "error", "save failure"} {
		t.Run(outcome, func(t *testing.T) {
			const sessionID = "nested-condense"
			repo, _ := setupCondensableSessionWithTranscript(t, sessionID)
			defer repo.Close()
			dir, err := paths.WorktreeRoot(t.Context())
			require.NoError(t, err)
			staged := filepath.Join(dir, paths.SessionMetadataDirFromSessionID(sessionID), paths.TranscriptFileName)
			err = MutateSessionState(t.Context(), sessionID, func(_ *SessionState) error {
				require.NoError(t, (&ManualCommitStrategy{}).CondenseSessionByID(t.Context(), sessionID))
				require.FileExists(t, staged, "the nested mutation has not saved yet")
				switch outcome {
				case "skip":
					return ErrMutationSkip
				case "error":
					return errors.New("outer mutation failed")
				case "save failure":
					stateDir, err := getSessionStateDir(t.Context())
					require.NoError(t, err)
					statePath := filepath.Join(stateDir, sessionID+".json")
					require.NoError(t, os.Rename(statePath, statePath+".backup"))
					require.NoError(t, os.Mkdir(statePath, 0o700))
				}
				return nil
			})
			if outcome == "error" || outcome == "save failure" {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			if outcome == "success" {
				require.NoFileExists(t, staged)
			} else {
				require.FileExists(t, staged, "failed/skipped saves must retain the recovery copy")
			}
		})
	}
}

func TestWithSessionMetadata_SerializesPublication(t *testing.T) {
	dir := setupGitRepo(t)
	t.Chdir(dir)
	const sessionID = "staged-writer"
	require.NoError(t, SaveSessionState(t.Context(), &SessionState{SessionID: sessionID, Phase: session.PhaseIdle, StartedAt: time.Now()}))
	started, published := make(chan struct{}), make(chan error, 1)
	require.NoError(t, MutateSessionState(t.Context(), sessionID, func(_ *SessionState) error {
		go func() {
			close(started)
			published <- WithSessionMetadata(t.Context(), sessionID, func(root *os.Root) error {
				return entiredir.WriteFile(root, entiredir.MustName(paths.SessionMetadataDirFromSessionID(sessionID))+"/"+paths.TranscriptFileName, []byte("new transcript"), 0o600)
			})
		}()
		<-started
		select {
		case err := <-published:
			t.Fatalf("publication ran while condensation held the gate: %v", err)
		case <-time.After(30 * time.Millisecond):
		}
		return nil
	}))
	select {
	case err := <-published:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("publication did not resume after the gate was released")
	}
}

// Serial: uses the existing CWD-based fixture. A publication that predates
// the snapshot must still survive until its own SaveStep finishes.
func TestCondenseSessionByID_InFlightPublication(t *testing.T) {
	const sid = "inflight-stop"
	repo, _ := setupCondensableSessionWithTranscript(t, sid)
	defer repo.Close()
	dir, err := paths.WorktreeRoot(t.Context())
	require.NoError(t, err)
	metadata := paths.SessionMetadataDirFromSessionID(sid)
	release, err := AcquireSessionMetadataPublication(t.Context(), sid)
	require.NoError(t, err)
	released := false
	defer func() {
		if !released {
			release()
		}
	}()
	require.NoError(t, WithSessionMetadata(t.Context(), sid, func(root *os.Root) error {
		return entiredir.WriteFile(root, entiredir.MustName(metadata)+"/"+paths.TranscriptFileName, []byte("{\"type\":\"human\",\"message\":{\"content\":\"next turn\"}}\n"), 0o600)
	}))
	s := &ManualCommitStrategy{}
	require.NoError(t, s.CondenseSessionByID(t.Context(), sid))
	testutil.WriteFile(t, dir, "next.go", "package next")
	require.NoError(t, s.SaveStep(t.Context(), StepContext{SessionID: sid, NewFiles: []string{"next.go"}, MetadataDir: metadata, AuthorName: "Test", AuthorEmail: "test@example.com", AgentType: agent.AgentTypeClaudeCode}))
	require.FileExists(t, filepath.Join(dir, metadata, paths.TranscriptFileName))
	release()
	released = true
	// A released lease does not pin old files indefinitely.
	releaseStoredCopyForTest(t.Context(), sid)
	require.NoFileExists(t, filepath.Join(dir, metadata, paths.TranscriptFileName))
}
