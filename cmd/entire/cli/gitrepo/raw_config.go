package gitrepo

import (
	"errors"
	"fmt"

	"github.com/go-git/go-billy/v6"
	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/config"
	format "github.com/go-git/go-git/v6/plumbing/format/config"
)

// RawConfig returns the raw sections behind cfg, which repo.Config returned.
//
// Read raw options through this, never through cfg.Raw directly: go-git leaves
// Raw nil when extensions.worktreeConfig merges a config.worktree into the
// repository config, and a method call on the nil pointer panics. In that case
// the repository's shared config file is parsed instead, so the result leaves
// out config.worktree overrides. That suits keys git records there when it
// creates the repository, such as core.ignorecase and core.bare.
func RawConfig(repo *git.Repository, cfg *config.Config) (*format.Config, error) {
	if cfg != nil && cfg.Raw != nil {
		return cfg.Raw, nil
	}
	fsStorer, ok := repo.Storer.(interface{ Filesystem() billy.Filesystem })
	if !ok {
		return nil, errors.New("repository config has no raw sections and its storage has no filesystem")
	}
	f, err := fsStorer.Filesystem().Open("config")
	if err != nil {
		return nil, fmt.Errorf("open repository config: %w", err)
	}
	defer func() { _ = f.Close() }()
	raw := format.New()
	if err := format.NewDecoder(f).Decode(raw); err != nil {
		return nil, fmt.Errorf("parse repository config: %w", err)
	}
	return raw, nil
}
