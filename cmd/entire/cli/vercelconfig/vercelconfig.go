package vercelconfig

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"sync"

	"github.com/entireio/cli/cmd/entire/cli/jsonutil"
	"github.com/entireio/cli/cmd/entire/cli/settings"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/filemode"
	"github.com/go-git/go-git/v6/plumbing/object"
)

const (
	BranchPattern = "entire/**"
	FileName      = "vercel.json"
)

var (
	cachedSettingsMu sync.RWMutex
	cachedSettings   *settings.EntireSettings
)

var errSettingsNotInitialized = errors.New("vercel settings cache not initialized")

// InitSettings loads repository settings for the current command context and
// stores them in a small package cache.
func InitSettings(ctx context.Context) error {
	cachedSettingsMu.RLock()
	if cachedSettings != nil {
		cachedSettingsMu.RUnlock()
		return nil
	}
	cachedSettingsMu.RUnlock()

	s, err := settings.Load(ctx)
	if err != nil {
		return fmt.Errorf("load settings: %w", err)
	}

	cachedSettingsMu.Lock()
	cachedSettings = s
	cachedSettingsMu.Unlock()

	return nil
}

// CachedSettings returns the most recently initialized repository settings.
func CachedSettings() (*settings.EntireSettings, error) {
	cachedSettingsMu.RLock()
	defer cachedSettingsMu.RUnlock()
	if cachedSettings == nil {
		return nil, errSettingsNotInitialized
	}
	return cachedSettings, nil
}

// ResetSettingsCache clears the cached settings.
// Primarily intended for tests that exercise multiple repositories in one process.
func ResetSettingsCache() {
	cachedSettingsMu.Lock()
	cachedSettings = nil
	cachedSettingsMu.Unlock()
}

// Load reads a Vercel config file if present.
func Load(path string) (map[string]any, bool, error) {
	//nolint:gosec // path is provided by repository-local callers and intentionally supports arbitrary locations in tests
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return make(map[string]any), false, nil
		}
		return nil, false, fmt.Errorf("read %s: %w", FileName, err)
	}

	var config map[string]any
	if err := json.Unmarshal(data, &config); err != nil {
		return nil, false, fmt.Errorf("parse %s: %w", FileName, err)
	}
	if config == nil {
		config = make(map[string]any)
	}

	return config, DeploymentDisabled(config), nil
}

// DeploymentDisabled reports whether Entire branches are disabled in the config.
func DeploymentDisabled(config map[string]any) bool {
	gitConfig, ok := config["git"].(map[string]any)
	if !ok {
		return false
	}
	deploymentEnabled, ok := gitConfig["deploymentEnabled"].(map[string]any)
	if !ok {
		return false
	}
	enabled, ok := deploymentEnabled[BranchPattern].(bool)
	return ok && !enabled
}

// MergeDeploymentDisabled sets deploymentEnabled["entire/**"] = false while preserving other fields.
func MergeDeploymentDisabled(config map[string]any) {
	gitConfig, ok := config["git"].(map[string]any)
	if !ok {
		gitConfig = make(map[string]any)
		config["git"] = gitConfig
	}

	deploymentEnabled, ok := gitConfig["deploymentEnabled"].(map[string]any)
	if !ok {
		deploymentEnabled = make(map[string]any)
		gitConfig["deploymentEnabled"] = deploymentEnabled
	}

	deploymentEnabled[BranchPattern] = false
}

// Marshal formats a Vercel config with a trailing newline.
func Marshal(config map[string]any) ([]byte, error) {
	output, err := jsonutil.MarshalIndentWithNewline(config, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal %s: %w", FileName, err)
	}
	return output, nil
}

// MaybeMergeMetadataBranchConfig ensures the metadata branch root tree contains
// a vercel.json disabling deployments for Entire branches when Vercel support
// is enabled in cached settings. Existing vercel.json content is preserved.
func MaybeMergeMetadataBranchConfig(repo *git.Repository, rootTreeHash plumbing.Hash) (plumbing.Hash, error) {
	projectSettings, settingsErr := CachedSettings()
	if settingsErr != nil {
		if errors.Is(settingsErr, errSettingsNotInitialized) {
			return rootTreeHash, nil
		}
		return plumbing.ZeroHash, fmt.Errorf("get cached settings: %w", settingsErr)
	}
	if !projectSettings.Vercel {
		return rootTreeHash, nil
	}

	config := make(map[string]any)
	var existingContents string
	var rootEntries []object.TreeEntry
	if rootTreeHash != plumbing.ZeroHash {
		tree, treeErr := repo.TreeObject(rootTreeHash)
		if treeErr != nil && !errors.Is(treeErr, plumbing.ErrObjectNotFound) {
			return plumbing.ZeroHash, fmt.Errorf("read metadata tree: %w", treeErr)
		}
		if treeErr == nil {
			rootEntries = tree.Entries
			if file, fileErr := tree.File(FileName); fileErr == nil {
				contents, contentsErr := file.Contents()
				if contentsErr != nil {
					return plumbing.ZeroHash, fmt.Errorf("read %s from metadata branch: %w", FileName, contentsErr)
				}
				existingContents = contents
				if unmarshalErr := json.Unmarshal([]byte(contents), &config); unmarshalErr != nil {
					config = make(map[string]any)
				}
			}
		}
	}

	MergeDeploymentDisabled(config)
	output, err := Marshal(config)
	if err != nil {
		return plumbing.ZeroHash, err
	}
	if string(output) == existingContents {
		return rootTreeHash, nil
	}

	blobHash, err := createBlobFromContent(repo, output)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("create %s blob: %w", FileName, err)
	}

	updatedEntries := make([]object.TreeEntry, 0, len(rootEntries)+1)
	replaced := false
	for _, entry := range rootEntries {
		if entry.Name == FileName {
			updatedEntries = append(updatedEntries, object.TreeEntry{
				Name: FileName,
				Mode: filemode.Regular,
				Hash: blobHash,
			})
			replaced = true
			continue
		}
		updatedEntries = append(updatedEntries, entry)
	}
	if !replaced {
		updatedEntries = append(updatedEntries, object.TreeEntry{
			Name: FileName,
			Mode: filemode.Regular,
			Hash: blobHash,
		})
	}

	sort.Slice(updatedEntries, func(i, j int) bool {
		return updatedEntries[i].Name < updatedEntries[j].Name
	})

	return storeTree(repo, updatedEntries)
}

func createBlobFromContent(repo *git.Repository, content []byte) (plumbing.Hash, error) {
	obj := repo.Storer.NewEncodedObject()
	obj.SetType(plumbing.BlobObject)
	obj.SetSize(int64(len(content)))

	writer, err := obj.Writer()
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("get object writer: %w", err)
	}
	if _, err := writer.Write(content); err != nil {
		_ = writer.Close()
		return plumbing.ZeroHash, fmt.Errorf("write blob content: %w", err)
	}
	if err := writer.Close(); err != nil {
		return plumbing.ZeroHash, fmt.Errorf("close blob writer: %w", err)
	}

	hash, err := repo.Storer.SetEncodedObject(obj)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("store blob object: %w", err)
	}
	return hash, nil
}

func storeTree(repo *git.Repository, entries []object.TreeEntry) (plumbing.Hash, error) {
	tree := &object.Tree{Entries: entries}
	obj := repo.Storer.NewEncodedObject()
	if err := tree.Encode(obj); err != nil {
		return plumbing.ZeroHash, fmt.Errorf("encode tree: %w", err)
	}
	hash, err := repo.Storer.SetEncodedObject(obj)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("store tree: %w", err)
	}
	return hash, nil
}
