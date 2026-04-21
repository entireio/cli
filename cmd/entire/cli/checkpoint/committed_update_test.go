package checkpoint

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/redact"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/config"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/object"
)

// setupRepoForUpdate creates a repo with an initial commit and writes a committed checkpoint.
func setupRepoForUpdate(t *testing.T) (*git.Repository, *GitStore, id.CheckpointID) {
	t.Helper()

	tempDir := t.TempDir()
	repo, err := git.PlainInit(tempDir, false)
	if err != nil {
		t.Fatalf("failed to init git repo: %v", err)
	}

	worktree, err := repo.Worktree()
	if err != nil {
		t.Fatalf("failed to get worktree: %v", err)
	}

	readmeFile := filepath.Join(tempDir, "README.md")
	if err := os.WriteFile(readmeFile, []byte("# Test"), 0o644); err != nil {
		t.Fatalf("failed to write README: %v", err)
	}
	if _, err := worktree.Add("README.md"); err != nil {
		t.Fatalf("failed to add README: %v", err)
	}
	if _, err := worktree.Commit("Initial commit", &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@test.com"},
	}); err != nil {
		t.Fatalf("failed to commit: %v", err)
	}

	store := NewGitStore(repo)
	cpID := id.MustCheckpointID("a1b2c3d4e5f6")

	err = store.WriteCommitted(context.Background(), WriteCommittedOptions{
		CheckpointID: cpID,
		SessionID:    "session-001",
		Strategy:     "manual-commit",
		Transcript:   redact.AlreadyRedacted([]byte("provisional transcript line 1\n")),
		Prompts:      []string{"initial prompt"},
		AuthorName:   "Test",
		AuthorEmail:  "test@test.com",
	})
	if err != nil {
		t.Fatalf("WriteCommitted() error = %v", err)
	}

	return repo, store, cpID
}

func TestUpdateCommitted_ReplacesTranscript(t *testing.T) {
	t.Parallel()
	_, store, cpID := setupRepoForUpdate(t)

	// Update with full transcript (replace semantics)
	fullTranscript := []byte("full transcript line 1\nfull transcript line 2\nfull transcript line 3\n")
	err := store.UpdateCommitted(context.Background(), UpdateCommittedOptions{
		CheckpointID: cpID,
		SessionID:    "session-001",
		Transcript:   redact.AlreadyRedacted(fullTranscript),
	})
	if err != nil {
		t.Fatalf("UpdateCommitted() error = %v", err)
	}

	// Read back and verify transcript was replaced (not appended)
	content, err := store.ReadSessionContent(context.Background(), cpID, 0)
	if err != nil {
		t.Fatalf("ReadSessionContent() error = %v", err)
	}

	if string(content.Transcript) != string(fullTranscript) {
		t.Errorf("transcript mismatch\ngot:  %q\nwant: %q", string(content.Transcript), string(fullTranscript))
	}
}

func TestUpdateCommitted_ReplacesPrompts(t *testing.T) {
	t.Parallel()
	_, store, cpID := setupRepoForUpdate(t)

	err := store.UpdateCommitted(context.Background(), UpdateCommittedOptions{
		CheckpointID: cpID,
		SessionID:    "session-001",
		Prompts:      []string{"prompt 1", "prompt 2", "prompt 3"},
	})
	if err != nil {
		t.Fatalf("UpdateCommitted() error = %v", err)
	}

	content, err := store.ReadSessionContent(context.Background(), cpID, 0)
	if err != nil {
		t.Fatalf("ReadSessionContent() error = %v", err)
	}

	expected := "prompt 1\n\n---\n\nprompt 2\n\n---\n\nprompt 3"
	if content.Prompts != expected {
		t.Errorf("prompts mismatch\ngot:  %q\nwant: %q", content.Prompts, expected)
	}
}

func TestUpdateCommitted_ReplacesAllFieldsTogether(t *testing.T) {
	t.Parallel()
	_, store, cpID := setupRepoForUpdate(t)

	fullTranscript := []byte("complete transcript\n")
	err := store.UpdateCommitted(context.Background(), UpdateCommittedOptions{
		CheckpointID: cpID,
		SessionID:    "session-001",
		Transcript:   redact.AlreadyRedacted(fullTranscript),
		Prompts:      []string{"final prompt"},
	})
	if err != nil {
		t.Fatalf("UpdateCommitted() error = %v", err)
	}

	content, err := store.ReadSessionContent(context.Background(), cpID, 0)
	if err != nil {
		t.Fatalf("ReadSessionContent() error = %v", err)
	}

	if string(content.Transcript) != string(fullTranscript) {
		t.Errorf("transcript mismatch\ngot:  %q\nwant: %q", string(content.Transcript), string(fullTranscript))
	}
	if content.Prompts != "final prompt" {
		t.Errorf("prompts mismatch\ngot:  %q\nwant: %q", content.Prompts, "final prompt")
	}
}

func TestUpdateCommitted_NonexistentCheckpoint(t *testing.T) {
	t.Parallel()
	_, store, _ := setupRepoForUpdate(t)

	err := store.UpdateCommitted(context.Background(), UpdateCommittedOptions{
		CheckpointID: id.MustCheckpointID("deadbeef1234"),
		SessionID:    "session-001",
		Transcript:   redact.AlreadyRedacted([]byte("should fail")),
	})
	if err == nil {
		t.Fatal("expected error for nonexistent checkpoint, got nil")
	}
}

func TestUpdateCommitted_PreservesMetadata(t *testing.T) {
	t.Parallel()
	_, store, cpID := setupRepoForUpdate(t)

	// Read metadata before update
	contentBefore, err := store.ReadSessionContent(context.Background(), cpID, 0)
	if err != nil {
		t.Fatalf("ReadSessionContent() before error = %v", err)
	}

	// Update only transcript
	err = store.UpdateCommitted(context.Background(), UpdateCommittedOptions{
		CheckpointID: cpID,
		SessionID:    "session-001",
		Transcript:   redact.AlreadyRedacted([]byte("updated transcript\n")),
	})
	if err != nil {
		t.Fatalf("UpdateCommitted() error = %v", err)
	}

	// Read metadata after update - should be unchanged
	contentAfter, err := store.ReadSessionContent(context.Background(), cpID, 0)
	if err != nil {
		t.Fatalf("ReadSessionContent() after error = %v", err)
	}

	if contentAfter.Metadata.SessionID != contentBefore.Metadata.SessionID {
		t.Errorf("session ID changed: %q -> %q", contentBefore.Metadata.SessionID, contentAfter.Metadata.SessionID)
	}
	if contentAfter.Metadata.Strategy != contentBefore.Metadata.Strategy {
		t.Errorf("strategy changed: %q -> %q", contentBefore.Metadata.Strategy, contentAfter.Metadata.Strategy)
	}
}

func TestUpdateCommitted_MultipleCheckpoints(t *testing.T) {
	t.Parallel()
	_, store, cpID1 := setupRepoForUpdate(t)

	// Write a second checkpoint
	cpID2 := id.MustCheckpointID("b2c3d4e5f6a1")
	err := store.WriteCommitted(context.Background(), WriteCommittedOptions{
		CheckpointID: cpID2,
		SessionID:    "session-001",
		Strategy:     "manual-commit",
		Transcript:   redact.AlreadyRedacted([]byte("provisional cp2\n")),
		Prompts:      []string{"cp2 prompt"},
		AuthorName:   "Test",
		AuthorEmail:  "test@test.com",
	})
	if err != nil {
		t.Fatalf("WriteCommitted(cp2) error = %v", err)
	}

	fullTranscript := []byte("complete full transcript\n")

	// Update both checkpoints with the same full transcript
	for _, cpID := range []id.CheckpointID{cpID1, cpID2} {
		err = store.UpdateCommitted(context.Background(), UpdateCommittedOptions{
			CheckpointID: cpID,
			SessionID:    "session-001",
			Transcript:   redact.AlreadyRedacted(fullTranscript),
			Prompts:      []string{"final prompt 1", "final prompt 2"},
		})
		if err != nil {
			t.Fatalf("UpdateCommitted(%s) error = %v", cpID, err)
		}
	}

	// Verify both have the full transcript
	for _, cpID := range []id.CheckpointID{cpID1, cpID2} {
		content, readErr := store.ReadSessionContent(context.Background(), cpID, 0)
		if readErr != nil {
			t.Fatalf("ReadSessionContent(%s) error = %v", cpID, readErr)
		}
		if string(content.Transcript) != string(fullTranscript) {
			t.Errorf("checkpoint %s: transcript mismatch\ngot:  %q\nwant: %q", cpID, string(content.Transcript), string(fullTranscript))
		}
	}
}

func TestUpdateCommitted_UpdatesContentHash(t *testing.T) {
	t.Parallel()
	repo, store, cpID := setupRepoForUpdate(t)

	// Update transcript
	err := store.UpdateCommitted(context.Background(), UpdateCommittedOptions{
		CheckpointID: cpID,
		SessionID:    "session-001",
		Transcript:   redact.AlreadyRedacted([]byte("new full transcript content\n")),
	})
	if err != nil {
		t.Fatalf("UpdateCommitted() error = %v", err)
	}

	// Verify content_hash.txt was updated
	ref, err := repo.Reference(plumbing.NewBranchReferenceName(paths.MetadataBranchName), true)
	if err != nil {
		t.Fatalf("failed to get ref: %v", err)
	}
	commit, err := repo.CommitObject(ref.Hash())
	if err != nil {
		t.Fatalf("failed to get commit: %v", err)
	}
	tree, err := commit.Tree()
	if err != nil {
		t.Fatalf("failed to get tree: %v", err)
	}

	hashPath := cpID.Path() + "/0/" + paths.ContentHashFileName
	hashFile, err := tree.File(hashPath)
	if err != nil {
		t.Fatalf("content_hash.txt not found at %s: %v", hashPath, err)
	}
	hashContent, err := hashFile.Contents()
	if err != nil {
		t.Fatalf("failed to read content_hash.txt: %v", err)
	}

	// Hash should be for the new content, not the old
	if hashContent == "" || !isValidContentHash(hashContent) {
		t.Errorf("invalid content hash: %q", hashContent)
	}
}

func TestUpdateCommitted_EmptyCheckpointID(t *testing.T) {
	t.Parallel()
	_, store, _ := setupRepoForUpdate(t)

	err := store.UpdateCommitted(context.Background(), UpdateCommittedOptions{
		SessionID:  "session-001",
		Transcript: redact.AlreadyRedacted([]byte("should fail")),
	})
	if err == nil {
		t.Fatal("expected error for empty checkpoint ID, got nil")
	}
}

func TestUpdateCommitted_FallsBackToLatestSession(t *testing.T) {
	t.Parallel()
	_, store, cpID := setupRepoForUpdate(t)

	// Update with wrong session ID — should fall back to latest (index 0)
	fullTranscript := []byte("updated via fallback\n")
	err := store.UpdateCommitted(context.Background(), UpdateCommittedOptions{
		CheckpointID: cpID,
		SessionID:    "nonexistent-session",
		Transcript:   redact.AlreadyRedacted(fullTranscript),
	})
	if err != nil {
		t.Fatalf("UpdateCommitted() error = %v", err)
	}

	// Verify transcript was updated on the latest (and only) session
	content, err := store.ReadSessionContent(context.Background(), cpID, 0)
	if err != nil {
		t.Fatalf("ReadSessionContent() error = %v", err)
	}
	if string(content.Transcript) != string(fullTranscript) {
		t.Errorf("transcript mismatch\ngot:  %q\nwant: %q", string(content.Transcript), string(fullTranscript))
	}
}

func TestUpdateCommitted_SummaryPreserved(t *testing.T) {
	t.Parallel()
	_, store, cpID := setupRepoForUpdate(t)

	// Verify the root-level CheckpointSummary is preserved after update
	summaryBefore, err := store.ReadCommitted(context.Background(), cpID)
	if err != nil {
		t.Fatalf("ReadCommitted() before error = %v", err)
	}

	err = store.UpdateCommitted(context.Background(), UpdateCommittedOptions{
		CheckpointID: cpID,
		SessionID:    "session-001",
		Transcript:   redact.AlreadyRedacted([]byte("updated\n")),
	})
	if err != nil {
		t.Fatalf("UpdateCommitted() error = %v", err)
	}

	summaryAfter, err := store.ReadCommitted(context.Background(), cpID)
	if err != nil {
		t.Fatalf("ReadCommitted() after error = %v", err)
	}

	if summaryAfter.CheckpointID != summaryBefore.CheckpointID {
		t.Errorf("checkpoint ID changed in summary")
	}
	if len(summaryAfter.Sessions) != len(summaryBefore.Sessions) {
		t.Errorf("sessions count changed: %d -> %d", len(summaryBefore.Sessions), len(summaryAfter.Sessions))
	}
}

func isValidContentHash(hash string) bool {
	return len(hash) > 10 && hash[:7] == "sha256:"
}

// Verify JSON serialization of the new fields on State
func TestState_TurnCheckpointIDs_JSON(t *testing.T) {
	t.Parallel()

	type stateSnippet struct {
		TurnCheckpointIDs []string `json:"turn_checkpoint_ids,omitempty"`
	}

	// Test round-trip
	original := stateSnippet{
		TurnCheckpointIDs: []string{"a1b2c3d4e5f6", "b2c3d4e5f6a1"},
	}
	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal error: %v", err)
	}

	var decoded stateSnippet
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal error: %v", err)
	}

	if len(decoded.TurnCheckpointIDs) != 2 {
		t.Errorf("expected 2 IDs, got %d", len(decoded.TurnCheckpointIDs))
	}

	// Test nil serialization (omitempty)
	empty := stateSnippet{}
	data, err = json.Marshal(empty)
	if err != nil {
		t.Fatalf("Marshal error: %v", err)
	}
	if string(data) != "{}" {
		t.Errorf("expected empty JSON, got %s", string(data))
	}
}

// TestUpdateCommitted_UsesCorrectAuthor verifies that the "Finalize transcript"
// commit on entire/checkpoints/v1 gets the correct author from global git config,
// not "Unknown <unknown@local>".
func TestUpdateCommitted_UsesCorrectAuthor(t *testing.T) {
	// Cannot use t.Parallel() because subtests use t.Setenv.

	tests := []struct {
		name        string
		localName   string
		localEmail  string
		globalName  string
		globalEmail string
		wantName    string
		wantEmail   string
	}{
		{
			name:        "global config only",
			globalName:  "Global User",
			globalEmail: "global@example.com",
			wantName:    "Global User",
			wantEmail:   "global@example.com",
		},
		{
			name:       "local config takes precedence",
			localName:  "Local User",
			localEmail: "local@example.com",
			globalName: "Global User",
			wantName:   "Local User",
			wantEmail:  "local@example.com",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			useAutoConfigLoader(t)

			// Isolate global git config by pointing HOME to a temp dir
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("XDG_CONFIG_HOME", "")

			// Write global .gitconfig if needed
			if tt.globalName != "" || tt.globalEmail != "" {
				globalCfg := "[user]\n"
				if tt.globalName != "" {
					globalCfg += "\tname = " + tt.globalName + "\n"
				}
				if tt.globalEmail != "" {
					globalCfg += "\temail = " + tt.globalEmail + "\n"
				}
				if err := os.WriteFile(filepath.Join(home, ".gitconfig"), []byte(globalCfg), 0o644); err != nil {
					t.Fatalf("failed to write global gitconfig: %v", err)
				}
			}

			// Create repo
			dir := t.TempDir()
			repo, err := git.PlainInit(dir, false)
			if err != nil {
				t.Fatalf("failed to init repo: %v", err)
			}

			// Set local config if needed
			if tt.localName != "" || tt.localEmail != "" {
				cfg, err := repo.Config()
				if err != nil {
					t.Fatalf("failed to get repo config: %v", err)
				}
				cfg.User.Name = tt.localName
				cfg.User.Email = tt.localEmail
				if err := repo.SetConfig(cfg); err != nil {
					t.Fatalf("failed to set repo config: %v", err)
				}
			}

			// Create initial commit so repo has HEAD
			wt, err := repo.Worktree()
			if err != nil {
				t.Fatalf("failed to get worktree: %v", err)
			}
			readmeFile := filepath.Join(dir, "README.md")
			if err := os.WriteFile(readmeFile, []byte("# Test"), 0o644); err != nil {
				t.Fatalf("failed to write README: %v", err)
			}
			if _, err := wt.Add("README.md"); err != nil {
				t.Fatalf("failed to add README: %v", err)
			}
			if _, err := wt.Commit("Initial commit", &git.CommitOptions{
				Author: &object.Signature{Name: "Setup", Email: "setup@test.com"},
			}); err != nil {
				t.Fatalf("failed to commit: %v", err)
			}

			// Write initial checkpoint
			store := NewGitStore(repo)
			cpID := id.MustCheckpointID("a1b2c3d4e5f6")
			err = store.WriteCommitted(context.Background(), WriteCommittedOptions{
				CheckpointID: cpID,
				SessionID:    "session-001",
				Strategy:     "manual-commit",
				Transcript:   redact.AlreadyRedacted([]byte("provisional\n")),
				AuthorName:   tt.wantName,
				AuthorEmail:  tt.wantEmail,
			})
			if err != nil {
				t.Fatalf("WriteCommitted() error = %v", err)
			}

			// Call UpdateCommitted — this is the operation under test
			err = store.UpdateCommitted(context.Background(), UpdateCommittedOptions{
				CheckpointID: cpID,
				SessionID:    "session-001",
				Transcript:   redact.AlreadyRedacted([]byte("full transcript\n")),
			})
			if err != nil {
				t.Fatalf("UpdateCommitted() error = %v", err)
			}

			// Read the latest commit on entire/checkpoints/v1 and verify author
			ref, err := repo.Reference(plumbing.NewBranchReferenceName(paths.MetadataBranchName), true)
			if err != nil {
				t.Fatalf("failed to get sessions branch ref: %v", err)
			}
			commit, err := repo.CommitObject(ref.Hash())
			if err != nil {
				t.Fatalf("failed to get commit: %v", err)
			}

			if commit.Author.Name != tt.wantName {
				t.Errorf("commit author name = %q, want %q", commit.Author.Name, tt.wantName)
			}
			if commit.Author.Email != tt.wantEmail {
				t.Errorf("commit author email = %q, want %q", commit.Author.Email, tt.wantEmail)
			}
		})
	}
}

// TestGetGitAuthorFromRepo_GlobalFallback verifies that GetGitAuthorFromRepo
// falls back to global git config when local config is empty.
func TestGetGitAuthorFromRepo_GlobalFallback(t *testing.T) {
	// Cannot use t.Parallel() because we use t.Setenv.
	useAutoConfigLoader(t)

	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")

	// Write global .gitconfig with user info
	globalCfg := "[user]\n\tname = Global Author\n\temail = global@test.com\n"
	if err := os.WriteFile(filepath.Join(home, ".gitconfig"), []byte(globalCfg), 0o644); err != nil {
		t.Fatalf("failed to write global gitconfig: %v", err)
	}

	// Create repo with NO local user config
	dir := t.TempDir()
	repo, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("failed to init repo: %v", err)
	}

	name, email := GetGitAuthorFromRepo(repo)
	if name != "Global Author" {
		t.Errorf("name = %q, want %q", name, "Global Author")
	}
	if email != "global@test.com" {
		t.Errorf("email = %q, want %q", email, "global@test.com")
	}
}

// TestGetGitAuthorFromRepo_NoConfig verifies defaults when no config exists.
func TestGetGitAuthorFromRepo_NoConfig(t *testing.T) {
	// Cannot use t.Parallel() because we use t.Setenv.

	home := t.TempDir() // Empty home — no .gitconfig
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")

	dir := t.TempDir()
	repo, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("failed to init repo: %v", err)
	}

	name, email := GetGitAuthorFromRepo(repo)
	if name != "Unknown" {
		t.Errorf("name = %q, want %q", name, "Unknown")
	}
	if email != "unknown@local" {
		t.Errorf("email = %q, want %q", email, "unknown@local")
	}
}

// Verify go-git config import is used (compile-time check).
var _ = config.GlobalScope

// TestUpdateCommitted_PrecomputedBlobs_Roundtrip verifies that passing
// precomputed blob hashes produces the same on-disk tree content as the
// non-precomputed path.
func TestUpdateCommitted_PrecomputedBlobs_Roundtrip(t *testing.T) {
	t.Parallel()
	_, store, cpID := setupRepoForUpdate(t)

	transcript := redact.AlreadyRedacted([]byte("line1\nline2\nline3 with some payload\n"))

	precomputed, err := PrecomputeTranscriptBlobs(context.Background(), store.repo, transcript, "")
	if err != nil {
		t.Fatalf("PrecomputeTranscriptBlobs() error = %v", err)
	}
	if len(precomputed.ChunkHashes) == 0 {
		t.Fatal("precompute returned no chunk hashes")
	}
	if precomputed.ContentHashBlob.IsZero() {
		t.Fatal("precompute returned zero content-hash blob")
	}

	if err := store.UpdateCommitted(context.Background(), UpdateCommittedOptions{
		CheckpointID:     cpID,
		SessionID:        "session-001",
		Transcript:       transcript,
		PrecomputedBlobs: precomputed,
	}); err != nil {
		t.Fatalf("UpdateCommitted(precomputed) error = %v", err)
	}

	content, err := store.ReadSessionContent(context.Background(), cpID, 0)
	if err != nil {
		t.Fatalf("ReadSessionContent() error = %v", err)
	}
	if string(content.Transcript) != string(transcript.Bytes()) {
		t.Errorf("transcript mismatch via precomputed path\ngot:  %q\nwant: %q",
			string(content.Transcript), string(transcript.Bytes()))
	}
}

// TestUpdateCommitted_ContentHashShortCircuit verifies that a second update
// with identical transcript content skips chunking entirely (short-circuit
// fires before agent.ChunkTranscript is called).
func TestUpdateCommitted_ContentHashShortCircuit(t *testing.T) {
	// Cannot run in parallel: patches the package-level chunkTranscript hook.
	_, store, cpID := setupRepoForUpdate(t)

	transcript := redact.AlreadyRedacted([]byte("stable transcript content\n"))

	if err := store.UpdateCommitted(context.Background(), UpdateCommittedOptions{
		CheckpointID: cpID,
		SessionID:    "session-001",
		Transcript:   transcript,
	}); err != nil {
		t.Fatalf("UpdateCommitted(first) error = %v", err)
	}

	// Install a counter. The second UpdateCommitted with identical content
	// should never touch the chunking function.
	calls := installChunkCounter(t)

	if err := store.UpdateCommitted(context.Background(), UpdateCommittedOptions{
		CheckpointID: cpID,
		SessionID:    "session-001",
		Transcript:   transcript,
	}); err != nil {
		t.Fatalf("UpdateCommitted(second) error = %v", err)
	}

	if *calls != 0 {
		t.Errorf("short-circuit failed: chunkTranscript was called %d time(s) on a no-op re-update; expected 0", *calls)
	}
}

// installChunkCounter swaps the package-level chunkTranscript hook for a
// counter and restores it when the test completes. Returns a pointer the
// caller can dereference to read the running count.
func installChunkCounter(t *testing.T) *int {
	t.Helper()
	original := chunkTranscript
	t.Cleanup(func() { chunkTranscript = original })
	var count int
	chunkTranscript = func(ctx context.Context, content []byte, agentType types.AgentType) ([][]byte, error) {
		count++
		return original(ctx, content, agentType)
	}
	return &count
}

// TestUpdateCommitted_ContentChangedRewrites verifies the short-circuit does
// not fire when content actually differs.
func TestUpdateCommitted_ContentChangedRewrites(t *testing.T) {
	t.Parallel()
	repo, store, cpID := setupRepoForUpdate(t)

	first := redact.AlreadyRedacted([]byte("first version\n"))
	second := redact.AlreadyRedacted([]byte("second version with more content\n"))

	if err := store.UpdateCommitted(context.Background(), UpdateCommittedOptions{
		CheckpointID: cpID,
		SessionID:    "session-001",
		Transcript:   first,
	}); err != nil {
		t.Fatalf("UpdateCommitted(first) error = %v", err)
	}
	hashBefore := readTranscriptBlobHash(t, repo, cpID)

	if err := store.UpdateCommitted(context.Background(), UpdateCommittedOptions{
		CheckpointID: cpID,
		SessionID:    "session-001",
		Transcript:   second,
	}); err != nil {
		t.Fatalf("UpdateCommitted(second) error = %v", err)
	}
	hashAfter := readTranscriptBlobHash(t, repo, cpID)

	if hashBefore == hashAfter {
		t.Errorf("expected transcript blob to change; stayed at %v", hashBefore)
	}

	content, err := store.ReadSessionContent(context.Background(), cpID, 0)
	if err != nil {
		t.Fatalf("ReadSessionContent() error = %v", err)
	}
	if string(content.Transcript) != string(second.Bytes()) {
		t.Errorf("transcript content mismatch\ngot:  %q\nwant: %q",
			string(content.Transcript), string(second.Bytes()))
	}
}

// TestPrecomputeAndReuse_MatchesFreshWrite verifies that precomputed blob
// hashes match the hashes produced by a fresh chunk + blob-write pass.
func TestPrecomputeAndReuse_MatchesFreshWrite(t *testing.T) {
	t.Parallel()
	_, store, _ := setupRepoForUpdate(t)

	transcript := redact.AlreadyRedacted([]byte("deterministic content for hash comparison\n"))

	precomputed, err := PrecomputeTranscriptBlobs(context.Background(), store.repo, transcript, "")
	if err != nil {
		t.Fatalf("PrecomputeTranscriptBlobs() error = %v", err)
	}

	freshBlob, err := CreateBlobFromContent(store.repo, transcript.Bytes())
	if err != nil {
		t.Fatalf("CreateBlobFromContent() error = %v", err)
	}

	if len(precomputed.ChunkHashes) != 1 {
		t.Fatalf("expected 1 chunk for small transcript; got %d", len(precomputed.ChunkHashes))
	}
	if precomputed.ChunkHashes[0] != freshBlob {
		t.Errorf("precomputed chunk hash %v != fresh blob hash %v",
			precomputed.ChunkHashes[0], freshBlob)
	}
}

// readTranscriptBlobHash reads the transcript blob hash at session 0 from the
// metadata branch.
func readTranscriptBlobHash(t *testing.T, repo *git.Repository, cpID id.CheckpointID) plumbing.Hash {
	t.Helper()
	ref, err := repo.Reference(plumbing.NewBranchReferenceName(paths.MetadataBranchName), true)
	if err != nil {
		t.Fatalf("failed to get ref: %v", err)
	}
	commit, err := repo.CommitObject(ref.Hash())
	if err != nil {
		t.Fatalf("failed to get commit: %v", err)
	}
	tree, err := commit.Tree()
	if err != nil {
		t.Fatalf("failed to get tree: %v", err)
	}
	transcriptPath := cpID.Path() + "/0/" + paths.TranscriptFileName
	file, err := tree.File(transcriptPath)
	if err != nil {
		t.Fatalf("failed to find transcript blob at %s: %v", transcriptPath, err)
	}
	return file.Hash
}
