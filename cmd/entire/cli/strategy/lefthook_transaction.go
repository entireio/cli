package strategy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/entireio/cli/cmd/entire/cli/gitdir"
	"github.com/entireio/cli/cmd/entire/cli/jsonutil"
	"github.com/entireio/cli/cmd/entire/cli/osroot"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/worktreedir"
)

func installLefthookFiles(ctx context.Context, absolutePath bool) (int, error) { //nolint:unparam // Task 3 wires the settings-selected true path.
	repoRoot, err := paths.WorktreeRoot(ctx)
	if err != nil {
		return 0, fmt.Errorf("resolve worktree root: %w", err)
	}
	return installLefthookFilesAt(ctx, repoRoot, absolutePath, nil)
}

func installLefthookFilesAt(ctx context.Context, repoRoot string, absolutePath bool, beforePublish func(string) error) (int, error) {
	root, err := worktreedir.OpenAt(repoRoot)
	if err != nil {
		return 0, fmt.Errorf("open worktree: %w", err)
	}
	managers, detectErr := detectHookManagersForIntegration(repoRoot)
	if detectErr != nil {
		return 0, detectErr
	}
	manager, ok, selectErr := selectLefthookIntegrationManager(managers)
	if selectErr != nil {
		return 0, selectErr
	}
	if !ok {
		return 0, errors.New("lefthook main config not found")
	}
	if err := validateLefthookMainConfig(root, manager); err != nil {
		return 0, err
	}

	existing, _, err := readOptionalRegular(root, lefthookLocalConfigName)
	if err != nil {
		return 0, fmt.Errorf("read %s: %w", lefthookLocalConfigName, err)
	}
	merged, err := mergeLefthookLocalConfig(existing)
	if err != nil {
		return 0, err
	}

	cmdPrefix, err := hookCmdPrefix(absolutePath)
	if err != nil {
		return 0, err
	}
	specs := buildHookSpecs(cmdPrefix)
	for _, spec := range specs {
		name := lefthookScriptPath(spec.name)
		current, _, readErr := readOptionalRegular(root, name)
		if readErr != nil {
			return 0, fmt.Errorf("read %s: %w", name, readErr)
		}
		if current != nil && !lefthookScriptOwned(string(current)) {
			return 0, fmt.Errorf("%w: script path %s already exists", ErrLefthookOwnedEntryConflict, name)
		}
	}

	commonDir, err := gitdir.CommonDirForWorktree(ctx, repoRoot)
	if err != nil {
		return 0, fmt.Errorf("resolve git common directory: %w", err)
	}
	gitRoot, err := gitdir.OpenAt(commonDir)
	if err != nil {
		return 0, fmt.Errorf("open git common directory: %w", err)
	}
	exclude, _, err := readOptionalRegular(gitRoot, "info/exclude")
	if err != nil {
		return 0, fmt.Errorf("read git exclude: %w", err)
	}
	mergedExclude := mergeLefthookInfoExclude(exclude)

	createdDirs, gitInfoCreated, err := prepareLefthookDirectories(root, gitRoot, specs)
	if err != nil {
		return 0, err
	}
	staged, configFile, err := stageLefthookArtifacts(root, gitRoot, specs, mergedExclude, merged)
	if err != nil {
		cleanupLefthookDirs(root, createdDirs)
		cleanupGitInfoDir(gitRoot, gitInfoCreated)
		return 0, err
	}
	if len(staged) == 0 {
		return 0, nil
	}
	defer cleanupStagedLefthookFiles(staged)
	written, err := publishLefthookTransaction(staged, configFile, beforePublish)
	if err != nil {
		cleanupLefthookDirs(root, createdDirs)
		cleanupGitInfoDir(gitRoot, gitInfoCreated)
	}
	return written, err
}

func prepareLefthookDirectories(root, gitRoot *os.Root, specs []hookSpec) ([]string, bool, error) {
	created := make([]string, 0, len(specs)+1)
	dirs := []string{lefthookLocalDir}
	for _, spec := range specs {
		dirs = append(dirs, filepath.ToSlash(filepath.Join(lefthookLocalDir, spec.name)))
	}
	for _, dir := range dirs {
		info, err := osroot.LstatNoSymlinks(root, dir)
		switch {
		case os.IsNotExist(err):
			created = append(created, dir)
		case err != nil:
			cleanupLefthookDirs(root, created)
			return nil, false, fmt.Errorf("inspect Lefthook directory %s: %w", dir, err)
		case !info.IsDir():
			cleanupLefthookDirs(root, created)
			return nil, false, fmt.Errorf("%s is not a directory", dir)
		}
		if err := osroot.MkdirAllNoSymlink(root, dir, 0o755); err != nil {
			cleanupLefthookDirs(root, created)
			return nil, false, fmt.Errorf("create Lefthook directory %s: %w", dir, err)
		}
	}
	gitInfoCreated := false
	info, err := osroot.LstatNoSymlinks(gitRoot, "info")
	switch {
	case os.IsNotExist(err):
		gitInfoCreated = true
	case err != nil:
		cleanupLefthookDirs(root, created)
		return nil, false, fmt.Errorf("inspect git info directory: %w", err)
	case !info.IsDir():
		cleanupLefthookDirs(root, created)
		return nil, false, errors.New("git info path is not a directory")
	}
	if err := osroot.MkdirAllNoSymlink(gitRoot, "info", 0o755); err != nil {
		cleanupLefthookDirs(root, created)
		return nil, false, fmt.Errorf("create git info directory: %w", err)
	}
	return created, gitInfoCreated, nil
}

func stageLefthookArtifacts(root, gitRoot *os.Root, specs []hookSpec, exclude, config []byte) ([]*stagedLefthookFile, *stagedLefthookFile, error) {
	staged := make([]*stagedLefthookFile, 0, len(specs)+2)
	stage := func(target *os.Root, name string, data []byte, mode os.FileMode, counted, force bool) error {
		file, err := stageLefthookFile(target, name, data, mode, counted, force)
		if errors.Is(err, errLefthookFileUnchanged) {
			return nil
		}
		if err != nil {
			return err
		}
		staged = append(staged, file)
		return nil
	}
	for _, spec := range specs {
		if err := stage(root, lefthookScriptPath(spec.name), []byte(renderLefthookScript(spec)), 0o755, true, false); err != nil {
			cleanupStagedLefthookFiles(staged)
			return nil, nil, err
		}
	}
	if err := stage(gitRoot, "info/exclude", exclude, 0o644, false, false); err != nil {
		cleanupStagedLefthookFiles(staged)
		return nil, nil, err
	}
	configFile, err := stageLefthookFile(root, lefthookLocalConfigName, config, 0o644, true, len(staged) > 0)
	if errors.Is(err, errLefthookFileUnchanged) {
		return staged, nil, nil
	}
	if err != nil {
		cleanupStagedLefthookFiles(staged)
		return nil, nil, err
	}
	staged = append(staged, configFile)
	return staged, configFile, nil
}

func publishLefthookTransaction(staged []*stagedLefthookFile, config *stagedLefthookFile, beforePublish func(string) error) (int, error) {
	if err := config.deactivate(); err != nil {
		return 0, errors.Join(err, rollbackStagedLefthookFiles(staged))
	}
	written := 0
	for _, file := range staged {
		if beforePublish != nil {
			if err := beforePublish(file.name); err != nil {
				return 0, errors.Join(err, rollbackStagedLefthookFiles(staged))
			}
		}
		if err := file.publish(); err != nil {
			return 0, errors.Join(err, rollbackStagedLefthookFiles(staged))
		}
		if file.counted {
			written++
		}
	}
	if err := config.finishDeactivation(); err != nil {
		return 0, errors.Join(err, rollbackStagedLefthookFiles(staged))
	}
	return written, nil
}

type stagedLefthookFile struct {
	parent         *os.Root
	name           string
	leaf           string
	temp           string
	original       []byte
	originalMode   os.FileMode
	originalExists bool
	published      bool
	counted        bool
	closeParent    func()
	deactivated    string
}

func stageLefthookFile(root *os.Root, name string, data []byte, mode os.FileMode, counted, force bool) (*stagedLefthookFile, error) {
	parent, leaf, closeParent, err := osroot.OpenParentNoSymlinks(root, name)
	if err != nil {
		return nil, fmt.Errorf("open parent for %s: %w", name, err)
	}
	original, info, err := readOptionalRegular(parent, leaf)
	if err != nil {
		closeParent()
		return nil, fmt.Errorf("read existing %s: %w", name, err)
	}
	changed := original == nil || !bytes.Equal(original, data) || info.Mode().Perm() != mode.Perm()
	if !changed && !force {
		closeParent()
		return nil, errLefthookFileUnchanged
	}
	temp, tempName, err := jsonutil.CreateTempIn(parent, leaf)
	if err != nil {
		closeParent()
		return nil, fmt.Errorf("stage %s: %w", name, err)
	}
	removeTemp := true
	defer func() {
		if removeTemp {
			_ = temp.Close()
			_ = parent.Remove(tempName) //nolint:errcheck // best-effort cleanup after a staging failure
			closeParent()
		}
	}()
	if err := integrationFault("write", name); err != nil {
		return nil, err
	}
	if _, err := temp.Write(data); err != nil {
		return nil, fmt.Errorf("stage %s: %w", name, err)
	}
	if err := temp.Close(); err != nil {
		return nil, fmt.Errorf("stage %s: %w", name, err)
	}
	if err := parent.Chmod(tempName, mode); err != nil {
		return nil, fmt.Errorf("stage %s: %w", name, err)
	}
	removeTemp = false
	file := &stagedLefthookFile{
		parent: parent, name: name, leaf: leaf, temp: tempName, original: original,
		counted: counted && changed, closeParent: closeParent,
	}
	if info != nil {
		file.originalExists = true
		file.originalMode = info.Mode().Perm()
	}
	return file, nil
}

func (file *stagedLefthookFile) deactivate() error {
	if !file.originalExists {
		return nil
	}
	placeholder, backupName, err := jsonutil.CreateTempIn(file.parent, file.leaf+".backup")
	if err != nil {
		return fmt.Errorf("prepare deactivation of %s: %w", file.name, err)
	}
	if err := placeholder.Close(); err != nil {
		_ = file.parent.Remove(backupName) //nolint:errcheck // best-effort cleanup after closing the placeholder fails
		return fmt.Errorf("prepare deactivation of %s: %w", file.name, err)
	}
	if err := file.parent.Remove(backupName); err != nil {
		return fmt.Errorf("prepare deactivation of %s: %w", file.name, err)
	}
	if err := file.parent.Rename(file.leaf, backupName); err != nil {
		return fmt.Errorf("deactivate %s: %w", file.name, err)
	}
	file.deactivated = backupName
	return nil
}

func (file *stagedLefthookFile) finishDeactivation() error {
	if file.deactivated == "" {
		return nil
	}
	if err := file.parent.Remove(file.deactivated); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove deactivated %s backup: %w", file.name, err)
	}
	file.deactivated = ""
	return nil
}

func (file *stagedLefthookFile) publish() error {
	if err := file.parent.Rename(file.temp, file.leaf); err != nil {
		return fmt.Errorf("publish %s: %w", file.name, err)
	}
	file.temp = ""
	file.published = true
	return nil
}

func rollbackStagedLefthookFiles(files []*stagedLefthookFile) error {
	var rollbackErrs []error
	for i := len(files) - 1; i >= 0; i-- {
		file := files[i]
		if file.deactivated != "" {
			if file.published {
				if err := file.parent.Remove(file.leaf); err != nil && !os.IsNotExist(err) {
					rollbackErrs = append(rollbackErrs, fmt.Errorf("remove published %s: %w", file.name, err))
				}
				file.published = false
			}
			if err := file.parent.Rename(file.deactivated, file.leaf); err != nil {
				rollbackErrs = append(rollbackErrs, fmt.Errorf("reactivate %s: %w", file.name, err))
			} else {
				file.deactivated = ""
			}
			if file.temp != "" {
				if err := file.parent.Remove(file.temp); err != nil && !os.IsNotExist(err) {
					rollbackErrs = append(rollbackErrs, fmt.Errorf("remove staged %s: %w", file.name, err))
				}
				file.temp = ""
			}
			continue
		}
		if file.published {
			if file.originalExists {
				if err := jsonutil.WriteFileAtomicIn(file.parent, file.leaf, file.original, file.originalMode); err != nil {
					rollbackErrs = append(rollbackErrs, fmt.Errorf("restore %s: %w", file.name, err))
				}
			} else if err := file.parent.Remove(file.leaf); err != nil && !os.IsNotExist(err) {
				rollbackErrs = append(rollbackErrs, fmt.Errorf("remove %s: %w", file.name, err))
			}
			file.published = false
		}
		if file.temp != "" {
			if err := file.parent.Remove(file.temp); err != nil && !os.IsNotExist(err) {
				rollbackErrs = append(rollbackErrs, fmt.Errorf("remove staged %s: %w", file.name, err))
			}
			file.temp = ""
		}
	}
	return errors.Join(rollbackErrs...)
}

func cleanupStagedLefthookFiles(files []*stagedLefthookFile) {
	for _, file := range files {
		if file.temp != "" {
			_ = file.parent.Remove(file.temp) //nolint:errcheck // best-effort deferred cleanup; rollback reports material failures
			file.temp = ""
		}
		file.closeParent()
	}
}

func cleanupLefthookDirs(root *os.Root, dirs []string) {
	for i := len(dirs) - 1; i >= 0; i-- {
		_ = root.Remove(dirs[i]) //nolint:errcheck // best-effort cleanup of directories created during a failed transaction
	}
}

func cleanupGitInfoDir(root *os.Root, created bool) {
	if created {
		_ = root.Remove("info") //nolint:errcheck // best-effort cleanup; non-empty means the directory was not ours to remove
	}
}
