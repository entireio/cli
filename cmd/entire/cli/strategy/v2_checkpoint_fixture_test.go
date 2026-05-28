package strategy

import (
	"context"
	"crypto/sha256"
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
	CheckpointID     id.CheckpointID
	SessionID        string
	Strategy         string
	Branch           string
	Transcript       redact.RedactedBytes
	Prompts          []string
	FilesTouched     []string
	CheckpointsCount int
	CreatedAt        time.Time
	Agent            types.AgentType
	Model            string
	TokenUsage       *agent.TokenUsage
	HasReview        bool
}

func writeV2CheckpointFixture(t *testing.T, repo *git.Repository, opts v2CheckpointFixtureOptions) {
	t.Helper()

	if opts.CreatedAt.IsZero() {
		opts.CreatedAt = time.Now()
	}

	basePath := opts.CheckpointID.Path() + "/"
	sessionPath := basePath + "0/"
	filePaths := checkpoint.SessionFilePaths{
		Metadata: "/" + sessionPath + paths.MetadataFileName,
	}

	entries := map[string]object.TreeEntry{}
	if len(opts.Prompts) > 0 {
		promptBlob, err := checkpoint.CreateBlobFromContent(repo, []byte(checkpoint.JoinPrompts(opts.Prompts)))
		require.NoError(t, err)
		entries[sessionPath+paths.PromptFileName] = object.TreeEntry{Name: sessionPath + paths.PromptFileName, Mode: filemode.Regular, Hash: promptBlob}
		filePaths.Prompt = "/" + sessionPath + paths.PromptFileName
	}

	metadata := checkpoint.CommittedMetadata{
		CLIVersion:       versioninfo.Version,
		CheckpointID:     opts.CheckpointID,
		SessionID:        opts.SessionID,
		Strategy:         opts.Strategy,
		CreatedAt:        opts.CreatedAt,
		Branch:           opts.Branch,
		CheckpointsCount: opts.CheckpointsCount,
		FilesTouched:     opts.FilesTouched,
		Agent:            opts.Agent,
		Model:            opts.Model,
	}
	metadataJSON, err := jsonutil.MarshalIndentWithNewline(metadata, "", "  ")
	require.NoError(t, err)
	metadataBlob, err := checkpoint.CreateBlobFromContent(repo, metadataJSON)
	require.NoError(t, err)
	entries[sessionPath+paths.MetadataFileName] = object.TreeEntry{Name: sessionPath + paths.MetadataFileName, Mode: filemode.Regular, Hash: metadataBlob}

	summary := checkpoint.CheckpointSummary{
		CLIVersion:       versioninfo.Version,
		CheckpointID:     opts.CheckpointID,
		Strategy:         opts.Strategy,
		Branch:           opts.Branch,
		CheckpointsCount: opts.CheckpointsCount,
		FilesTouched:     opts.FilesTouched,
		Sessions:         []checkpoint.SessionFilePaths{filePaths},
		TokenUsage:       opts.TokenUsage,
		HasReview:        opts.HasReview,
	}
	summaryJSON, err := jsonutil.MarshalIndentWithNewline(summary, "", "  ")
	require.NoError(t, err)
	summaryBlob, err := checkpoint.CreateBlobFromContent(repo, summaryJSON)
	require.NoError(t, err)
	entries[basePath+paths.MetadataFileName] = object.TreeEntry{Name: basePath + paths.MetadataFileName, Mode: filemode.Regular, Hash: summaryBlob}

	writeV2FixtureEntries(t, repo, plumbing.ReferenceName(paths.V2MainRefName), entries, "test v2 main fixture")

	if len(opts.Transcript.Bytes()) > 0 {
		writeV2RawTranscriptFixture(t, repo, opts)
	}
}

func writeV2RawTranscriptFixture(t *testing.T, repo *git.Repository, opts v2CheckpointFixtureOptions) {
	t.Helper()

	sessionPath := opts.CheckpointID.Path() + "/0/"
	contentHash := []byte(fmt.Sprintf("sha256:%x", sha256.Sum256(opts.Transcript.Bytes())))
	entries := map[string]object.TreeEntry{}
	for path, content := range map[string][]byte{
		sessionPath + paths.V2RawTranscriptFileName:     opts.Transcript.Bytes(),
		sessionPath + paths.V2RawTranscriptHashFileName: contentHash,
	} {
		blobHash, err := checkpoint.CreateBlobFromContent(repo, content)
		require.NoError(t, err)
		entries[path] = object.TreeEntry{Name: path, Mode: filemode.Regular, Hash: blobHash}
	}
	writeV2FixtureEntries(t, repo, plumbing.ReferenceName(paths.V2FullCurrentRefName), entries, "test v2 full fixture")
}

func writeV2FixtureEntries(t *testing.T, repo *git.Repository, refName plumbing.ReferenceName, entries map[string]object.TreeEntry, message string) {
	t.Helper()

	treeHash, err := checkpoint.BuildTreeFromEntries(context.Background(), repo, entries)
	require.NoError(t, err)
	authorName, authorEmail := checkpoint.GetGitAuthorFromRepo(repo)
	commitHash, err := checkpoint.CreateCommit(context.Background(), repo, treeHash, plumbing.ZeroHash, message, authorName, authorEmail)
	require.NoError(t, err)
	require.NoError(t, repo.Storer.SetReference(plumbing.NewHashReference(refName, commitHash)))
}
