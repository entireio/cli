package strategy

import (
	"fmt"
	"github.com/entireio/cli/cmd/entire/cli/osroot"
	"github.com/entireio/cli/cmd/entire/cli/worktreedir"
	"os"
	"slices"
	"strings"
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

func selectLefthookIntegrationManager(managers []hookManager) (hookManager, bool, error) {
	var mains, locals []hookManager
	for _, manager := range managers {
		if manager.IntegrationKind == hookManagerIntegrationLefthook {
			if slices.Contains(lefthookMainConfigNames, manager.ConfigPath) {
				mains = append(mains, manager)
			} else if slices.Contains(lefthookLocalConfigNames, manager.ConfigPath) {
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
