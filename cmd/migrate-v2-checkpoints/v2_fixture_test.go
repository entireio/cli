package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/jsonutil"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/versioninfo"
	"github.com/entireio/cli/redact"
	"github.com/stretchr/testify/require"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/filemode"
	"github.com/go-git/go-git/v6/plumbing/object"
)

type testV2CheckpointOptions struct {
	CheckpointID              id.CheckpointID
	SessionID                 string
	CreatedAt                 time.Time
	Strategy                  string
	Branch                    string
	Transcript                redact.RedactedBytes
	CompactTranscript         []byte
	Prompts                   []string
	FilesTouched              []string
	CheckpointsCount          int
	AuthorName                string
	AuthorEmail               string
	AuthorWhen                time.Time
	Agent                     types.AgentType
	Model                     string
	TurnID                    string
	IsTask                    bool
	ToolUseID                 string
	CheckpointTranscriptStart int
	CompactTranscriptStart    int
	TokenUsage                *agent.TokenUsage
	SessionMetrics            *checkpoint.SessionMetrics
	Summary                   *checkpoint.Summary
	InitialAttribution        *checkpoint.InitialAttribution
	PromptAttributions        json.RawMessage
	CombinedAttribution       *checkpoint.InitialAttribution
	Kind                      string
	ReviewSkills              []string
	ReviewPrompt              string
	HasReview                 bool
	InvestigateRunID          string
	InvestigateTopic          string
	HasInvestigation          bool
}

func writeTestV2Checkpoint(t *testing.T, repo *git.Repository, opts testV2CheckpointOptions) {
	t.Helper()

	if opts.CreatedAt.IsZero() {
		opts.CreatedAt = time.Date(2024, 5, 6, 7, 8, 9, 0, time.UTC)
	}
	if opts.Strategy == "" {
		opts.Strategy = testStrategy
	}
	if opts.Branch == "" {
		opts.Branch = testBranchName
	}
	if opts.AuthorName == "" {
		opts.AuthorName = testAuthorName
	}
	if opts.AuthorEmail == "" {
		opts.AuthorEmail = testAuthorEmail
	}
	if opts.AuthorWhen.IsZero() {
		opts.AuthorWhen = opts.CreatedAt
	}

	sessionIndex := writeTestV2MainCheckpoint(t, repo, opts)
	if opts.Transcript.Len() > 0 {
		writeTestV2FullTranscript(t, repo, opts.CheckpointID, sessionIndex, opts.Transcript.Bytes())
	}
}

func writeTestV2MainCheckpoint(t *testing.T, repo *git.Repository, opts testV2CheckpointOptions) int {
	t.Helper()

	refName := plumbing.ReferenceName(paths.V2MainRefName)
	parentHash, entries := readTestV2RefEntries(t, repo, refName)
	basePath := opts.CheckpointID.Path() + "/"

	summary := checkpoint.CheckpointSummary{
		CLIVersion:          versioninfo.Version,
		CheckpointID:        opts.CheckpointID,
		Strategy:            opts.Strategy,
		Branch:              opts.Branch,
		CheckpointsCount:    opts.CheckpointsCount,
		FilesTouched:        opts.FilesTouched,
		TokenUsage:          opts.TokenUsage,
		CombinedAttribution: opts.CombinedAttribution,
		HasReview:           opts.HasReview,
		HasInvestigation:    opts.HasInvestigation,
	}
	if entry, ok := entries[basePath+paths.MetadataFileName]; ok {
		existing := readTestJSONFromBlob[checkpoint.CheckpointSummary](t, repo, entry.Hash)
		summary = *existing
		if opts.HasReview {
			summary.HasReview = true
		}
	}

	sessionIndex := len(summary.Sessions)
	sessionPath := fmt.Sprintf("%s%d/", basePath, sessionIndex)
	filePaths := checkpoint.SessionFilePaths{
		Metadata: "/" + sessionPath + paths.MetadataFileName,
	}

	if len(opts.Prompts) > 0 {
		promptBlob, err := checkpoint.CreateBlobFromContent(repo, []byte(checkpoint.JoinPrompts(opts.Prompts)))
		require.NoError(t, err)
		entries[sessionPath+paths.PromptFileName] = object.TreeEntry{
			Name: sessionPath + paths.PromptFileName,
			Mode: filemode.Regular,
			Hash: promptBlob,
		}
		filePaths.Prompt = "/" + sessionPath + paths.PromptFileName
	}

	if len(opts.CompactTranscript) > 0 {
		compactBlob, err := checkpoint.CreateBlobFromContent(repo, opts.CompactTranscript)
		require.NoError(t, err)
		entries[sessionPath+paths.CompactTranscriptFileName] = object.TreeEntry{
			Name: sessionPath + paths.CompactTranscriptFileName,
			Mode: filemode.Regular,
			Hash: compactBlob,
		}
		filePaths.Transcript = "/" + sessionPath + paths.CompactTranscriptFileName

		compactHash := []byte(fmt.Sprintf("sha256:%x", sha256.Sum256(opts.CompactTranscript)))
		compactHashBlob, err := checkpoint.CreateBlobFromContent(repo, compactHash)
		require.NoError(t, err)
		entries[sessionPath+paths.CompactTranscriptHashFileName] = object.TreeEntry{
			Name: sessionPath + paths.CompactTranscriptHashFileName,
			Mode: filemode.Regular,
			Hash: compactHashBlob,
		}
		filePaths.ContentHash = "/" + sessionPath + paths.CompactTranscriptHashFileName
	}

	transcriptStart := opts.CheckpointTranscriptStart
	if opts.CompactTranscriptStart > 0 {
		transcriptStart = opts.CompactTranscriptStart
	}
	metadata := checkpoint.CommittedMetadata{
		CLIVersion:                versioninfo.Version,
		CheckpointID:              opts.CheckpointID,
		SessionID:                 opts.SessionID,
		Strategy:                  opts.Strategy,
		CreatedAt:                 opts.CreatedAt,
		Branch:                    opts.Branch,
		CheckpointsCount:          opts.CheckpointsCount,
		FilesTouched:              opts.FilesTouched,
		Agent:                     opts.Agent,
		Model:                     opts.Model,
		TurnID:                    opts.TurnID,
		IsTask:                    opts.IsTask,
		ToolUseID:                 opts.ToolUseID,
		CheckpointTranscriptStart: transcriptStart,
		TranscriptLinesAtStart:    transcriptStart,
		TokenUsage:                opts.TokenUsage,
		SessionMetrics:            opts.SessionMetrics,
		Summary:                   opts.Summary,
		InitialAttribution:        opts.InitialAttribution,
		PromptAttributions:        opts.PromptAttributions,
		Kind:                      opts.Kind,
		ReviewSkills:              opts.ReviewSkills,
		ReviewPrompt:              opts.ReviewPrompt,
		InvestigateRunID:          opts.InvestigateRunID,
		InvestigateTopic:          opts.InvestigateTopic,
	}
	metadataJSON, err := jsonutil.MarshalIndentWithNewline(metadata, "", "  ")
	require.NoError(t, err)
	metadataBlob, err := checkpoint.CreateBlobFromContent(repo, metadataJSON)
	require.NoError(t, err)
	entries[sessionPath+paths.MetadataFileName] = object.TreeEntry{
		Name: sessionPath + paths.MetadataFileName,
		Mode: filemode.Regular,
		Hash: metadataBlob,
	}

	summary.Sessions = append(summary.Sessions, filePaths)
	summaryJSON, err := jsonutil.MarshalIndentWithNewline(summary, "", "  ")
	require.NoError(t, err)
	summaryBlob, err := checkpoint.CreateBlobFromContent(repo, summaryJSON)
	require.NoError(t, err)
	entries[basePath+paths.MetadataFileName] = object.TreeEntry{
		Name: basePath + paths.MetadataFileName,
		Mode: filemode.Regular,
		Hash: summaryBlob,
	}

	writeTestV2RefEntriesWithAuthor(t, repo, refName, parentHash, entries, "test v2 main fixture", object.Signature{
		Name:  opts.AuthorName,
		Email: opts.AuthorEmail,
		When:  opts.AuthorWhen,
	})
	return sessionIndex
}

func writeTestV2FullTranscript(t *testing.T, repo *git.Repository, cpID id.CheckpointID, sessionIndex int, transcript []byte) {
	t.Helper()

	sessionPath := fmt.Sprintf("%s/%d/", cpID.Path(), sessionIndex)
	contentHash := []byte(fmt.Sprintf("sha256:%x", sha256.Sum256(transcript)))
	writeTestV2FullSessionFiles(t, repo, map[string][]byte{
		sessionPath + paths.V2RawTranscriptFileName:     transcript,
		sessionPath + paths.V2RawTranscriptHashFileName: contentHash,
	})
}

func writeTestV2FullSessionFiles(t *testing.T, repo *git.Repository, files map[string][]byte) {
	t.Helper()

	refName := plumbing.ReferenceName(paths.V2FullCurrentRefName)
	parentHash, entries := readTestV2RefEntries(t, repo, refName)
	for path, content := range files {
		blobHash, err := checkpoint.CreateBlobFromContent(repo, content)
		require.NoError(t, err)
		entries[path] = object.TreeEntry{Name: path, Mode: filemode.Regular, Hash: blobHash}
	}
	writeTestV2RefEntries(t, repo, refName, parentHash, entries, "test v2 full fixture")
}

func readTestV2RefEntries(t *testing.T, repo *git.Repository, refName plumbing.ReferenceName) (plumbing.Hash, map[string]object.TreeEntry) {
	t.Helper()

	entries := make(map[string]object.TreeEntry)
	ref, err := repo.Reference(refName, true)
	if errors.Is(err, plumbing.ErrReferenceNotFound) {
		return plumbing.ZeroHash, entries
	}
	require.NoError(t, err)
	commit, err := repo.CommitObject(ref.Hash())
	require.NoError(t, err)
	tree, err := commit.Tree()
	require.NoError(t, err)

	files := tree.Files()
	err = files.ForEach(func(file *object.File) error {
		entries[file.Name] = object.TreeEntry{Name: file.Name, Mode: file.Mode, Hash: file.Hash}
		return nil
	})
	require.NoError(t, err)
	return ref.Hash(), entries
}

func writeTestV2RefEntries(t *testing.T, repo *git.Repository, refName plumbing.ReferenceName, parentHash plumbing.Hash, entries map[string]object.TreeEntry, message string) {
	t.Helper()

	authorName, authorEmail := checkpoint.GetGitAuthorFromRepo(repo)
	writeTestV2RefEntriesWithAuthor(t, repo, refName, parentHash, entries, message, object.Signature{
		Name:  authorName,
		Email: authorEmail,
		When:  time.Now(),
	})
}

func writeTestV2RefEntriesWithAuthor(t *testing.T, repo *git.Repository, refName plumbing.ReferenceName, parentHash plumbing.Hash, entries map[string]object.TreeEntry, message string, author object.Signature) {
	t.Helper()

	treeHash, err := checkpoint.BuildTreeFromEntries(context.Background(), repo, entries)
	require.NoError(t, err)
	commit := &object.Commit{
		TreeHash:  treeHash,
		Author:    author,
		Committer: author,
		Message:   message,
	}
	if parentHash != plumbing.ZeroHash {
		commit.ParentHashes = []plumbing.Hash{parentHash}
	}
	encoded := repo.Storer.NewEncodedObject()
	require.NoError(t, commit.Encode(encoded))
	commitHash, err := repo.Storer.SetEncodedObject(encoded)
	require.NoError(t, err)
	require.NoError(t, repo.Storer.SetReference(plumbing.NewHashReference(refName, commitHash)))
}

func readTestJSONFromBlob[T any](t *testing.T, repo *git.Repository, hash plumbing.Hash) *T {
	t.Helper()

	blob, err := repo.BlobObject(hash)
	require.NoError(t, err)
	reader, err := blob.Reader()
	require.NoError(t, err)
	defer func() {
		require.NoError(t, reader.Close())
	}()
	content, err := io.ReadAll(reader)
	require.NoError(t, err)

	var result T
	require.NoError(t, json.Unmarshal(content, &result))
	return &result
}
