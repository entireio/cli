package cli

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	cpkg "github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/entireio/cli/cmd/entire/cli/trailers"
)

// These scenarios change CWD/environment through setupAttachTestRepo.
func TestAttach_MissingReceiptCheckpoint(t *testing.T) {
	for _, backend := range []string{"git-branch", "git-refs"} {
		t.Run(backend, func(t *testing.T) {
			for _, linked := range []bool{false, true} {
				name := "pending"
				if linked {
					name = "linked"
				}
				t.Run(name, func(t *testing.T) {
					setupAttachTestRepo(t)
					t.Setenv("ENTIRE_CHECKPOINTS_PRIMARY", backend)
					const sid = "missing-receipt"
					setupClaudeTranscript(t, sid, attachFirstTurn)
					var out bytes.Buffer
					if err := runAttach(t.Context(), &out, &out, sid, agent.AgentNameClaudeCode, attachOptions{Force: linked}); err != nil {
						t.Fatal(err)
					}
					before, _, _ := readAttachSnapshot(t, sid)
					for _, ref := range checkpointStorageRefs(t.Context(), before.LastCheckpointID) {
						if !strings.HasPrefix(ref, "refs/") {
							ref = "refs/heads/" + ref
						}
						testutil.RunGit(t, mustGetwd(t), "update-ref", "-d", ref)
					}
					setupClaudeTranscript(t, sid, attachFirstTurn+attachNextTurn)
					err := runAttach(t.Context(), &out, &out, sid, agent.AgentNameClaudeCode, attachOptions{Force: true})
					if linked {
						if err == nil {
							t.Fatal("must refuse to replace a checkpoint referenced by HEAD")
						}
						return
					}
					if err != nil {
						t.Fatalf("retry should recover a missing unlinked checkpoint: %v", err)
					}
					after, _, content := readAttachSnapshot(t, sid)
					if after.LastCheckpointID == before.LastCheckpointID {
						t.Error("reused missing checkpoint ID")
					}
					if content.Metadata.GetTranscriptStart() != 0 || content.Metadata.TokenUsage == nil || content.Metadata.TokenUsage.InputTokens != 30 {
						t.Errorf("recovery must capture full transcript and tokens: %+v", content.Metadata)
					}
					linkedIDs := trailers.ParseAllCheckpoints(testutil.RunGit(t, mustGetwd(t), "log", "-1", "--format=%B"))
					if len(linkedIDs) != 1 || linkedIDs[0] != after.LastCheckpointID {
						t.Errorf("HEAD checkpoints=%v, want %s", linkedIDs, after.LastCheckpointID)
					}
				})
			}
		})
	}
}

func TestAttach_HookTrackedSessionPreservesCheckpointWindow(t *testing.T) {
	for _, marker := range []string{"commit hash", "transcript offset"} {
		t.Run(marker, func(t *testing.T) {
			setupAttachTestRepo(t)
			const sid = "hook-tracked-attach"
			setupClaudeTranscript(t, sid, attachFirstTurn)
			var out bytes.Buffer
			opts := attachOptions{Force: true}
			if err := runAttach(t.Context(), &out, &out, sid, agent.AgentNameClaudeCode, opts); err != nil {
				t.Fatal(err)
			}
			state, _, _ := readAttachSnapshot(t, sid)
			if marker == "commit hash" {
				state.LastCheckpointCommitHash = testutil.RunGit(t, mustGetwd(t), "rev-parse", "HEAD")
			} else {
				state.CheckpointTranscriptStart = 2
			}
			state.CheckpointTokenUsage = &agent.TokenUsage{InputTokens: 20}
			states, err := session.NewStateStore(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if err := states.Save(t.Context(), state); err != nil {
				t.Fatal(err)
			}
			// Membership is checked first: already attached sessions stay idempotent.
			if err := runAttach(t.Context(), &out, &out, sid, agent.AgentNameClaudeCode, opts); err != nil {
				t.Fatal(err)
			}
			testutil.WriteFile(t, mustGetwd(t), "next.txt", "next")
			testutil.GitAdd(t, mustGetwd(t), "next.txt")
			testutil.GitCommit(t, mustGetwd(t), "next commit")
			setupClaudeTranscript(t, sid, attachFirstTurn+attachNextTurn)
			err = runAttach(t.Context(), &out, &out, sid, agent.AgentNameClaudeCode, opts)
			if err == nil || !strings.Contains(err.Error(), "hooks") {
				t.Errorf("want hook ownership error, got %v", err)
			}
			after, err := states.Load(t.Context(), sid)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(state, after) {
				t.Error("attach changed hook-owned checkpoint state")
			}
			if ids := trailers.ParseAllCheckpoints(testutil.RunGit(t, mustGetwd(t), "log", "-1", "--format=%B")); len(ids) != 0 {
				t.Errorf("refused attach amended HEAD: %v", ids)
			}
		})
	}
}

type attachReadFailureStore struct {
	cpkg.PersistentStore

	stage   string
	failure error
}

func (s attachReadFailureStore) Read(ctx context.Context, cpID id.CheckpointID) (*cpkg.CheckpointSummary, error) {
	if s.stage == "summary" {
		return nil, s.failure
	}
	return s.PersistentStore.Read(ctx, cpID)
}
func (s attachReadFailureStore) ReadSessionMetadata(ctx context.Context, cpID id.CheckpointID, index int) (*cpkg.Metadata, error) {
	if s.stage == "metadata" {
		return nil, s.failure
	}
	return s.PersistentStore.ReadSessionMetadata(ctx, cpID, index)
}
func (s attachReadFailureStore) ReadSessionContent(ctx context.Context, cpID id.CheckpointID, index int) (*cpkg.SessionContent, error) {
	if s.stage == "transcript" {
		return nil, s.failure
	}
	return s.PersistentStore.ReadSessionContent(ctx, cpID, index)
}

func TestAttach_PreviousSnapshotReadFailure(t *testing.T) {
	setupAttachTestRepo(t)
	const sid = "unreadable-previous"
	setupClaudeTranscript(t, sid, attachFirstTurn)
	var out bytes.Buffer
	if err := runAttach(t.Context(), &out, &out, sid, agent.AgentNameClaudeCode, attachOptions{Force: true}); err != nil {
		t.Fatal(err)
	}
	state, store, _ := readAttachSnapshot(t, sid)
	for _, stage := range []string{"summary", "metadata", "transcript"} {
		t.Run(stage, func(t *testing.T) {
			for _, failure := range []error{errors.New("checkpoint object unavailable"), context.Canceled, context.DeadlineExceeded} {
				failing := attachReadFailureStore{PersistentStore: store, stage: stage, failure: failure}
				start, err := attachTranscriptStart(t.Context(), failing, state, agent.AgentTypeClaudeCode, []byte(attachFirstTurn+attachNextTurn))
				if errors.Is(failure, context.Canceled) || errors.Is(failure, context.DeadlineExceeded) {
					if !errors.Is(err, failure) {
						t.Errorf("must preserve cancellation: %v", err)
					}
				} else if err != nil || start != 0 {
					t.Errorf("unavailable prefix must capture full transcript: start=%d err=%v", start, err)
				}
			}
		})
	}
}
