package checkpoint

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/jsonutil"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/entireio/cli/cmd/entire/cli/versioninfo"
	"github.com/entireio/cli/redact"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing/filemode"
	"github.com/go-git/go-git/v6/plumbing/object"
)

func TestWriteStandardCheckpointEntries_RefusesUnexpectedSessionZeroOverwrite(t *testing.T) {
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	testutil.InitRepo(t, tmpDir)
	repo, err := git.PlainOpen(tmpDir)
	if err != nil {
		t.Fatalf("PlainOpen() error = %v", err)
	}
	store := NewGitStore(repo, DefaultV1Refs())

	// The write path warns through the context's logger; give it a real one so
	// the tripwire's diagnostics land in the log file instead of the terminal.
	l, err := logging.New(logging.Config{Dir: filepath.Join(tmpDir, logging.LogsDir)})
	if err != nil {
		t.Fatalf("logging.New() error = %v", err)
	}
	defer func() { _ = l.Close() }()
	ctx := logging.WithLogger(context.Background(), l)

	checkpointID, err := id.Generate()
	if err != nil {
		t.Fatalf("id.Generate() error = %v", err)
	}
	basePath := checkpointID.Path() + "/"

	oldMetadata := Metadata{
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

	opts := WriteOptions{
		CheckpointID: checkpointID,
		SessionID:    "session-new",
		Strategy:     "manual-commit",
		Transcript:   redact.AlreadyRedacted([]byte("{\"type\":\"user\",\"message\":\"hi\"}\n")),
		Prompts:      []string{"hi"},
	}

	err = store.writeStandardCheckpointEntries(ctx, opts, basePath, entries)
	if err == nil {
		t.Fatal("expected writeStandardCheckpointEntries to refuse, got nil error")
	}
	if !strings.Contains(err.Error(), "refusing to overwrite session 0") {
		t.Errorf("error message should announce the refuse; got: %v", err)
	}
	if !strings.Contains(err.Error(), "session-old") || !strings.Contains(err.Error(), "session-new") {
		t.Errorf("error should include both session IDs; got: %v", err)
	}

	// Alice's original metadata must remain untouched in the entries map —
	// the refuse runs before writeSessionToSubdirectory clears the subtree.
	entry, ok := entries[sessionZeroPath]
	if !ok {
		t.Fatalf("session 0 metadata entry unexpectedly removed from entries map")
	}
	if entry.Hash != oldMetadataHash {
		t.Errorf("session 0 metadata blob changed: got %s, want %s", entry.Hash, oldMetadataHash)
	}
}
