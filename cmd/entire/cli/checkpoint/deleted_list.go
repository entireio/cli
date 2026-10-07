package checkpoint

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"slices"

	"github.com/go-git/go-git/v6"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/gitdir"
	"github.com/entireio/cli/cmd/entire/cli/internal/flock"
	"github.com/entireio/cli/cmd/entire/cli/jsonutil"
	"github.com/entireio/cli/cmd/entire/cli/osroot"
)

// The deleted-checkpoints list lives in the git common dir beside the push
// queue, so every worktree of a clone sees the same answer. It records the IDs
// `entire checkpoint delete` removed so local writers that would otherwise
// re-create one (an amend restoring its Entire-Checkpoint trailer, a v1→refs
// migration reading a stale tracking ref) can decline. It is local-only: a
// precursor to remote tombstones, which would also stop other clones.
const (
	deletedCheckpointsFileName = "entire-deleted-checkpoints.json"
	deletedCheckpointsLockName = "entire-deleted-checkpoints.lock"
)

// deletedCheckpointsFile stores plain strings so one malformed entry is
// skipped on read rather than failing id.CheckpointID's strict unmarshal for
// the whole list.
type deletedCheckpointsFile struct {
	CheckpointIDs []string `json:"checkpoint_ids"`
}

// DeletedCheckpointSet is a loaded deleted-checkpoints list.
type DeletedCheckpointSet map[id.CheckpointID]struct{}

// Contains reports whether cid was deleted from this clone.
func (s DeletedCheckpointSet) Contains(cid id.CheckpointID) bool {
	_, ok := s[cid]
	return ok
}

// DeletedCheckpoints is the flock-protected list of checkpoint IDs deleted
// from this clone.
type DeletedCheckpoints struct {
	dir string
}

// NewDeletedCheckpoints returns the list stored in gitCommonDir.
func NewDeletedCheckpoints(gitCommonDir string) *DeletedCheckpoints {
	return &DeletedCheckpoints{dir: gitCommonDir}
}

// DeletedCheckpointsForRepo resolves repo's git common dir and returns its list.
func DeletedCheckpointsForRepo(repo *git.Repository) (*DeletedCheckpoints, error) {
	_, dir, err := repositoryDirs(repo)
	if err != nil {
		return nil, fmt.Errorf("resolve git common dir for deleted checkpoints: %w", err)
	}
	return NewDeletedCheckpoints(dir), nil
}

// LoadDeletedCheckpoints reads the list for the repository ctx resolves to.
// Callers on hook paths treat an error as "nothing deleted": the list only
// narrows what a hook writes, so failing open keeps the hook working.
func LoadDeletedCheckpoints(ctx context.Context) (DeletedCheckpointSet, error) {
	dir, err := gitdir.CommonDir(ctx)
	if err != nil {
		return nil, fmt.Errorf("resolve git common dir for deleted checkpoints: %w", err)
	}
	return NewDeletedCheckpoints(dir).Load()
}

func loadDeletedForRepo(repo *git.Repository) (DeletedCheckpointSet, error) {
	list, err := DeletedCheckpointsForRepo(repo)
	if err != nil {
		return nil, err
	}
	return list.Load()
}

// Record adds cid to the list. Recording an ID already present is a no-op.
func (d *DeletedCheckpoints) Record(cid id.CheckpointID) error {
	if cid.Kind() == id.KindUnknown {
		return fmt.Errorf("record deleted checkpoint: invalid checkpoint ID %q", cid)
	}
	root, err := gitdir.OpenAt(d.dir)
	if err != nil {
		return fmt.Errorf("open git common dir: %w", err)
	}
	release, err := flock.AcquireIn(root, deletedCheckpointsLockName)
	if err != nil {
		return fmt.Errorf("lock deleted checkpoints list: %w", err)
	}
	defer release()

	ids, err := readDeletedCheckpoints(root)
	if err != nil {
		return err
	}
	if slices.Contains(ids, cid) {
		return nil
	}
	ids = append(ids, cid)
	raw := make([]string, len(ids))
	for i, recorded := range ids {
		raw[i] = recorded.String()
	}
	data, err := json.Marshal(deletedCheckpointsFile{CheckpointIDs: raw})
	if err != nil {
		return fmt.Errorf("encode deleted checkpoints list: %w", err)
	}
	if err := jsonutil.WriteFileAtomicIn(root, deletedCheckpointsFileName, data, 0o600); err != nil {
		return fmt.Errorf("write deleted checkpoints list: %w", err)
	}
	return nil
}

// Load returns the recorded IDs. A missing file is an empty list; an
// unreadable or corrupt one is an error, never an empty list, so a damaged
// file cannot silently let a deleted checkpoint come back.
func (d *DeletedCheckpoints) Load() (DeletedCheckpointSet, error) {
	root, err := gitdir.OpenAt(d.dir)
	if err != nil {
		return nil, fmt.Errorf("open git common dir: %w", err)
	}
	ids, err := readDeletedCheckpoints(root)
	if err != nil {
		return nil, err
	}
	set := make(DeletedCheckpointSet, len(ids))
	for _, cid := range ids {
		set[cid] = struct{}{}
	}
	return set, nil
}

func readDeletedCheckpoints(root *os.Root) ([]id.CheckpointID, error) {
	data, err := osroot.ReadFileNoFollow(root, deletedCheckpointsFileName)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read deleted checkpoints list: %w", err)
	}
	var file deletedCheckpointsFile
	if err := json.Unmarshal(data, &file); err != nil {
		return nil, fmt.Errorf("parse deleted checkpoints list: %w", err)
	}
	valid := make([]id.CheckpointID, 0, len(file.CheckpointIDs))
	for _, raw := range file.CheckpointIDs {
		if cid, err := id.NewCheckpointID(raw); err == nil {
			valid = append(valid, cid)
		}
	}
	return valid, nil
}
