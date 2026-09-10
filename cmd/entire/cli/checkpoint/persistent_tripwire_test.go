package checkpoint

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/entiredir"
	"github.com/entireio/cli/cmd/entire/cli/jsonutil"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/entireio/cli/cmd/entire/cli/versioninfo"
	"github.com/entireio/cli/redact"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
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
	l, err := logging.New(logging.Config{Root: entiredir.OpenerAt(tmpDir), Dir: logging.LogsName})
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

// readSummaryBlob decodes a CheckpointSummary straight out of the object store.
func readSummaryBlob(t *testing.T, repo *git.Repository, hash plumbing.Hash) CheckpointSummary {
	t.Helper()
	blob, err := repo.BlobObject(hash)
	if err != nil {
		t.Fatalf("BlobObject() error = %v", err)
	}
	reader, err := blob.Reader()
	if err != nil {
		t.Fatalf("blob.Reader() error = %v", err)
	}
	defer func() { _ = reader.Close() }()
	data, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("read blob: %v", err)
	}
	var summary CheckpointSummary
	if err := json.Unmarshal(data, &summary); err != nil {
		t.Fatalf("unmarshal summary: %v", err)
	}
	return summary
}

// The git store must not assert the delta token scope over session rows that
// predate it. A CLI upgrade between two writes to one checkpoint — a review
// session attached to an older checkpoint, say — would otherwise stamp
// token_usage_version 2 while the surviving row's token_usage is still a
// session-cumulative total, and readers would skip the dedupe it needs.
func TestWriteStandardCheckpointEntries_KeepsLegacyTokenUsageVersion(t *testing.T) {
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	testutil.InitRepo(t, tmpDir)
	repo, err := git.PlainOpen(tmpDir)
	if err != nil {
		t.Fatalf("PlainOpen() error = %v", err)
	}
	store := NewGitStore(repo, DefaultV1Refs())

	checkpointID, err := id.Generate()
	if err != nil {
		t.Fatalf("id.Generate() error = %v", err)
	}
	basePath := checkpointID.Path() + "/"

	// A checkpoint written before token_usage_version existed: one session row,
	// no version on the summary.
	legacyMetadata := Metadata{
		CheckpointID: checkpointID,
		SessionID:    "session-legacy",
		Strategy:     "manual-commit",
		CLIVersion:   versioninfo.Version,
	}
	legacyMetadataJSON, err := jsonutil.MarshalIndentWithNewline(legacyMetadata, "", "  ")
	if err != nil {
		t.Fatalf("marshal legacy metadata: %v", err)
	}
	legacyMetadataHash, err := CreateBlobFromContent(repo, legacyMetadataJSON)
	if err != nil {
		t.Fatalf("CreateBlobFromContent(legacy metadata) error = %v", err)
	}
	sessionZeroPath := basePath + "0/" + paths.MetadataFileName

	legacySummary := CheckpointSummary{
		CheckpointID:     checkpointID,
		Strategy:         "manual-commit",
		CheckpointsCount: 1,
		Sessions:         []SessionFilePaths{{Metadata: "/" + sessionZeroPath}},
	}
	legacySummaryJSON, err := jsonutil.MarshalIndentWithNewline(legacySummary, "", "  ")
	if err != nil {
		t.Fatalf("marshal legacy summary: %v", err)
	}
	legacySummaryHash, err := CreateBlobFromContent(repo, legacySummaryJSON)
	if err != nil {
		t.Fatalf("CreateBlobFromContent(legacy summary) error = %v", err)
	}
	rootMetadataPath := basePath + paths.MetadataFileName

	entries := map[string]object.TreeEntry{
		sessionZeroPath:  {Name: sessionZeroPath, Mode: filemode.Regular, Hash: legacyMetadataHash},
		rootMetadataPath: {Name: rootMetadataPath, Mode: filemode.Regular, Hash: legacySummaryHash},
	}

	// A newer CLI attaches a second session to that same checkpoint.
	opts := WriteOptions{
		CheckpointID: checkpointID,
		SessionID:    "session-new",
		Strategy:     "manual-commit",
		Transcript:   redact.AlreadyRedacted([]byte("{\"type\":\"user\",\"message\":\"hi\"}\n")),
		Prompts:      []string{"hi"},
		AuthorName:   "T",
		AuthorEmail:  "t@example.com",
	}
	if err := store.writeStandardCheckpointEntries(context.Background(), opts, basePath, entries); err != nil {
		t.Fatalf("writeStandardCheckpointEntries() error = %v", err)
	}

	summary := readSummaryBlob(t, repo, entries[rootMetadataPath].Hash)
	if len(summary.Sessions) != 2 {
		t.Fatalf("want the legacy row plus the new one, got %d sessions", len(summary.Sessions))
	}
	if summary.TokenUsageVersion != 0 {
		t.Errorf("TokenUsageVersion = %d, want 0: the legacy row survives this write, so "+
			"the summary must not claim every row is a delta",
			summary.TokenUsageVersion)
	}
}
