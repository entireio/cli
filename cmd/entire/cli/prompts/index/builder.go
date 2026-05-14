package index

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/paths"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/filemode"
)

const MaxPromptLength = 2000

type Builder struct {
	repo  *git.Repository
	store *Store
}

func NewBuilder(repo *git.Repository, store *Store) *Builder {
	return &Builder{repo: repo, store: store}
}

func (b *Builder) AppendCheckpoint(_ context.Context, cpID id.CheckpointID, commitHash, commitMsg, branch, agent, model string, filesTouched []string, sessionIdx, turnIdx int, promptText string) error {
	truncated := false
	if len(promptText) > MaxPromptLength {
		promptText = promptText[:MaxPromptLength]
		truncated = true
	}

	entry := Entry{
		CheckpointID:    cpID.String(),
		SessionIndex:    sessionIdx,
		TurnIndex:       turnIdx,
		Kind:            "session",
		PromptText:      promptText,
		PromptTruncated: truncated,
		CommitHash:      commitHash,
		CommitMessage:   commitMsg,
		Branch:          branch,
		Agent:           agent,
		Model:           model,
		FilesTouched:    filesTouched,
		CreatedAt:       time.Now(),
	}

	if err := b.store.AppendEntries([]Entry{entry}); err != nil {
		return fmt.Errorf("appending entry: %w", err)
	}

	return nil
}

func (b *Builder) Build(_ context.Context, out io.Writer, progress func(done, total int)) error {
	if err := b.store.InitIndex(); err != nil {
		return fmt.Errorf("initializing index: %w", err)
	}

	ref, err := b.repo.Reference(plumbing.NewBranchReferenceName(paths.MetadataBranchName), true)
	if err != nil {
		return fmt.Errorf("getting metadata branch: %w", err)
	}

	commit, err := b.repo.CommitObject(ref.Hash())
	if err != nil {
		return fmt.Errorf("getting commit: %w", err)
	}

	tree, err := commit.Tree()
	if err != nil {
		return fmt.Errorf("getting tree: %w", err)
	}

	var cpIDs []id.CheckpointID
	if err := walkCheckpointShards(b.repo, tree.ID(), func(cpID id.CheckpointID, _ plumbing.Hash) error {
		cpIDs = append(cpIDs, cpID)
		return nil
	}); err != nil {
		return fmt.Errorf("walking checkpoint shards: %w", err)
	}

	total := len(cpIDs)
	allEntries := make([]Entry, 0)

	for i, cpID := range cpIDs {
		entries, err := b.loadCheckpoint(cpID)
		if err != nil {
			logging.Warn(nil, "skipping checkpoint due to load error", "checkpoint_id", cpID, "error", err)
			continue
		}
		allEntries = append(allEntries, entries...)
		if progress != nil {
			progress(i+1, total)
		}
	}

	if len(allEntries) > 0 {
		if err := b.store.AppendEntries(allEntries); err != nil {
			return fmt.Errorf("writing index entries: %w", err)
		}
	}

	fmt.Fprintf(out, "Indexed %d prompts from %d checkpoints.\n", len(allEntries), total)

	return nil
}

func isHex(s string) bool {
	for _, c := range s {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}

func walkCheckpointShards(repo *git.Repository, treeHash plumbing.Hash, fn func(id.CheckpointID, plumbing.Hash) error) error {
	rootTree, err := repo.TreeObject(treeHash)
	if err != nil {
		return fmt.Errorf("getting tree: %w", err)
	}

	for _, shardEntry := range rootTree.Entries {
		entryMode := shardEntry.Mode
		if entryMode != filemode.Dir || len(shardEntry.Name) != 2 || !isHex(shardEntry.Name) {
			continue
		}

		shardTree, err := repo.TreeObject(shardEntry.Hash)
		if err != nil {
			continue
		}

		for _, cpEntry := range shardTree.Entries {
			cpMode := cpEntry.Mode
			if cpMode != filemode.Dir || len(cpEntry.Name) != 10 || !isHex(cpEntry.Name) {
				continue
			}

			fullID := shardEntry.Name + cpEntry.Name
			cpID, err := id.NewCheckpointID(fullID)
			if err != nil {
				continue
			}

			if err := fn(cpID, cpEntry.Hash); err != nil {
				return fmt.Errorf("processing checkpoint: %w", err)
			}
		}
	}

	return nil
}

func (b *Builder) loadCheckpoint(cpID id.CheckpointID) ([]Entry, error) {
	shard := cpID.String()[:2]
	rest := cpID.String()[2:]
	cpDir := filepath.Join(shard, rest, "0")

	ref, err := b.repo.Reference(plumbing.NewBranchReferenceName(paths.MetadataBranchName), true)
	if err != nil {
		return nil, fmt.Errorf("getting metadata branch ref: %w", err)
	}

	commit, err := b.repo.CommitObject(ref.Hash())
	if err != nil {
		return nil, fmt.Errorf("getting commit object: %w", err)
	}

	tree, err := commit.Tree()
	if err != nil {
		return nil, fmt.Errorf("getting commit tree: %w", err)
	}

	cpTree, err := tree.Tree(cpDir)
	if err != nil {
		return nil, fmt.Errorf("getting checkpoint tree: %w", err)
	}

	metaFile, err := cpTree.File("metadata.json")
	if err != nil {
		return nil, fmt.Errorf("getting metadata file: %w", err)
	}

	metaContent, err := metaFile.Contents()
	if err != nil {
		return nil, fmt.Errorf("reading metadata: %w", err)
	}

	var metadata checkpoint.CheckpointSummary
	if err := json.Unmarshal([]byte(metaContent), &metadata); err != nil {
		return nil, fmt.Errorf("parsing metadata: %w", err)
	}

	promptFile, err := cpTree.File("prompt.txt")
	var allPrompts string
	if err == nil {
		allPrompts, _ = promptFile.Contents() //nolint:errcheck // best-effort
	}
	prompts := splitPrompts(allPrompts)

	entries := make([]Entry, 0)
	for i := range metadata.Sessions {
		sessionDir := filepath.Join(cpDir, strconv.Itoa(i))
		sessionTree, err := cpTree.Tree(sessionDir)
		if err != nil {
			continue
		}

		sessionMetaFile, err := sessionTree.File("metadata.json")
		if err != nil {
			continue
		}

		sessionMetaContent, err := sessionMetaFile.Contents()
		if err != nil {
			continue
		}

		var sessionMeta checkpoint.CommittedMetadata
		if err := json.Unmarshal([]byte(sessionMetaContent), &sessionMeta); err != nil {
			continue
		}

		prompt := ""
		if i < len(prompts) {
			prompt = prompts[i]
		}

		truncated := false
		if len(prompt) > MaxPromptLength {
			prompt = prompt[:MaxPromptLength]
			truncated = true
		}

		entry := Entry{
			CheckpointID:    cpID.String(),
			SessionIndex:    i,
			TurnIndex:       0,
			Kind:            "session",
			PromptText:      prompt,
			PromptTruncated: truncated,
			Agent:           string(sessionMeta.Agent),
			Model:           sessionMeta.Model,
			FilesTouched:    sessionMeta.FilesTouched,
		}

		entries = append(entries, entry)
	}

	return entries, nil
}

func splitPrompts(promptContent string) []string {
	if promptContent == "" {
		return nil
	}

	result := strings.Split(promptContent, "---\n\n")
	if len(result) == 0 {
		return []string{promptContent}
	}
	return result
}
