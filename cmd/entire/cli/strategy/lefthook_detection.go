package strategy

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/osroot"
	"github.com/entireio/cli/cmd/entire/cli/worktreedir"
	"gopkg.in/yaml.v3"
)

func lefthookConfigNames(local bool) []string {
	variant := ""
	if local {
		variant = "-local"
	}
	names := make([]string, 0, 15)
	for _, prefix := range []string{"", ".", ".config/"} {
		for _, ext := range []string{"yml", "yaml", "json", "jsonc", "toml"} {
			names = append(names, prefix+"lefthook"+variant+"."+ext)
		}
	}
	return names
}

// detectHookManagersForIntegration retains every Lefthook variant. The general
// warning detector intentionally deduplicates managers, but integration safety
// depends on distinguishing one main config from two competing main configs.
func detectHookManagersForIntegration(repoRoot string) ([]hookManager, error) {
	root, err := worktreedir.OpenAt(repoRoot)
	if err != nil {
		return nil, fmt.Errorf("open worktree: %w", err)
	}
	checks := []hookManager{
		{Name: "Husky", ConfigPath: ".husky/", OverwritesHooks: true, IntegrationKind: hookManagerIntegrationHookDirectory},
		{Name: "pre-commit", ConfigPath: ".pre-commit-config.yaml"},
		{Name: "Overcommit", ConfigPath: ".overcommit.yml"},
		{Name: "hk", ConfigPath: "hk.pkl"},
		{Name: "hk", ConfigPath: "hk.local.pkl"},
		{Name: "hk", ConfigPath: ".config/hk.pkl"},
		{Name: "hk", ConfigPath: ".config/hk.local.pkl"},
	}
	managers := make([]hookManager, 0, len(checks)+2)
	seen := make(map[string]bool)
	for _, manager := range checks {
		name := strings.TrimSuffix(manager.ConfigPath, "/")
		info, statErr := osroot.LstatNoSymlinks(root, name)
		if statErr != nil {
			if os.IsNotExist(statErr) {
				continue
			}
			return nil, &hookManagerDetectionError{manager: manager, err: fmt.Errorf("inspect hook manager candidate %s: %w", manager.ConfigPath, statErr)}
		}
		wantDir := manager.IntegrationKind == hookManagerIntegrationHookDirectory
		if wantDir != info.IsDir() || (!wantDir && !info.Mode().IsRegular()) {
			return nil, &hookManagerDetectionError{manager: manager, err: fmt.Errorf("hook manager candidate %s has an unsupported file type", manager.ConfigPath)}
		}
		if !seen[manager.Name] {
			seen[manager.Name] = true
			managers = append(managers, manager)
		}
	}
	for _, name := range append(append([]string{}, lefthookMainConfigNames...), lefthookLocalConfigNames...) {
		info, statErr := osroot.LstatNoSymlinks(root, name)
		if statErr != nil {
			if os.IsNotExist(statErr) {
				continue
			}
			return nil, &hookManagerDetectionError{manager: hookManager{Name: lefthookManagerName, ConfigPath: name, OverwritesHooks: true, IntegrationKind: hookManagerIntegrationLefthook}, err: fmt.Errorf("inspect Lefthook config candidate %s: %w", name, statErr)}
		}
		if !info.Mode().IsRegular() {
			return nil, &hookManagerDetectionError{manager: hookManager{Name: lefthookManagerName, ConfigPath: name, OverwritesHooks: true, IntegrationKind: hookManagerIntegrationLefthook}, err: fmt.Errorf("lefthook config candidate %s is not a regular file", name)}
		}
		managers = append(managers, hookManager{
			Name:            lefthookManagerName,
			ConfigPath:      name,
			OverwritesHooks: true,
			IntegrationKind: hookManagerIntegrationLefthook,
		})
	}
	return managers, nil
}

func validateLefthookMainConfig(root *os.Root, manager hookManager) error {
	name := manager.ConfigPath
	if strings.HasPrefix(name, ".") && name != ".lefthook.yml" && name != ".lefthook.yaml" {
		return fmt.Errorf("safe Entire Lefthook integration inspection does not support config location %s", name)
	}
	ext := strings.ToLower(filepath.Ext(name))
	if ext == ".toml" || ext == ".jsonc" {
		return fmt.Errorf("lefthook config format %s is unsupported for safe source-directory inspection", ext)
	}
	data, err := osroot.ReadFileNoFollow(root, name)
	if err != nil {
		return fmt.Errorf("read Lefthook main config %s: %w", name, err)
	}
	document, err := parseYAMLMapping(data, name)
	if err != nil {
		return err
	}
	if _, found, duplicate := mappingValueCount(document, "extends"); found || duplicate {
		return errors.New("lefthook extends are unsupported for safe source-directory inspection")
	}
	if _, found, duplicate := mappingValueCount(document, "remotes"); found || duplicate {
		return errors.New("lefthook remotes are unsupported for safe source-directory inspection")
	}
	sourceDir, err := lefthookScalarSetting(document, "source_dir", ".lefthook")
	if err != nil {
		return err
	}
	sourceDirLocal, err := lefthookScalarSetting(document, "source_dir_local", lefthookLocalDir)
	if err != nil {
		return err
	}
	if sourceDirLocal != lefthookLocalDir {
		return fmt.Errorf("main config source_dir_local %q conflicts with Entire's %q", sourceDirLocal, lefthookLocalDir)
	}
	sourceDir, err = cleanLefthookSourceDir(sourceDir)
	if err != nil {
		return err
	}
	if sourceDir == lefthookLocalDir {
		return errors.New("lefthook source_dir collides with Entire's source_dir_local")
	}
	for _, hook := range gitHookNames {
		name := filepath.ToSlash(filepath.Join(sourceDir, hook, lefthookScriptName))
		_, statErr := osroot.LstatNoSymlinks(root, name)
		if statErr == nil {
			return fmt.Errorf("lefthook source_dir already contains managed script name %s", name)
		}
		if !os.IsNotExist(statErr) {
			return fmt.Errorf("inspect Lefthook source script %s: %w", name, statErr)
		}
	}
	return nil
}

func parseYAMLMapping(data []byte, name string) (*yaml.Node, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}, nil
	}
	var document yaml.Node
	if err := yaml.Unmarshal(data, &document); err != nil {
		return nil, fmt.Errorf("parse %s: %w", name, err)
	}
	if len(document.Content) != 1 || document.Content[0].Kind != yaml.MappingNode {
		return nil, fmt.Errorf("parse %s: top level must be a mapping", name)
	}
	return document.Content[0], nil
}

func mappingValueCount(parent *yaml.Node, key string) (value *yaml.Node, found, duplicate bool) {
	for i := 0; i+1 < len(parent.Content); i += 2 {
		if parent.Content[i].Value != key {
			continue
		}
		if found {
			return nil, true, true
		}
		value, found = parent.Content[i+1], true
	}
	return value, found, false
}

func lefthookScalarSetting(root *yaml.Node, key, fallback string) (string, error) {
	value, found, duplicate := mappingValueCount(root, key)
	if duplicate {
		return "", fmt.Errorf("duplicate Lefthook %s setting", key)
	}
	if !found {
		return fallback, nil
	}
	if value.Kind != yaml.ScalarNode || value.Tag != "!!str" {
		return "", fmt.Errorf("lefthook %s must be a string", key)
	}
	return value.Value, nil
}

func cleanLefthookSourceDir(value string) (string, error) {
	if value == "" || filepath.IsAbs(value) || filepath.VolumeName(value) != "" {
		return "", fmt.Errorf("lefthook source_dir %q must be a repository-relative directory", value)
	}
	cleaned := filepath.ToSlash(filepath.Clean(filepath.FromSlash(value)))
	if cleaned == "." || strings.HasPrefix(cleaned, "../") || cleaned == ".." {
		return "", fmt.Errorf("lefthook source_dir %q must stay beneath the repository", value)
	}
	return strings.TrimSuffix(cleaned, "/"), nil
}

func selectLefthookIntegrationManager(managers []hookManager) (hookManager, bool, error) {
	var mains, locals []hookManager
	for _, manager := range managers {
		if manager.IntegrationKind == hookManagerIntegrationLefthook {
			if containsString(lefthookMainConfigNames, manager.ConfigPath) {
				mains = append(mains, manager)
			} else if containsString(lefthookLocalConfigNames, manager.ConfigPath) {
				locals = append(locals, manager)
			}
		}
	}

	if len(mains) == 0 {
		if len(locals) > 0 {
			return hookManager{}, false, fmt.Errorf("%w: Lefthook has a local config but no main config", ErrLefthookAmbiguous)
		}
		return hookManager{}, false, nil
	}
	if len(mains) != 1 {
		return hookManager{}, false, fmt.Errorf("%w: found %d main configs", ErrLefthookAmbiguous, len(mains))
	}
	for _, local := range locals {
		if local.ConfigPath != lefthookLocalConfigName {
			return hookManager{}, false, fmt.Errorf("%w: alternate local config %s", ErrLefthookAmbiguous, local.ConfigPath)
		}
	}
	for _, manager := range managers {
		if manager.IntegrationKind != hookManagerIntegrationLefthook && manager.OverwritesHooks {
			return hookManager{}, false, fmt.Errorf("%w: %s also owns Git hooks", ErrLefthookAmbiguous, manager.Name)
		}
	}
	return mains[0], true, nil
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
