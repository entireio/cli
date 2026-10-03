package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	cpkg "github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/entireio/cli/cmd/entire/cli/trailers"
)

const attachFirstTurn = `{"type":"user","message":{"role":"user","content":"first"},"uuid":"u1"}
{"type":"assistant","message":{"id":"msg_1","role":"assistant","content":[{"type":"text","text":"done"}],"usage":{"input_tokens":10,"output_tokens":5}},"uuid":"a1"}
`
const attachNextTurn = `{"type":"user","message":{"role":"user","content":"next"},"uuid":"u2"}
{"type":"assistant","message":{"id":"msg_2","role":"assistant","content":[{"type":"text","text":"done"}],"usage":{"input_tokens":20,"output_tokens":7}},"uuid":"a2"}
`

func readAttachSnapshot(t *testing.T, sid string) (*session.State, cpkg.PersistentStore, *cpkg.SessionContent) {
	t.Helper()
	ctx := t.Context()
	states, err := session.NewStateStore(ctx)
	if err != nil {
		t.Fatal(err)
	}
	state, err := states.Load(ctx, sid)
	if err != nil || state == nil {
		t.Fatalf("state=%v err=%v", state, err)
	}
	repo, err := openRepository(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := repo.Close(); err != nil {
			t.Error(err)
		}
	})
	store, err := openAttachStore(ctx, repo, (attachOptions{}).committedRefs(ctx))
	if err != nil {
		t.Fatal(err)
	}
	content, err := store.ReadSessionContent(ctx, state.LastCheckpointID, 0)
	if err != nil {
		t.Fatal(err)
	}
	return state, store, content
}

func TestAttach_RetryAfterPrintedTrailer(t *testing.T) {
	for _, backend := range []string{"git-branch", "git-refs"} {
		t.Run(backend, func(t *testing.T) {
			setupAttachTestRepo(t)
			t.Setenv("ENTIRE_CHECKPOINTS_PRIMARY", backend)
			const sid = "adversarial-attach-retry"
			setupClaudeTranscript(t, sid, attachFirstTurn)
			var out bytes.Buffer
			if err := runAttach(t.Context(), &out, &out, sid, agent.AgentNameClaudeCode, attachOptions{}); err != nil {
				t.Fatal(err)
			}
			before, _, first := readAttachSnapshot(t, sid)
			repo, err := openRepository(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			head, err := getHeadCommit(repo)
			if err != nil {
				t.Fatal(err)
			}
			if err := repo.Close(); err != nil {
				t.Fatal(err)
			}
			if len(trailers.ParseAllCheckpoints(head.Message)) != 0 {
				t.Fatal("fixture unexpectedly linked first checkpoint")
			}
			// Reprinting must not manufacture another snapshot either.
			if err := runAttach(t.Context(), &out, &out, sid, agent.AgentNameClaudeCode, attachOptions{}); err != nil {
				t.Fatal(err)
			}
			retry, _, repeated := readAttachSnapshot(t, sid)
			if retry.LastCheckpointID != before.LastCheckpointID || !reflect.DeepEqual(first, repeated) {
				t.Error("non-force retry changed snapshot")
			}
			if err := runAttach(t.Context(), &out, &out, sid, agent.AgentNameClaudeCode, attachOptions{Force: true}); err != nil {
				t.Fatal(err)
			}
			after, _, content := readAttachSnapshot(t, sid)
			linked := trailers.ParseAllCheckpoints(testutil.RunGit(t, mustGetwd(t), "log", "-1", "--format=%B"))
			if len(linked) != 1 || linked[0] != before.LastCheckpointID {
				t.Errorf("HEAD checkpoints=%v, want %s", linked, before.LastCheckpointID)
			}
			scoped := scopeTranscriptForCheckpoint(content.Transcript, content.Metadata.GetTranscriptStart(), content.Metadata.Agent)
			t.Logf("unlinked=%s linked=%s start=%d fullBytes=%d scopedBytes=%d usage=%+v", before.LastCheckpointID, after.LastCheckpointID, content.Metadata.GetTranscriptStart(), len(content.Transcript), len(scoped), content.Metadata.TokenUsage)
			if len(bytes.TrimSpace(scoped)) == 0 {
				t.Error("retry attached an empty scoped conversation")
			}
			if content.Metadata.TokenUsage == nil || content.Metadata.TokenUsage.InputTokens != 10 {
				t.Error("retry lost the initial input tokens")
			}
			if before.LastCheckpointID != after.LastCheckpointID {
				t.Error("retry created another checkpoint instead of linking the existing one")
			}
		})
	}
}

// These tests change CWD and agent environment via setupAttachTestRepo.
func TestAttach_ReviewSnapshots(t *testing.T) {
	for _, tc := range []struct {
		name                                   string
		newCommit, removeState, explicitReview bool
	}{
		{name: "same HEAD", explicitReview: true},
		{name: "same HEAD without state", removeState: true, explicitReview: true},
		{name: "later commit explicit review", newCommit: true, explicitReview: true},
		{name: "later commit inherited review", newCommit: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setupAttachTestRepo(t)
			const sid = "repeat-review"
			setupClaudeTranscript(t, sid, attachFirstTurn)
			opts := attachOptions{Force: true, Review: true, ReviewSkillsOverride: []string{"/review"}, ReviewPromptOverride: "Check correctness"}
			var out bytes.Buffer
			if err := runAttach(t.Context(), &out, &out, sid, agent.AgentNameClaudeCode, opts); err != nil {
				t.Fatal(err)
			}
			before, store, first := readAttachSnapshot(t, sid)
			if tc.removeState {
				if err := os.Remove(filepath.Join(mustGetwd(t), ".git", "entire-sessions", sid+".json")); err != nil {
					t.Fatal(err)
				}
			}
			if tc.newCommit {
				testutil.WriteFile(t, mustGetwd(t), "next.txt", "next change")
				testutil.GitAdd(t, mustGetwd(t), "next.txt")
				testutil.GitCommit(t, mustGetwd(t), "next commit")
			}
			setupClaudeTranscript(t, sid, attachFirstTurn+attachNextTurn)
			if !tc.explicitReview {
				opts = attachOptions{Force: true}
			}
			if err := runAttach(t.Context(), &out, &out, sid, agent.AgentNameClaudeCode, opts); err != nil {
				t.Fatal(err)
			}
			unchanged, err := store.ReadSessionContent(t.Context(), before.LastCheckpointID, 0)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(first, unchanged) {
				t.Error("retry changed the earlier snapshot")
			}
			if !tc.newCommit {
				return
			}
			after, nextStore, next := readAttachSnapshot(t, sid)
			if before.LastCheckpointID == after.LastCheckpointID {
				t.Error("later commit reused checkpoint")
			}
			if next.Metadata.Kind != first.Metadata.Kind || !reflect.DeepEqual(next.Metadata.ReviewSkills, first.Metadata.ReviewSkills) || next.Metadata.ReviewPrompt != first.Metadata.ReviewPrompt {
				t.Errorf("review metadata lost: %+v", next.Metadata)
			}
			summary, err := nextStore.Read(t.Context(), after.LastCheckpointID)
			if err != nil {
				t.Fatal(err)
			}
			if !summary.HasReview {
				t.Error("checkpoint lost HasReview")
			}
			if next.Metadata.GetTranscriptStart() != 2 {
				t.Errorf("start=%d, want 2", next.Metadata.GetTranscriptStart())
			}
		})
	}
}
