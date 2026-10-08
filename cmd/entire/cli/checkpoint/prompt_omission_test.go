package checkpoint

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/redact"
)

const promptOmissionSecret = "do-not-sync-this-prompt"

func promptBearingWriteOptions(checkpointID id.CheckpointID) WriteOptions {
	return WriteOptions{
		CheckpointID:     checkpointID,
		SessionID:        "session-omit",
		Strategy:         "manual-commit",
		Transcript:       redact.AlreadyRedacted([]byte(`{"type":"user","message":"` + promptOmissionSecret + `"}` + "\n")),
		TranscriptPath:   "/tmp/does-not-matter.jsonl",
		Assets:           []TranscriptAsset{{}},
		Prompts:          []string{promptOmissionSecret},
		FilesTouched:     []string{"main.go"},
		Summary:          &Summary{Intent: promptOmissionSecret},
		ReviewPrompt:     promptOmissionSecret,
		InvestigateTopic: promptOmissionSecret,
		Attribution:      &Attribution{AgentLines: 7},
		Tasks: []TaskPayload{{
			ToolUseID:       "toolu_1",
			AgentID:         "agent-1",
			TaskDescription: promptOmissionSecret,
			Transcript:      redact.AlreadyRedacted([]byte(promptOmissionSecret + "\n")),
			Files:           []string{"sub.go"},
			StartedAt:       time.Unix(1, 0).UTC(),
		}},
		SkillEvents: []types.SkillEvent{{
			EventType:        types.SkillEventTypePromptInvocation,
			Skill:            types.SkillEventSkill{Name: "deploy"},
			Native:           map[string]string{"command": "/skill:deploy " + promptOmissionSecret},
			TranscriptAnchor: &types.SkillEventTranscriptAnchor{Unit: "line", Start: 1, End: 2},
			Collapse:         types.SkillEventCollapse{Label: "/skill:deploy " + promptOmissionSecret},
		}},
		AuthorName:  "Test",
		AuthorEmail: "test@test.com",
	}
}

func TestOmitPromptContent_SessionStripsPromptBearingFields(t *testing.T) {
	t.Parallel()
	opts := promptBearingWriteOptions(id.MustCheckpointID("a1b2c3d4e5f6"))

	for _, req := range []WriteRequest{Session(opts), ReservedSession(opts)} {
		got, ok := omitPromptContent(req)
		require.True(t, ok)
		var out WriteOptions
		switch r := got.(type) {
		case Session:
			out = WriteOptions(r)
		case ReservedSession:
			out = WriteOptions(r)
		default:
			t.Fatalf("unexpected request type %T", got)
		}

		assert.Zero(t, out.Transcript.Len())
		assert.Empty(t, out.TranscriptPath, "TranscriptPath would let the store re-read the transcript from disk")
		assert.Empty(t, out.Assets)
		assert.Empty(t, out.Prompts)
		assert.Nil(t, out.Summary)
		assert.Empty(t, out.ReviewPrompt)
		assert.Empty(t, out.InvestigateTopic)
		require.Len(t, out.Tasks, 1)
		assert.Zero(t, out.Tasks[0].Transcript.Len())
		assert.Empty(t, out.Tasks[0].TaskDescription)
		assert.Equal(t, TaskTranscriptReasonOmittedBySettings, out.Tasks[0].TranscriptUnavailableReason)
		assert.Equal(t, []string{"sub.go"}, out.Tasks[0].Files)
		require.Len(t, out.SkillEvents, 1)
		assert.Nil(t, out.SkillEvents[0].Native)
		assert.Nil(t, out.SkillEvents[0].TranscriptAnchor)
		assert.Equal(t, "deploy", out.SkillEvents[0].Collapse.Label)

		// Non-prompt metadata survives.
		assert.Equal(t, []string{"main.go"}, out.FilesTouched)
		assert.Equal(t, 7, out.Attribution.AgentLines)
	}

	// The caller's request is not mutated (Tasks/SkillEvents are copied).
	assert.Equal(t, promptOmissionSecret, opts.Tasks[0].TaskDescription)
	assert.NotNil(t, opts.SkillEvents[0].Native)
}

func TestOmitPromptContent_BackfillsAndOtherRequests(t *testing.T) {
	t.Parallel()
	cpID := id.MustCheckpointID("a1b2c3d4e5f6")

	got, ok := omitPromptContent(SessionTranscript{
		CheckpointID:     cpID,
		Transcript:       redact.AlreadyRedacted([]byte(promptOmissionSecret)),
		Assets:           []TranscriptAsset{{}},
		Prompts:          []string{promptOmissionSecret},
		PrecomputedBlobs: &PrecomputedTranscriptBlobs{},
	})
	require.True(t, ok)
	st, isTranscript := got.(SessionTranscript)
	require.True(t, isTranscript)
	assert.Zero(t, st.Transcript.Len())
	assert.Empty(t, st.Assets)
	assert.Empty(t, st.Prompts)
	assert.Nil(t, st.PrecomputedBlobs)

	_, ok = omitPromptContent(SessionSummary{CheckpointID: cpID, Summary: &Summary{Intent: promptOmissionSecret}})
	assert.False(t, ok, "summary backfill carries only prompt-derived content and must be dropped")

	attr := CheckpointAttribution{CheckpointID: cpID, Attribution: &Attribution{AgentLines: 1}}
	got, ok = omitPromptContent(attr)
	require.True(t, ok)
	assert.Equal(t, attr, got)
}

func TestWithPromptOmission_PassThroughWhenSyncEnabled(t *testing.T) {
	t.Parallel()
	_, repo, _ := newTestRepo(t)
	store := NewGitStore(repo, DefaultV1Refs())
	assert.Same(t, PersistentStore(store), withPromptOmission(store, false))

	wrapped := withPromptOmission(store, true)
	_, isAuthor := wrapped.(AuthorReader)
	assert.True(t, isAuthor, "omission wrapper must preserve AuthorReader")
}

// Not parallel: uses t.Chdir so settings resolve to the test repo.
func TestOpen_SyncPromptsFalseOmitsPromptContent(t *testing.T) {
	for _, backend := range []string{BackendTypeGitBranch, BackendTypeGitRefs} {
		t.Run(backend, func(t *testing.T) {
			dir, repo, _ := newTestRepo(t)
			t.Chdir(dir)
			writeRawSettings(t, dir, `{"enabled": true, "strategy_options": {"sync_prompts": false}, "checkpoints": {"primary": {"type": "`+backend+`"}}}`)

			stores, err := Open(context.Background(), repo, OpenOptions{})
			require.NoError(t, err)
			_, isAuthor := stores.Persistent.(AuthorReader)
			assert.True(t, isAuthor)

			ctx := context.Background()
			cpID := id.MustCheckpointID("b1b2c3d4e5f6")
			if backend == BackendTypeGitRefs {
				cpID = id.MustCheckpointID("01ARZ3NDEKTSV4RRFFQ69G5FAV")
			}
			opts := promptBearingWriteOptions(cpID)
			opts.Assets = nil // an empty asset placeholder is not a valid stored asset
			require.NoError(t, stores.Persistent.Write(ctx, Session(opts)))
			require.NoError(t, stores.Persistent.Write(ctx, SessionTranscript{
				CheckpointID: cpID,
				SessionID:    opts.SessionID,
				Transcript:   redact.AlreadyRedacted([]byte(promptOmissionSecret + "\n")),
				Prompts:      []string{promptOmissionSecret},
			}))
			require.NoError(t, stores.Persistent.Write(ctx, SessionSummary{
				CheckpointID: cpID,
				Summary:      &Summary{Intent: promptOmissionSecret},
			}))

			meta, err := stores.Persistent.ReadSessionMetadata(ctx, cpID, 0)
			require.NoError(t, err)
			assert.Equal(t, []string{"main.go"}, meta.FilesTouched)
			assert.Nil(t, meta.Summary)
			assert.Empty(t, meta.ReviewPrompt)
			assert.Empty(t, meta.InvestigateTopic)

			prompts, err := stores.Persistent.ReadSessionPrompts(ctx, cpID, 0)
			require.NoError(t, err)
			assert.Empty(t, prompts)

			summary, err := stores.Persistent.Read(ctx, cpID)
			require.NoError(t, err)
			require.Len(t, summary.Sessions, 1)
			assert.Empty(t, summary.Sessions[0].Transcript)
			assert.Empty(t, summary.Sessions[0].Prompt)
			assert.Empty(t, summary.Sessions[0].CompactTranscript)
		})
	}
}

// Not parallel: uses t.Chdir so settings resolve to the test repo.
func TestOpen_SyncPromptsDefaultKeepsPromptContent(t *testing.T) {
	dir, repo, _ := newTestRepo(t)
	t.Chdir(dir)
	writeRawSettings(t, dir, `{"enabled": true}`)

	stores, err := Open(context.Background(), repo, OpenOptions{})
	require.NoError(t, err)

	ctx := context.Background()
	cpID := id.MustCheckpointID("c1b2c3d4e5f6")
	opts := promptBearingWriteOptions(cpID)
	opts.Assets = nil
	opts.Tasks = nil
	require.NoError(t, stores.Persistent.Write(ctx, Session(opts)))

	prompts, err := stores.Persistent.ReadSessionPrompts(ctx, cpID, 0)
	require.NoError(t, err)
	assert.Contains(t, prompts, promptOmissionSecret)
}

// Not parallel: uses t.Chdir so settings resolve to the test repo.
func TestOpen_SyncPromptsUninterpretableFailsClosed(t *testing.T) {
	for name, body := range map[string]string{
		"non-boolean":  `{"enabled": true, "strategy_options": {"sync_prompts": "false"}}`,
		"syntax error": `{"enabled": true, "strategy_options": {`,
	} {
		t.Run(name, func(t *testing.T) {
			dir, repo, _ := newTestRepo(t)
			t.Chdir(dir)
			writeRawSettings(t, dir, body)

			stores, err := Open(context.Background(), repo, OpenOptions{})
			require.NoError(t, err, "Open stays fail-soft about settings content")
			_, omitting := stores.Persistent.(*promptOmittingStoreWithAuthor)
			assert.True(t, omitting, "settings that cannot be interpreted must withhold prompts")
		})
	}
}
