package checkpoint

import (
	"context"
	"encoding/json"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/testutil/gitenv"
	"github.com/entireio/cli/redact"

	"github.com/go-git/go-git/v6"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Line attribution older CLIs wrote into checkpoint metadata. The CLI no
// longer reads or writes it, but a rewrite of an old checkpoint must not
// strip it: entire.io still derives the experts counts from it.
const (
	legacyInitialAttribution  = `{"calculated_at":"2026-01-02T03:04:05Z","agent_lines":12,"human_added":3,"total_committed":15,"agent_percentage":80,"metric_version":2}`
	legacyPromptAttributions  = `[{"checkpoint_number":1,"user_lines_added":3,"agent_lines_added":12}]`
	legacyCombinedAttribution = `{"calculated_at":"2026-01-02T03:04:06Z","agent_lines":20,"human_added":5,"total_committed":25,"agent_percentage":80}`
)

// runGitIn runs git in dir with the test-isolated config and extra env.
func runGitIn(t *testing.T, dir string, env []string, stdin string, args ...string) string {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", args...)
	cmd.Dir = dir
	cmd.Env = append(gitenv.Isolated(), env...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	out, err := cmd.Output()
	require.NoError(t, err, "git %v", args)
	return strings.TrimSpace(string(out))
}

// injectLegacyAttribution rewrites the checkpoint on the metadata branch as an
// older CLI left it: the session's metadata.json gains initial_attribution and
// prompt_attributions, the root summary gains combined_attribution.
func injectLegacyAttribution(t *testing.T, repo *git.Repository, cpID id.CheckpointID, sessionIndex string) {
	t.Helper()
	wt, err := repo.Worktree()
	require.NoError(t, err)
	dir := wt.Filesystem().Root()
	branch := "refs/heads/" + paths.MetadataBranchName
	index := filepath.Join(t.TempDir(), "index")
	env := []string{"GIT_INDEX_FILE=" + index}

	addKeys := func(path string, keys map[string]string) {
		raw := runGitIn(t, dir, nil, "", "cat-file", "-p", branch+":"+path)
		var doc map[string]json.RawMessage
		require.NoError(t, json.Unmarshal([]byte(raw), &doc))
		for k, v := range keys {
			doc[k] = json.RawMessage(v)
		}
		data, err := json.MarshalIndent(doc, "", "  ")
		require.NoError(t, err)
		blob := runGitIn(t, dir, nil, string(data)+"\n", "hash-object", "-w", "--stdin")
		runGitIn(t, dir, env, "", "update-index", "--cacheinfo", "100644,"+blob+","+path)
	}
	runGitIn(t, dir, env, "", "read-tree", branch)
	addKeys(cpID.Path()+"/"+sessionIndex+"/"+paths.MetadataFileName, map[string]string{
		"initial_attribution": legacyInitialAttribution,
		"prompt_attributions": legacyPromptAttributions,
	})
	addKeys(cpID.Path()+"/"+paths.MetadataFileName, map[string]string{
		"combined_attribution": legacyCombinedAttribution,
	})
	tree := runGitIn(t, dir, env, "", "write-tree")
	parent := runGitIn(t, dir, nil, "", "rev-parse", branch)
	commit := runGitIn(t, dir, nil, "", "commit-tree", tree, "-p", parent, "-m", "older CLI checkpoint")
	runGitIn(t, dir, nil, "", "update-ref", branch, commit)
}

// readRawKeys returns the named keys of a JSON file on the metadata branch.
func readRawKeys(t *testing.T, store *GitStore, path string) map[string]json.RawMessage {
	t.Helper()
	content, ok := readBranchFile(t, store, path)
	require.True(t, ok, "missing %s", path)
	var doc map[string]json.RawMessage
	require.NoError(t, json.Unmarshal([]byte(content), &doc))
	return doc
}

func assertLegacyAttributionKept(t *testing.T, store *GitStore, cpID id.CheckpointID, sessionIndex string) {
	t.Helper()
	session := readRawKeys(t, store, cpID.Path()+"/"+sessionIndex+"/"+paths.MetadataFileName)
	assert.JSONEq(t, legacyInitialAttribution, string(session["initial_attribution"]))
	assert.JSONEq(t, legacyPromptAttributions, string(session["prompt_attributions"]))
	root := readRawKeys(t, store, cpID.Path()+"/"+paths.MetadataFileName)
	assert.JSONEq(t, legacyCombinedAttribution, string(root["combined_attribution"]))
}

func writeLegacyTestSession(t *testing.T, store *GitStore, cpID id.CheckpointID, sessionID string) {
	t.Helper()
	require.NoError(t, store.Write(context.Background(), Session{
		CheckpointID: cpID,
		SessionID:    sessionID,
		Strategy:     "manual-commit",
		Transcript:   redact.AlreadyRedacted([]byte(`{"type":"user","message":"hi"}` + "\n")),
		FilesTouched: []string{"a.go"},
		Agent:        agent.AgentTypeClaudeCode,
		AuthorName:   "Test",
		AuthorEmail:  "test@example.com",
	}))
}

// Every rewrite of an existing checkpoint keeps the legacy attribution keys:
// a summary backfill (`explain --generate`), a transcript backfill (finalize),
// attaching another session, and re-writing the same session slot.
func TestRewritesKeepLegacyAttribution(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		rewrite func(t *testing.T, store *GitStore, cpID id.CheckpointID)
	}{
		{"summary backfill", func(t *testing.T, store *GitStore, cpID id.CheckpointID) {
			t.Helper()
			require.NoError(t, store.Write(context.Background(), SessionSummary{CheckpointID: cpID, Summary: &Summary{Intent: "intent"}}))
		}},
		{"transcript backfill", func(t *testing.T, store *GitStore, cpID id.CheckpointID) {
			t.Helper()
			require.NoError(t, store.Write(context.Background(), SessionTranscript{
				CheckpointID: cpID, SessionID: "legacy-session",
				Transcript: redact.AlreadyRedacted([]byte(`{"type":"user","message":"hi again"}` + "\n")),
				Agent:      agent.AgentTypeClaudeCode,
			}))
		}},
		{"attach another session", func(t *testing.T, store *GitStore, cpID id.CheckpointID) {
			t.Helper()
			writeLegacyTestSession(t, store, cpID, "attached-session")
		}},
		{"rewrite the same session", func(t *testing.T, store *GitStore, cpID id.CheckpointID) {
			t.Helper()
			writeLegacyTestSession(t, store, cpID, "legacy-session")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			repo := setupTestRepo(t)
			store := NewGitStore(repo, DefaultV1Refs())
			cpID := id.MustCheckpointID("a1b2c3d4e5f6")
			writeLegacyTestSession(t, store, cpID, "legacy-session")
			injectLegacyAttribution(t, repo, cpID, "0")
			assertLegacyAttributionKept(t, store, cpID, "0")

			tc.rewrite(t, store, cpID)

			assertLegacyAttributionKept(t, store, cpID, "0")
		})
	}
}

// A checkpoint this version writes carries none of the legacy keys.
func TestNewCheckpointHasNoLegacyAttribution(t *testing.T) {
	t.Parallel()
	repo := setupTestRepo(t)
	store := NewGitStore(repo, DefaultV1Refs())
	cpID := id.MustCheckpointID("b1b2c3d4e5f6")
	writeLegacyTestSession(t, store, cpID, "fresh-session")

	session := readRawKeys(t, store, cpID.Path()+"/0/"+paths.MetadataFileName)
	root := readRawKeys(t, store, cpID.Path()+"/"+paths.MetadataFileName)
	for _, key := range []string{"initial_attribution", "prompt_attributions"} {
		assert.NotContains(t, session, key)
	}
	assert.NotContains(t, root, "combined_attribution")
}

// A JSON round-trip of the API types keeps the legacy keys and omits them
// when absent.
func TestMetadataRoundTripKeepsLegacyAttribution(t *testing.T) {
	t.Parallel()
	in := `{"checkpoint_id":"a1b2c3d4e5f6","session_id":"s","strategy":"manual-commit","created_at":"2026-01-02T03:04:05Z","checkpoints_count":1,"files_touched":["a.go"],"initial_attribution":` +
		legacyInitialAttribution + `,"prompt_attributions":` + legacyPromptAttributions + `}`
	var m Metadata
	require.NoError(t, json.Unmarshal([]byte(in), &m))
	out, err := json.Marshal(m)
	require.NoError(t, err)
	var doc map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(out, &doc))
	assert.JSONEq(t, legacyInitialAttribution, string(doc["initial_attribution"]))
	assert.JSONEq(t, legacyPromptAttributions, string(doc["prompt_attributions"]))

	empty, err := json.Marshal(Metadata{})
	require.NoError(t, err)
	assert.NotContains(t, string(empty), "attribution")

	var sum CheckpointSummary
	require.NoError(t, json.Unmarshal([]byte(`{"checkpoint_id":"a1b2c3d4e5f6","combined_attribution":`+legacyCombinedAttribution+`}`), &sum))
	out, err = json.Marshal(sum)
	require.NoError(t, err)
	assert.Contains(t, string(out), `"combined_attribution"`)
}
