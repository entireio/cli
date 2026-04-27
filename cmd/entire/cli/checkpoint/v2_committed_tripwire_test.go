package checkpoint

import (
	"context"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/jsonutil"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/versioninfo"

	"github.com/go-git/go-git/v6/plumbing/filemode"
	"github.com/go-git/go-git/v6/plumbing/object"
)

// Mirrors TestWriteStandardCheckpointEntries_RefusesUnexpectedSessionZeroOverwrite
// but for the v2 store. Guards writeMainCheckpointEntries against the same
// corruption / stale-summary shape that we catch in v1.
func TestV2WriteMainCheckpointEntries_RefusesUnexpectedSessionZeroOverwrite(t *testing.T) {
	repo := initTestRepo(t)
	store := NewV2GitStore(repo, "origin")

	if err := logging.Init(context.Background(), ""); err != nil {
		t.Fatalf("logging.Init() error = %v", err)
	}
	defer logging.Close()

	checkpointID, err := id.Generate()
	if err != nil {
		t.Fatalf("id.Generate() error = %v", err)
	}
	basePath := checkpointID.Path() + "/"

	oldMetadata := CommittedMetadata{
		CheckpointID: checkpointID,
		SessionID:    "session-old",
		Strategy:     "manual-commit",
		CLIVersion:   versioninfo.Version,
	}
	oldMetadataJSON, err := jsonutil.MarshalIndentWithNewline(oldMetadata, "", "  ")
	if err != nil {
		t.Fatalf("marshal old metadata: %v", err)
	}
	oldMetadataHash, err := CreateBlobFromContent(repo, oldMetadataJSON)
	if err != nil {
		t.Fatalf("CreateBlobFromContent(old metadata) error = %v", err)
	}

	sessionZeroPath := basePath + "0/" + paths.MetadataFileName
	entries := map[string]object.TreeEntry{
		sessionZeroPath: {
			Name: sessionZeroPath,
			Mode: filemode.Regular,
			Hash: oldMetadataHash,
		},
	}

	opts := WriteCommittedOptions{
		CheckpointID: checkpointID,
		SessionID:    "session-new",
		Strategy:     "manual-commit",
		Prompts:      []string{"hi"},
	}

	_, err = store.writeMainCheckpointEntries(context.Background(), opts, basePath, entries)
	if err == nil {
		t.Fatal("expected writeMainCheckpointEntries to refuse, got nil error")
	}
	if !strings.Contains(err.Error(), "refusing to overwrite session 0") {
		t.Errorf("error message should announce the refuse; got: %v", err)
	}
	if !strings.Contains(err.Error(), "session-old") || !strings.Contains(err.Error(), "session-new") {
		t.Errorf("error should include both session IDs; got: %v", err)
	}

	// The original session-0 metadata entry must remain untouched — the refuse
	// runs before writeMainSessionToSubdirectory clears the subtree.
	entry, ok := entries[sessionZeroPath]
	if !ok {
		t.Fatalf("session 0 metadata entry unexpectedly removed from entries map")
	}
	if entry.Hash != oldMetadataHash {
		t.Errorf("session 0 metadata blob changed: got %s, want %s", entry.Hash, oldMetadataHash)
	}
}
