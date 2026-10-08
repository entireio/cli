package agentimport

import (
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
)

// ErrAnchorRepoConfig reports that the repository's object format could not be
// read, so no anchor could be checked. It is separated from the rejections
// below because it says nothing about the candidate: it fails identically for
// every one, and a caller looping over candidates must stop rather than
// mistake it for "this ref's object is missing" and end up blaming the refs.
var ErrAnchorRepoConfig = errors.New("read anchor repository config")

// ValidateAnchorCommit requires a full hexadecimal object ID in the
// repository's object format (40 hex characters under SHA-1, 64 under
// SHA-256) naming a commit in repo, and returns its canonical lowercase ID. It
// never interprets the input as a ref or revision expression, or peels a tag
// into a commit.
func ValidateAnchorCommit(repo *git.Repository, sha string) (string, error) {
	// Named separately from the length complaint below: Run is exported, so an
	// unset field is a caller that forgot one, and telling them about
	// hexadecimal width describes a typo they did not make.
	if sha == "" {
		return "", errors.New("import anchor is required: no commit ID was resolved for this import")
	}
	cfg, err := repo.Config()
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrAnchorRepoConfig, err)
	}
	if len(sha) != cfg.Extensions.ObjectFormat.HexSize() {
		return "", fmt.Errorf("import anchor must be a full %d-character hexadecimal commit ID", cfg.Extensions.ObjectFormat.HexSize())
	}
	if _, err := hex.DecodeString(sha); err != nil {
		return "", fmt.Errorf("import anchor must be hexadecimal: %w", err)
	}
	commit, err := repo.CommitObject(plumbing.NewHash(sha))
	if err != nil {
		return "", fmt.Errorf("import anchor %q does not resolve to a commit object: %w", sha, err)
	}
	return commit.Hash.String(), nil
}
