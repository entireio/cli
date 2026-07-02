package ticket

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/entireio/cli/cmd/entire/cli/session"
)

const (
	// ticketStateDirName is the directory (within the git common dir) that
	// holds ticket-link state, shared across worktrees like entire-sessions.
	ticketStateDirName = "entire-tickets"
	// linksFileName holds the branch → link mapping.
	linksFileName = "links.json"
)

// Link records which ticket a branch is associated with. It is stored per
// branch so a ticket can be linked before, during, or after the work — the
// session capture happens continuously regardless.
type Link struct {
	Platform string `json:"platform"`
	ID       string `json:"id"`
	// Snapshot is the last-seen ticket state, refreshed on each fetch so drift
	// can be detected. Nil until the ticket has been fetched at least once.
	Snapshot *Snapshot `json:"snapshot,omitempty"`
}

// linkStore persists branch → Link mappings as a single JSON file in the git
// common dir.
type linkStore struct {
	dir string
}

// newLinkStore resolves the ticket-state directory from the git common dir.
func newLinkStore(ctx context.Context) (*linkStore, error) {
	commonDir, err := session.GetGitCommonDir(ctx)
	if err != nil {
		return nil, fmt.Errorf("resolve git common dir: %w", err)
	}
	return &linkStore{dir: filepath.Join(commonDir, ticketStateDirName)}, nil
}

// newLinkStoreWithDir builds a store rooted at an explicit directory, for tests.
func newLinkStoreWithDir(dir string) *linkStore {
	return &linkStore{dir: dir}
}

func (s *linkStore) path() string {
	return filepath.Join(s.dir, linksFileName)
}

// all returns the full branch → Link map, or an empty map when no state exists.
func (s *linkStore) all() (map[string]Link, error) {
	data, err := os.ReadFile(s.path())
	if errors.Is(err, os.ErrNotExist) {
		return map[string]Link{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read ticket links: %w", err)
	}
	links := map[string]Link{}
	if len(data) == 0 {
		return links, nil
	}
	if err := json.Unmarshal(data, &links); err != nil {
		return nil, fmt.Errorf("parse ticket links: %w", err)
	}
	return links, nil
}

// get returns the link for branch, if any.
func (s *linkStore) get(branch string) (Link, bool, error) {
	links, err := s.all()
	if err != nil {
		return Link{}, false, err
	}
	l, ok := links[branch]
	return l, ok, nil
}

// set records the link for branch, replacing any existing one.
func (s *linkStore) set(branch string, l Link) error {
	links, err := s.all()
	if err != nil {
		return err
	}
	links[branch] = l
	return s.write(links)
}

// del removes the link for branch, reporting whether one existed.
func (s *linkStore) del(branch string) (bool, error) {
	links, err := s.all()
	if err != nil {
		return false, err
	}
	if _, ok := links[branch]; !ok {
		return false, nil
	}
	delete(links, branch)
	return true, s.write(links)
}

// write atomically persists the full map (temp file + rename).
func (s *linkStore) write(links map[string]Link) error {
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return fmt.Errorf("create ticket state dir: %w", err)
	}
	data, err := json.MarshalIndent(links, "", "  ")
	if err != nil {
		return fmt.Errorf("encode ticket links: %w", err)
	}
	tmp, err := os.CreateTemp(s.dir, "links-*.json.tmp")
	if err != nil {
		return fmt.Errorf("create temp ticket links: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temp ticket links: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp ticket links: %w", err)
	}
	if err := os.Rename(tmpName, s.path()); err != nil {
		return fmt.Errorf("commit ticket links: %w", err)
	}
	return nil
}
