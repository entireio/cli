package repopolicy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/internal/gitpath"
)

const (
	projectSettingsRelative = ".entire/settings.json"
	localSettingsRelative   = ".entire/settings.local.json"
)

var errUnverifiedPath = errors.New("path changed while provenance was verified")

// SettingsOwnership records whether Git proved ownership of safely read local
// settings. Unknown is distinct from an unsafe path: local policy may retain
// ordinary preferences, but executable and global-only consumers must require
// SettingsOwnershipUntracked.
type SettingsOwnership uint8

const (
	SettingsOwnershipUnknown SettingsOwnership = iota
	SettingsOwnershipUntracked
	SettingsOwnershipTracked
)

// SettingsProvenance contains settings bytes read from the same regular files
// whose worktree-rooted paths and Git ownership were verified. Consumers must
// use these bytes rather than reopening ProjectPath or LocalPath.
type SettingsProvenance struct {
	ProjectPath       string
	ProjectPathSafe   bool
	ProjectVerified   bool
	ProjectData       []byte
	LocalPath         string
	LocalPathSafe     bool
	LocalOwnership    SettingsOwnership
	LocalOPFOwnership SettingsOwnership
	LocalData         []byte
}

type settingsProvenanceOptions struct {
	pathTracked       func(context.Context, string, string) (bool, error)
	pathTrackedInHEAD func(context.Context, string, string) (bool, error)
}

func defaultSettingsProvenanceOptions() settingsProvenanceOptions {
	return settingsProvenanceOptions{
		pathTracked:       gitPathTracked,
		pathTrackedInHEAD: gitPathTrackedInHEAD,
	}
}

type verifiedReadHooks struct {
	afterPrecheck func()
	afterOpen     func()
}

// VerifySettingsProvenance binds validation and consumption to files opened
// beneath the canonical worktree root. Unsafe, missing, tracked, or changing
// paths are reported as unverified rather than followed.
func VerifySettingsProvenance(ctx context.Context, repository Repository) (SettingsProvenance, error) {
	return verifySettingsProvenanceWithOptions(ctx, repository, defaultSettingsProvenanceOptions())
}

func verifySettingsProvenanceWithOptions(
	ctx context.Context,
	repository Repository,
	options settingsProvenanceOptions,
) (SettingsProvenance, error) {
	if err := validateRepository(repository); err != nil {
		return SettingsProvenance{}, err
	}
	provenance := SettingsProvenance{
		ProjectPath: filepath.Join(repository.WorktreeRoot, filepath.FromSlash(projectSettingsRelative)),
		LocalPath:   filepath.Join(repository.WorktreeRoot, filepath.FromSlash(localSettingsRelative)),
	}
	root, err := os.OpenRoot(repository.WorktreeRoot)
	if err != nil {
		return provenance, fmt.Errorf("opening worktree root: %w", err)
	}
	defer root.Close()

	projectData, projectErr := readVerifiedRegular(root, filepath.FromSlash(projectSettingsRelative), nil, verifiedReadHooks{})
	if projectErr == nil {
		provenance.ProjectPathSafe = true
		provenance.ProjectVerified = true
		provenance.ProjectData = projectData
	}

	localData, localErr := readVerifiedRegular(root, filepath.FromSlash(localSettingsRelative), func(data []byte) error {
		provenance.LocalOwnership = querySettingsOwnership(
			ctx,
			repository.WorktreeRoot,
			localSettingsRelative,
			options.pathTracked,
		)
		if provenance.LocalOwnership != SettingsOwnershipUntracked {
			provenance.LocalOPFOwnership = provenance.LocalOwnership
			return nil
		}
		provenance.LocalOPFOwnership = SettingsOwnershipUntracked
		if settingsDataHasOPFCommand(data) {
			provenance.LocalOPFOwnership = querySettingsOwnership(
				ctx,
				repository.WorktreeRoot,
				localSettingsRelative,
				options.pathTrackedInHEAD,
			)
		}
		return nil
	}, verifiedReadHooks{})
	if localErr == nil {
		provenance.LocalPathSafe = true
		provenance.LocalData = localData
	}
	return provenance, nil
}

func querySettingsOwnership(
	ctx context.Context,
	root string,
	rel string,
	query func(context.Context, string, string) (bool, error),
) SettingsOwnership {
	tracked, err := query(ctx, root, rel)
	if err != nil {
		return SettingsOwnershipUnknown
	}
	if tracked {
		return SettingsOwnershipTracked
	}
	return SettingsOwnershipUntracked
}

func readVerifiedRegular(root *os.Root, rel string, verify func([]byte) error, hooks verifiedReadHooks) ([]byte, error) {
	return consumeVerifiedRegular(root, rel, verify, hooks, true)
}

func verifyRegular(root *os.Root, rel string, hooks verifiedReadHooks) error {
	_, err := consumeVerifiedRegular(root, rel, nil, hooks, false)
	return err
}

func consumeVerifiedRegular(root *os.Root, rel string, verify func([]byte) error, hooks verifiedReadHooks, readContent bool) ([]byte, error) {
	before, err := lstatRootPath(root, rel, false)
	if err != nil {
		return nil, err
	}
	if hooks.afterPrecheck != nil {
		hooks.afterPrecheck()
	}
	file, err := root.Open(rel)
	if err != nil {
		return nil, fmt.Errorf("%w: opening verified file: %w", errUnverifiedPath, err)
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("%w: inspecting opened file: %w", errUnverifiedPath, err)
	}
	if !opened.Mode().IsRegular() || !os.SameFile(before, opened) {
		return nil, errUnverifiedPath
	}
	if hooks.afterOpen != nil {
		hooks.afterOpen()
	}
	var data []byte
	if readContent {
		data, err = io.ReadAll(file)
		if err != nil {
			return nil, fmt.Errorf("reading verified file: %w", err)
		}
	}
	if verify != nil {
		if err := verify(data); err != nil {
			return nil, fmt.Errorf("%w: verifying file ownership: %w", errUnverifiedPath, err)
		}
	}
	after, err := lstatRootPath(root, rel, false)
	if err != nil || !os.SameFile(opened, after) {
		return nil, errUnverifiedPath
	}
	final, err := file.Stat()
	if err != nil || !os.SameFile(opened, final) {
		return nil, errUnverifiedPath
	}
	return data, nil
}

func settingsDataHasOPFCommand(data []byte) bool {
	var entire map[string]json.RawMessage
	if json.Unmarshal(data, &entire) != nil {
		return false
	}
	var redaction map[string]json.RawMessage
	if json.Unmarshal(entire["redaction"], &redaction) != nil {
		return false
	}
	var opf map[string]json.RawMessage
	if json.Unmarshal(redaction["openai_privacy_filter"], &opf) != nil {
		return false
	}
	_, ok := opf["command"]
	return ok
}

func lstatRootPath(root *os.Root, rel string, finalDir bool) (fs.FileInfo, error) {
	if filepath.IsAbs(rel) {
		return nil, errors.New("path provenance requires relative path")
	}
	parts := strings.Split(filepath.Clean(rel), string(filepath.Separator))
	if len(parts) == 0 {
		return nil, errors.New("invalid worktree-relative path")
	}
	var info fs.FileInfo
	for i, part := range parts {
		if part == "" || part == "." || part == ".." {
			return nil, errors.New("invalid worktree-relative path")
		}
		component := filepath.Join(parts[:i+1]...)
		var err error
		info, err = root.Lstat(component)
		if err != nil {
			return nil, fmt.Errorf("inspecting path component: %w", err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("path component %s is a symlink", component)
		}
		isFinal := i == len(parts)-1
		if (!isFinal || finalDir) && !info.IsDir() {
			return nil, fmt.Errorf("path component %s is not a directory", component)
		}
		if isFinal && !finalDir && !info.Mode().IsRegular() {
			return nil, fmt.Errorf("path component %s is not a regular file", component)
		}
	}
	return info, nil
}

func gitPathTrackedInHEAD(ctx context.Context, root, rel string) (bool, error) {
	cmd := exec.CommandContext(ctx, "git", "-C", root, "ls-tree", "-rz", "--name-only", "HEAD")
	output, err := cmd.Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 128 {
			unborn, unbornErr := gitHEADIsUnborn(ctx, root)
			if unbornErr != nil {
				return false, unbornErr
			}
			if unborn {
				return false, nil
			}
		}
		return false, fmt.Errorf("checking settings ownership in HEAD: %w", err)
	}
	return nulListContainsEquivalent(output, rel), nil
}

func gitHEADIsUnborn(ctx context.Context, root string) (bool, error) {
	cmd := exec.CommandContext(ctx, "git", "-C", root, "status", "--porcelain=v2", "--branch", "--untracked-files=no")
	output, err := cmd.Output()
	if err != nil {
		return false, fmt.Errorf("checking unborn HEAD: %w", err)
	}
	return bytes.Contains(output, []byte("# branch.oid (initial)\n")), nil
}

func gitPathTracked(ctx context.Context, root, rel string) (bool, error) {
	cmd := exec.CommandContext(ctx, "git", "-C", root, "ls-files", "-z")
	output, err := cmd.Output()
	if err != nil {
		return false, fmt.Errorf("checking settings ownership: %w", err)
	}
	return nulListContainsEquivalent(output, rel), nil
}

func nulListContainsEquivalent(output []byte, rel string) bool {
	for _, name := range bytes.Split(output, []byte{0}) {
		if gitpath.Equivalent(string(name), rel) {
			return true
		}
	}
	return false
}
