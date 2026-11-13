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
// repository config (sparse checkout leaves one), and calling a method on the
// nil pointer panics (#3154). In that case the repository's own config file is
// parsed instead, so the result does not include config.worktree overrides.
// That suits the keys git records there at init, such as core.ignorecase and
// core.bare.
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
	defer f.Close()
	raw := format.New()
	if err := format.NewDecoder(f).Decode(raw); err != nil {
		return nil, fmt.Errorf("parse repository config: %w", err)
	}
	return raw, nil
}
