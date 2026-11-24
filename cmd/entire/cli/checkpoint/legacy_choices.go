package checkpoint

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/gitdir"
	"github.com/entireio/cli/cmd/entire/cli/internal/flock"
	"github.com/entireio/cli/cmd/entire/cli/jsonutil"
	"github.com/entireio/cli/cmd/entire/cli/osroot"
)

// The legacy-choices record lives in the git common dir beside the deleted
// list, so every worktree of a clone sees it. It records which v1 copy a
// person kept for a checkpoint whose copies diverged, so a later
// `entire doctor migrate-checkpoints` does not ask again. It is never pushed:
// a choice a remote could write would let that remote answer the question.
const (
	legacyChoicesFileName = "entire-legacy-checkpoint-choices.json"
	legacyChoicesLockName = "entire-legacy-checkpoint-choices.lock"
)

// legacyChoice is a person keeping the copy with subtree Kept over the copy
// with subtree Over. It applies only while the copies are exactly those two.
type legacyChoice struct {
	Kept plumbing.Hash
	Over plumbing.Hash
}

// legacyChoicesFile stores plain strings so one malformed entry is skipped on
// read rather than failing the whole record.
type legacyChoicesFile struct {
	Choices map[string]legacyChoiceEntry `json:"choices"`
}

type legacyChoiceEntry struct {
	Kept string `json:"kept"`
	Over string `json:"over"`
}

type legacyChoices struct {
	dir string
}

func legacyChoicesForRepo(repo *git.Repository) (*legacyChoices, error) {
	_, dir, err := repositoryDirs(repo)
	if err != nil {
		return nil, fmt.Errorf("resolve git common dir for legacy checkpoint choices: %w", err)
	}
	return &legacyChoices{dir: dir}, nil
}

// load returns the recorded choices. A missing file is no choices; an
// unreadable or corrupt one is an error naming the file.
func (c *legacyChoices) load() (map[id.CheckpointID]legacyChoice, error) {
	root, err := gitdir.OpenAt(c.dir)
	if err != nil {
		return nil, fmt.Errorf("open git common dir: %w", err)
	}
	return c.read(root)
}

// record saves choice for cid under the record's lock, replacing an earlier
// choice for it.
func (c *legacyChoices) record(cid id.CheckpointID, choice legacyChoice) error {
	root, err := gitdir.OpenAt(c.dir)
	if err != nil {
		return fmt.Errorf("open git common dir: %w", err)
	}
	release, err := flock.AcquireIn(root, legacyChoicesLockName)
	if err != nil {
		return fmt.Errorf("lock legacy checkpoint choices: %w", err)
	}
	defer release()

	choices, err := c.read(root)
	if err != nil {
		return err
	}
	choices[cid] = choice
	file := legacyChoicesFile{Choices: make(map[string]legacyChoiceEntry, len(choices))}
	for recorded, ch := range choices {
		file.Choices[recorded.String()] = legacyChoiceEntry{Kept: ch.Kept.String(), Over: ch.Over.String()}
	}
	data, err := json.Marshal(file)
	if err != nil {
		return fmt.Errorf("encode legacy checkpoint choices: %w", err)
	}
	if err := jsonutil.WriteFileAtomicIn(root, legacyChoicesFileName, data, 0o600); err != nil {
		return fmt.Errorf("write legacy checkpoint choices %s: %w", c.path(), err)
	}
	return nil
}

func (c *legacyChoices) path() string {
	return filepath.Join(c.dir, legacyChoicesFileName)
}

func (c *legacyChoices) read(root *os.Root) (map[id.CheckpointID]legacyChoice, error) {
	choices := make(map[id.CheckpointID]legacyChoice)
	data, err := osroot.ReadFileNoFollow(root, legacyChoicesFileName)
	if errors.Is(err, fs.ErrNotExist) {
		return choices, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read legacy checkpoint choices %s: %w", c.path(), err)
	}
	var file legacyChoicesFile
	if err := json.Unmarshal(data, &file); err != nil {
		return nil, fmt.Errorf("parse legacy checkpoint choices %s: %w", c.path(), err)
	}
	for raw, entry := range file.Choices {
		cid, err := id.NewCheckpointID(raw)
		if err != nil || !plumbing.IsHash(entry.Kept) || !plumbing.IsHash(entry.Over) {
			continue
		}
		choices[cid] = legacyChoice{Kept: plumbing.NewHash(entry.Kept), Over: plumbing.NewHash(entry.Over)}
	}
	return choices, nil
}
