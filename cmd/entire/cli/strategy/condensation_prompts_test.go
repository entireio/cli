package strategy

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/antigravity"
	"github.com/entireio/cli/cmd/entire/cli/gitrepo"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/stretchr/testify/require"
)

// The prompt ladder's last rung must work from the transcript BYTES being
// checkpointed. When condensation fell back to the shadow-branch copy because
// the live path could not be read, a re-read of the path found nothing and the
// checkpoint carried a full transcript with no prompt.
func TestResolveCondensationPrompts_UsesTranscriptBytesWhenLivePathIsGone(t *testing.T) {
	t.Parallel()
	ag := antigravity.NewAntigravityAgent()
	transcript := []byte(`{"type":"USER_INPUT","content":"<USER_REQUEST>\nfirst ask\n</USER_REQUEST>"}
{"type":"PLANNER_RESPONSE"}
{"type":"USER_INPUT","content":"<USER_REQUEST>\nadd another\n</USER_REQUEST>"}
`)
	missing := filepath.Join(t.TempDir(), "gone.jsonl")

	got := resolveCondensationPrompts(context.Background(), ag, transcript, missing, 2)
	require.Equal(t, []string{"add another"}, got)

	// Without bytes in hand the rung degrades to the path-based read, which
	// finds nothing here — the behaviour this test exists to stop relying on.
	require.Nil(t, resolveCondensationPrompts(context.Background(), ag, nil, missing, 2))
}

// buildShadowRepo commits files into a throwaway repo and returns it with the
// commit hash, so extractSessionData can be driven against a crafted tree.
func buildShadowRepo(t *testing.T, files map[string]string) (*git.Repository, plumbing.Hash) {
	t.Helper()
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	repo, err := gitrepo.OpenPath(dir)
	require.NoError(t, err)

	for path, content := range files {
		abs := filepath.Join(dir, filepath.FromSlash(path))
		require.NoError(t, os.MkdirAll(filepath.Dir(abs), 0o750))
		require.NoError(t, os.WriteFile(abs, []byte(content), 0o600))
	}
	wt, err := repo.Worktree()
	require.NoError(t, err)
	_, err = wt.Add(".")
	require.NoError(t, err)
	hash, err := wt.Commit("shadow", &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@test.com"},
	})
	require.NoError(t, err)
	return repo, hash
}

// TestExtractSessionData_ResolvesPromptsWithEmptyTranscript pins the prompt
// ladder OUTSIDE the transcript gate on the shadow-branch path. A
// LateTranscriptWriter routinely checkpoints an empty placeholder mid-turn;
// while the ladder sat inside `if fullTranscript != ""` that produced a
// checkpoint with no prompt from ANY source — not even the prompt.txt sitting
// in the very tree being read — and silenced logCondensationPrompts with it.
func TestExtractSessionData_ResolvesPromptsWithEmptyTranscript(t *testing.T) {
	t.Parallel()

	const sessionID = "20260924-agy-empty-transcript"
	metadataDir := paths.SessionMetadataDirFromSessionID(sessionID)
	repo, hash := buildShadowRepo(t, map[string]string{
		// The empty placeholder PrepareTranscript materialises when Stop beats
		// agy's flush, exactly as SaveStep checkpointed it.
		metadataDir + "/" + paths.TranscriptFileName: "",
		metadataDir + "/" + paths.PromptFileName:     "the prompt that must survive",
	})

	s := &ManualCommitStrategy{}
	data, err := s.extractSessionData(context.Background(), repo, hash, sessionID, nil,
		agent.AgentTypeAntigravity, "", 0, false)
	require.NoError(t, err)
	require.Empty(t, data.Transcript, "sanity: this is the empty-transcript case")
	require.Equal(t, []string{"the prompt that must survive"}, data.Prompts,
		"prompt resolution must not depend on transcript content")
}
