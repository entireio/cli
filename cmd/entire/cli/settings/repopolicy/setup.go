package repopolicy

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/entireio/cli/cmd/entire/cli/jsonutil"
)

const setupFileName = "setup.json"

func setupPath(repository Repository) string {
	return filepath.Join(registryDir(repository), setupFileName)
}

// ReadSetupRecord reads and verifies the current worktree's component record.
func ReadSetupRecord(repository Repository) (record SetupRecord, found bool, err error) {
	if err := validateRepository(repository); err != nil {
		return SetupRecord{}, false, err
	}
	data, err := os.ReadFile(setupPath(repository))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return SetupRecord{}, false, nil
		}
		return SetupRecord{}, false, fmt.Errorf("reading setup record: %w", err)
	}
	if err := decodeStrict(data, &record); err != nil {
		return SetupRecord{}, false, fmt.Errorf("parsing setup record: %w", err)
	}
	if record.Version != recordVersion {
		return SetupRecord{}, false, fmt.Errorf("unsupported setup record version %d", record.Version)
	}
	if record.CanonicalWorktree != repository.WorktreeRoot || record.CanonicalGitCommon != repository.GitCommonDir {
		return SetupRecord{}, false, errors.New("setup record repository identity mismatch")
	}
	if record.GitHooksSpec < 0 || record.PrimaryRefSpec < 0 {
		return SetupRecord{}, false, errors.New("invalid setup component version")
	}
	return record, true, nil
}

// WriteSetupRecord atomically publishes a verified component record.
func WriteSetupRecord(repository Repository, record SetupRecord) error {
	if err := validateRepository(repository); err != nil {
		return err
	}
	if existing, found, err := ReadSetupRecord(repository); err != nil {
		return err
	} else if found && (existing.CanonicalWorktree != repository.WorktreeRoot || existing.CanonicalGitCommon != repository.GitCommonDir) {
		return errors.New("setup record repository identity mismatch")
	}
	if err := ensureRegistryDir(repository); err != nil {
		return err
	}
	record.Version = recordVersion
	record.CanonicalWorktree = repository.WorktreeRoot
	record.CanonicalGitCommon = repository.GitCommonDir
	data, err := jsonutil.MarshalIndentWithNewline(record, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding setup record: %w", err)
	}
	if err := jsonutil.WriteFileAtomic(setupPath(repository), data, 0o600); err != nil {
		return fmt.Errorf("writing setup record: %w", err)
	}
	return nil
}

// RemoveWorktreeRuntime removes only current-worktree runtime and route/setup
// records. The activation record is deliberately retained as a tombstone.
func RemoveWorktreeRuntime(repository Repository) error {
	if err := validateRepository(repository); err != nil {
		return err
	}
	if _, _, err := ReadRuntimeRoute(repository); err != nil {
		return err
	}
	if _, _, err := ReadSetupRecord(repository); err != nil {
		return err
	}
	dir := registryDir(repository)
	for _, name := range []string{routeFileName, routeFileName + ".lock", setupFileName, "metadata", "logs", "tmp"} {
		if err := os.RemoveAll(filepath.Join(dir, name)); err != nil {
			return fmt.Errorf("removing worktree runtime %s: %w", name, err)
		}
	}
	if err := jsonutil.SyncDir(dir); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("syncing worktree registry: %w", err)
	}
	return nil
}
