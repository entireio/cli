package trail

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-git/go-git/v6"
)

// initTestRepo creates a test git repository with an initial commit.
func initTestRepo(t *testing.T) *git.Repository {
	t.Helper()

	dir := t.TempDir()

	ctx := context.Background()
	cmds := [][]string{
		{"git", "init", dir},
		{"git", "-C", dir, "config", "user.name", "Test"},
		{"git", "-C", dir, "config", "user.email", "test@test.com"},
		{"git", "-C", dir, "config", "commit.gpgsign", "false"},
	}
	for _, args := range cmds {
		cmd := exec.CommandContext(ctx, args[0], args[1:]...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("command %v failed: %v\n%s", args, err, out)
		}
	}

	// Create a file and commit
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# Test"), 0o644); err != nil {
		t.Fatalf("failed to write file: %v", err)
	}
	commitCmds := [][]string{
		{"git", "-C", dir, "add", "."},
		{"git", "-C", dir, "commit", "-m", "Initial commit"},
	}
	for _, args := range commitCmds {
		cmd := exec.CommandContext(ctx, args[0], args[1:]...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("command %v failed: %v\n%s", args, err, out)
		}
	}

	repo, err := git.PlainOpen(dir)
	if err != nil {
		t.Fatalf("failed to open repo: %v", err)
	}
	return repo
}

func TestStore_EnsureBranch(t *testing.T) {
	t.Parallel()
	repo := initTestRepo(t)
	store := NewStore(repo)

	// First call should create the branch
	if err := store.EnsureBranch(); err != nil {
		t.Fatalf("EnsureBranch() error = %v", err)
	}

	// Second call should be idempotent
	if err := store.EnsureBranch(); err != nil {
		t.Fatalf("EnsureBranch() second call error = %v", err)
	}
}

func TestStore_WriteAndRead(t *testing.T) {
	t.Parallel()
	repo := initTestRepo(t)
	store := NewStore(repo)

	trailID, err := GenerateID()
	if err != nil {
		t.Fatalf("GenerateID() error = %v", err)
	}

	now := time.Now().Truncate(time.Second)
	metadata := &Metadata{
		TrailID:   trailID,
		Branch:    "feature/test",
		Base:      "main",
		Title:     "Test trail",
		Body:      "A test trail",
		Status:    StatusDraft,
		Author:    "tester",
		Assignees: []string{},
		Labels:    []string{"test"},
		CreatedAt: now,
		UpdatedAt: now,
	}

	discussion := &Discussion{Comments: []Comment{}}

	if err := store.Write(metadata, discussion, nil); err != nil {
		t.Fatalf("Write() error = %v", err)
	}

	// Read it back
	gotMeta, gotDisc, _, err := store.Read(trailID)
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}

	if gotMeta.TrailID != trailID {
		t.Errorf("Read() trail_id = %s, want %s", gotMeta.TrailID, trailID)
	}
	if gotMeta.Branch != "feature/test" {
		t.Errorf("Read() branch = %q, want %q", gotMeta.Branch, "feature/test")
	}
	if gotMeta.Title != "Test trail" {
		t.Errorf("Read() title = %q, want %q", gotMeta.Title, "Test trail")
	}
	if gotMeta.Status != StatusDraft {
		t.Errorf("Read() status = %q, want %q", gotMeta.Status, StatusDraft)
	}
	if len(gotMeta.Labels) != 1 || gotMeta.Labels[0] != "test" {
		t.Errorf("Read() labels = %v, want [test]", gotMeta.Labels)
	}
	if gotDisc == nil {
		t.Error("Read() discussion should not be nil")
	}
}

func TestStore_FindByBranch(t *testing.T) {
	t.Parallel()
	repo := initTestRepo(t)
	store := NewStore(repo)

	now := time.Now()

	// Create two trails for different branches
	for _, branch := range []string{"feature/a", "feature/b"} {
		id, err := GenerateID()
		if err != nil {
			t.Fatalf("GenerateID() error = %v", err)
		}
		meta := &Metadata{
			TrailID:   id,
			Branch:    branch,
			Base:      "main",
			Title:     HumanizeBranchName(branch),
			Status:    StatusDraft,
			Author:    "test",
			Assignees: []string{},
			Labels:    []string{},
			CreatedAt: now,
			UpdatedAt: now,
		}
		if err := store.Write(meta, nil, nil); err != nil {
			t.Fatalf("Write() error = %v", err)
		}
	}

	// Find by branch
	found, err := store.FindByBranch("feature/a")
	if err != nil {
		t.Fatalf("FindByBranch() error = %v", err)
	}
	if found == nil {
		t.Fatal("FindByBranch() returned nil, expected trail")
	}
	if found.Branch != "feature/a" {
		t.Errorf("FindByBranch() branch = %q, want %q", found.Branch, "feature/a")
	}

	// Not found
	notFound, err := store.FindByBranch("feature/c")
	if err != nil {
		t.Fatalf("FindByBranch() error = %v", err)
	}
	if notFound != nil {
		t.Error("FindByBranch() should return nil for non-existent branch")
	}
}

func TestStore_List(t *testing.T) {
	t.Parallel()
	repo := initTestRepo(t)
	store := NewStore(repo)

	// List when no trails exist (branch doesn't exist yet)
	trails, err := store.List()
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if trails != nil {
		t.Errorf("List() = %v, want nil for empty store", trails)
	}

	// Create a trail
	now := time.Now()
	id, err := GenerateID()
	if err != nil {
		t.Fatalf("GenerateID() error = %v", err)
	}
	meta := &Metadata{
		TrailID:   id,
		Branch:    "feature/test",
		Base:      "main",
		Title:     "Test",
		Status:    StatusDraft,
		Author:    "test",
		Assignees: []string{},
		Labels:    []string{},
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := store.Write(meta, nil, nil); err != nil {
		t.Fatalf("Write() error = %v", err)
	}

	trails, err = store.List()
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(trails) != 1 {
		t.Fatalf("List() returned %d trails, want 1", len(trails))
	}
	if trails[0].TrailID != id {
		t.Errorf("List()[0].TrailID = %s, want %s", trails[0].TrailID, id)
	}
}

func TestStore_Update(t *testing.T) {
	t.Parallel()
	repo := initTestRepo(t)
	store := NewStore(repo)

	now := time.Now()
	id, err := GenerateID()
	if err != nil {
		t.Fatalf("GenerateID() error = %v", err)
	}
	meta := &Metadata{
		TrailID:   id,
		Branch:    "feature/test",
		Base:      "main",
		Title:     "Original",
		Status:    StatusDraft,
		Author:    "test",
		Assignees: []string{},
		Labels:    []string{},
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := store.Write(meta, nil, nil); err != nil {
		t.Fatalf("Write() error = %v", err)
	}

	// Update
	if err := store.Update(id, func(m *Metadata) {
		m.Title = "Updated"
		m.Status = StatusInProgress
		m.Labels = []string{"urgent"}
	}); err != nil {
		t.Fatalf("Update() error = %v", err)
	}

	// Verify
	updated, _, _, err := store.Read(id)
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if updated.Title != "Updated" {
		t.Errorf("Read() title = %q, want %q", updated.Title, "Updated")
	}
	if updated.Status != StatusInProgress {
		t.Errorf("Read() status = %q, want %q", updated.Status, StatusInProgress)
	}
	if len(updated.Labels) != 1 || updated.Labels[0] != "urgent" {
		t.Errorf("Read() labels = %v, want [urgent]", updated.Labels)
	}
	if !updated.UpdatedAt.After(now) {
		t.Error("Read() updated_at should be after original")
	}
}

func TestStore_Delete(t *testing.T) {
	t.Parallel()
	repo := initTestRepo(t)
	store := NewStore(repo)

	now := time.Now()
	id, err := GenerateID()
	if err != nil {
		t.Fatalf("GenerateID() error = %v", err)
	}
	meta := &Metadata{
		TrailID:   id,
		Branch:    "feature/test",
		Base:      "main",
		Title:     "To delete",
		Status:    StatusDraft,
		Author:    "test",
		Assignees: []string{},
		Labels:    []string{},
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := store.Write(meta, nil, nil); err != nil {
		t.Fatalf("Write() error = %v", err)
	}

	// Delete
	if err := store.Delete(id); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}

	// Verify it's gone
	_, _, _, err = store.Read(id)
	if err == nil {
		t.Error("Read() should fail after delete")
	}
}

func TestStore_ReadNonExistent(t *testing.T) {
	t.Parallel()
	repo := initTestRepo(t)
	store := NewStore(repo)

	if err := store.EnsureBranch(); err != nil {
		t.Fatalf("EnsureBranch() error = %v", err)
	}

	_, _, _, err := store.Read(ID("abcdef123456"))
	if err == nil {
		t.Error("Read() should fail for non-existent trail")
	}
}

func TestStore_ReadInvalidID(t *testing.T) {
	t.Parallel()
	repo := initTestRepo(t)
	store := NewStore(repo)

	// Invalid format: too short
	_, _, _, err := store.Read(ID("abc"))
	if err == nil {
		t.Error("Read() should fail for invalid trail ID")
	}

	// Path traversal attempt
	_, _, _, err = store.Read(ID("../../etc/pass"))
	if err == nil {
		t.Error("Read() should fail for path traversal ID")
	}
}

func TestStore_DeleteInvalidID(t *testing.T) {
	t.Parallel()
	repo := initTestRepo(t)
	store := NewStore(repo)

	// Invalid format: uppercase hex
	err := store.Delete(ID("ABCDEF123456"))
	if err == nil {
		t.Error("Delete() should fail for invalid trail ID")
	}

	// Path traversal attempt
	err = store.Delete(ID("../../../etc"))
	if err == nil {
		t.Error("Delete() should fail for path traversal ID")
	}
}

func TestStore_AddCheckpointPreservesOtherFields(t *testing.T) {
	t.Parallel()
	repo := initTestRepo(t)
	store := NewStore(repo)

	trailID, err := GenerateID()
	if err != nil {
		t.Fatalf("GenerateID() error = %v", err)
	}

	now := time.Now().Truncate(time.Second)
	metadata := &Metadata{
		TrailID:   trailID,
		Branch:    "feature/preserve",
		Base:      "main",
		Title:     "Preservation test",
		Body:      "Verify AddCheckpoint doesn't corrupt other fields",
		Status:    StatusInProgress,
		Author:    "tester",
		Assignees: []string{"alice"},
		Labels:    []string{"important"},
		CreatedAt: now,
		UpdatedAt: now,
	}
	discussion := &Discussion{Comments: []Comment{
		{ID: "c1", Author: "bob", Body: "looks good", CreatedAt: now},
	}}

	if err := store.Write(metadata, discussion, nil); err != nil {
		t.Fatalf("Write() error = %v", err)
	}

	// Add a checkpoint
	firstSummary := "first checkpoint"
	cpRef := CheckpointRef{
		CheckpointID: "aabbccddeeff",
		CommitSHA:    "deadbeef1234",
		CreatedAt:    now,
		Summary:      &firstSummary,
	}
	if err := store.AddCheckpoint(trailID, cpRef); err != nil {
		t.Fatalf("AddCheckpoint() error = %v", err)
	}

	// Read back and verify metadata + discussion are unchanged
	gotMeta, gotDisc, gotCPs, err := store.Read(trailID)
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}

	// Metadata unchanged
	if gotMeta.Title != "Preservation test" {
		t.Errorf("metadata title changed: got %q, want %q", gotMeta.Title, "Preservation test")
	}
	if gotMeta.Body != "Verify AddCheckpoint doesn't corrupt other fields" {
		t.Errorf("metadata body changed: got %q", gotMeta.Body)
	}
	if gotMeta.Status != StatusInProgress {
		t.Errorf("metadata status changed: got %q, want %q", gotMeta.Status, StatusInProgress)
	}
	if len(gotMeta.Assignees) != 1 || gotMeta.Assignees[0] != "alice" {
		t.Errorf("metadata assignees changed: got %v", gotMeta.Assignees)
	}
	if len(gotMeta.Labels) != 1 || gotMeta.Labels[0] != "important" {
		t.Errorf("metadata labels changed: got %v", gotMeta.Labels)
	}

	// Discussion unchanged
	if len(gotDisc.Comments) != 1 {
		t.Fatalf("discussion comments count = %d, want 1", len(gotDisc.Comments))
	}
	if gotDisc.Comments[0].ID != "c1" || gotDisc.Comments[0].Body != "looks good" {
		t.Errorf("discussion comment changed: got %+v", gotDisc.Comments[0])
	}

	// Checkpoint added correctly
	if len(gotCPs.Checkpoints) != 1 {
		t.Fatalf("checkpoints count = %d, want 1", len(gotCPs.Checkpoints))
	}
	if gotCPs.Checkpoints[0].CheckpointID != "aabbccddeeff" {
		t.Errorf("checkpoint ID = %q, want %q", gotCPs.Checkpoints[0].CheckpointID, "aabbccddeeff")
	}

	// Add a second checkpoint — should prepend
	secondSummary := "second checkpoint"
	cpRef2 := CheckpointRef{
		CheckpointID: "112233445566",
		CommitSHA:    "cafebabe5678",
		CreatedAt:    now,
		Summary:      &secondSummary,
	}
	if err := store.AddCheckpoint(trailID, cpRef2); err != nil {
		t.Fatalf("AddCheckpoint() second call error = %v", err)
	}

	_, _, gotCPs2, err := store.Read(trailID)
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if len(gotCPs2.Checkpoints) != 2 {
		t.Fatalf("checkpoints count = %d, want 2", len(gotCPs2.Checkpoints))
	}
	if gotCPs2.Checkpoints[0].CheckpointID != "112233445566" {
		t.Errorf("newest checkpoint should be first, got %q", gotCPs2.Checkpoints[0].CheckpointID)
	}
	if gotCPs2.Checkpoints[1].CheckpointID != "aabbccddeeff" {
		t.Errorf("older checkpoint should be second, got %q", gotCPs2.Checkpoints[1].CheckpointID)
	}
}
