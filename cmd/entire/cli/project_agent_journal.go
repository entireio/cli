package cli

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/entireio/cli/cmd/entire/cli/auth"
	"github.com/entireio/cli/cmd/entire/cli/internal/flock"
	"github.com/entireio/cli/cmd/entire/cli/jsonutil"
	"github.com/entireio/cli/cmd/entire/cli/osroot"
	"github.com/entireio/cli/internal/entireclient/userdirs"
)

type projectAgentJournal struct {
	root   *os.Root
	name   string
	unlock func()
}

func openProjectAgentJournal(ctx context.Context, t *projectAgentTarget) (*projectAgentJournal, error) {
	identity, err := auth.LocalIdentityCacheKey()
	if err != nil {
		return nil, fmt.Errorf("resolve worker identity: %w", err)
	}
	root, err := userdirs.ConfigRoot()
	if err != nil {
		return nil, fmt.Errorf("open worker state: %w", err)
	}
	if err := osroot.MkdirAllNoSymlink(root, "agent-serve", 0o700); err != nil {
		return nil, fmt.Errorf("create worker state directory: %w", err)
	}
	key := fmt.Sprintf("agent-serve/%x", sha256.Sum256([]byte(identity+"\n"+t.client.BaseURL()+"\n"+t.projectID)))
	unlock, err := flock.AcquireContextIn(ctx, root, key+".lock")
	if err != nil {
		return nil, fmt.Errorf("lock worker state: %w", err)
	}
	return &projectAgentJournal{root: root, name: key + ".json", unlock: unlock}, nil
}

func (j *projectAgentJournal) save(s *projectAgentSubmission) error {
	if s == nil {
		err := osroot.RemoveNoSymlinks(j.root, j.name)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove submitted result: %w", err)
		}
		return nil
	}
	b, err := json.Marshal(s)
	if err != nil {
		return fmt.Errorf("encode pending result: %w", err)
	}
	if err := jsonutil.WriteFileAtomicIn(j.root, j.name, b, 0o600); err != nil {
		return fmt.Errorf("save pending result: %w", err)
	}
	return nil
}

func (j *projectAgentJournal) load() (*projectAgentSubmission, error) {
	b, err := osroot.ReadFileNoFollow(j.root, j.name)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil //nolint:nilnil // No file means no pending submission.
	}
	if err != nil {
		return nil, fmt.Errorf("read pending result: %w", err)
	}
	var s projectAgentSubmission
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("decode pending result: %w", err)
	}
	return &s, nil
}
