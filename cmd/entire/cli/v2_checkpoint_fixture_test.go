package cli

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
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

type v2CheckpointFixtureOptions struct {
	CheckpointID              id.CheckpointID
	SessionID                 string
	Strategy                  string
	Branch                    string
	Transcript                redact.RedactedBytes
	Prompts                   []string
	FilesTouched              []string
	CheckpointsCount          int
	CreatedAt                 time.Time
	Agent                     types.AgentType
	Model                     string
	TurnID                    string
	TokenUsage                *agent.TokenUsage
	SessionMetrics            *checkpoint.SessionMetrics
	Summary                   *checkpoint.Summary
	InitialAttribution        *checkpoint.InitialAttribution
	PromptAttributions        json.RawMessage
	CompactTranscript         []byte
	CheckpointTranscriptStart int
	Kind                      string
	ReviewSkills              []string
	ReviewPrompt              string
	HasReview                 bool
}

func writeV2CheckpointFixture(t *testing.T, repo *git.Repository, opts v2CheckpointFixtureOptions) {
	t.Helper()

	sessionIndex := writeV2MainCheckpointFixture(t, repo, opts)
	if len(opts.Transcript.Bytes()) > 0 {
		writeV2FullTranscriptFixture(t, repo, opts.CheckpointID, sessionIndex, opts.Transcript.Bytes())
	}
}

func writeV2MainCheckpointFixture(t *testing.T, repo *git.Repository, opts v2CheckpointFixtureOptions) int {
	t.Helper()

	if opts.CreatedAt.IsZero() {
		opts.CreatedAt = time.Now()
	}

	refName := plumbing.ReferenceName(paths.V2MainRefName)
	parentHash, entries := readV2FixtureRefEntries(t, repo, refName)
	basePath := opts.CheckpointID.Path() + "/"

	summary := checkpoint.CheckpointSummary{
		CLIVersion:       versioninfo.Version,
		CheckpointID:     opts.CheckpointID,
		Strategy:         opts.Strategy,
		Branch:           opts.Branch,
		CheckpointsCount: opts.CheckpointsCount,
		FilesTouched:     opts.FilesTouched,
		TokenUsage:       opts.TokenUsage,
		HasReview:        opts.HasReview,
	}
	if entry, ok := entries[basePath+paths.MetadataFileName]; ok {
		readV2CheckpointSummaryFixture(t, repo, entry.Hash, &summary)
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
		CheckpointTranscriptStart: opts.CheckpointTranscriptStart,
		TranscriptLinesAtStart:    opts.CheckpointTranscriptStart,
		TokenUsage:                opts.TokenUsage,
		SessionMetrics:            opts.SessionMetrics,
		Summary:                   opts.Summary,
		InitialAttribution:        opts.InitialAttribution,
		PromptAttributions:        opts.PromptAttributions,
		Kind:                      opts.Kind,
		ReviewSkills:              opts.ReviewSkills,
		ReviewPrompt:              opts.ReviewPrompt,
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

	writeV2FixtureRefEntries(t, repo, refName, parentHash, entries, "test v2 main fixture")
	return sessionIndex
}

func writeV2FullTranscriptFixture(t *testing.T, repo *git.Repository, cpID id.CheckpointID, sessionIndex int, transcript []byte) {
	t.Helper()

	sessionPath := fmt.Sprintf("%s/%d/", cpID.Path(), sessionIndex)
	contentHash := []byte(fmt.Sprintf("sha256:%x", sha256.Sum256(transcript)))
	writeV2FullSessionFilesFixture(t, repo, map[string][]byte{
		sessionPath + paths.V2RawTranscriptFileName:     transcript,
		sessionPath + paths.V2RawTranscriptHashFileName: contentHash,
	})
}

func writeV2FullSessionFilesFixture(t *testing.T, repo *git.Repository, files map[string][]byte) {
	t.Helper()

	refName := plumbing.ReferenceName(paths.V2FullCurrentRefName)
	parentHash, entries := readV2FixtureRefEntries(t, repo, refName)
	for path, content := range files {
		blobHash, err := checkpoint.CreateBlobFromContent(repo, content)
		require.NoError(t, err)
		entries[path] = object.TreeEntry{Name: path, Mode: filemode.Regular, Hash: blobHash}
	}
	writeV2FixtureRefEntries(t, repo, refName, parentHash, entries, "test v2 full fixture")
}

func readV2FixtureRefEntries(t *testing.T, repo *git.Repository, refName plumbing.ReferenceName) (plumbing.Hash, map[string]object.TreeEntry) {
	t.Helper()

	entries := make(map[string]object.TreeEntry)
	ref, err := repo.Reference(refName, true)
	if err != nil {
		return plumbing.ZeroHash, entries
	}
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

func writeV2FixtureRefEntries(t *testing.T, repo *git.Repository, refName plumbing.ReferenceName, parentHash plumbing.Hash, entries map[string]object.TreeEntry, message string) {
	t.Helper()

	treeHash, err := checkpoint.BuildTreeFromEntries(context.Background(), repo, entries)
	require.NoError(t, err)
	authorName, authorEmail := checkpoint.GetGitAuthorFromRepo(repo)
	commitHash, err := checkpoint.CreateCommit(context.Background(), repo, treeHash, parentHash, message, authorName, authorEmail)
	require.NoError(t, err)
	require.NoError(t, repo.Storer.SetReference(plumbing.NewHashReference(refName, commitHash)))
}

func readV2CheckpointSummaryFixture(t *testing.T, repo *git.Repository, hash plumbing.Hash, summary *checkpoint.CheckpointSummary) {
	t.Helper()

	blob, err := repo.BlobObject(hash)
	require.NoError(t, err)
	reader, err := blob.Reader()
	require.NoError(t, err)
	defer reader.Close()

	require.NoError(t, json.NewDecoder(reader).Decode(summary))
}
