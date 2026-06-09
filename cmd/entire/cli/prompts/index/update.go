package index

import (
	"context"
	"path/filepath"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/paths"
)

func UpdateIndexForCheckpoint(_ context.Context, repoRoot string, cpID id.CheckpointID, commitHash, commitMsg, branch, agent, model string, filesTouched []string, sessionIdx, turnIdx int, promptText string) error {
	entireDir := filepath.Join(repoRoot, paths.EntireDir)
	indexDir := filepath.Join(entireDir, IndexDirName)

	store := &Store{
		repoRoot:  repoRoot,
		indexPath: filepath.Join(indexDir, IndexFileName),
		lockPath:  filepath.Join(indexDir, LockFileName),
	}

	builder := &Builder{store: store}

	return builder.AppendCheckpoint(nil, cpID, commitHash, commitMsg, branch, agent, model, filesTouched, sessionIdx, turnIdx, promptText)
}
