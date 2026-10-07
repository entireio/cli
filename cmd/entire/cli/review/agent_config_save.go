package review

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/settings"
)

// Reviewer config choices offered by --edit.
const (
	agentConfigKeep     = "keep"
	agentConfigCheckout = "checkout"
	agentConfigIsolated = "isolated"
	agentConfigFile     = "file"
)

// loadAgentConfigFile reads a reviewer config from a JSON file in the
// ReviewAgentConfig shape ({"settings": ..., "mcp_servers": ..., "extensions": [...]}).
func loadAgentConfigFile(path string) (*settings.ReviewAgentConfig, error) {
	if !filepath.IsAbs(path) {
		abs, err := filepath.Abs(path)
		if err != nil {
			return nil, fmt.Errorf("resolve %s: %w", path, err)
		}
		path = abs
	}
	data, err := os.ReadFile(path) //nolint:gosec // an explicit path the user typed for their own config
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var cfg settings.ReviewAgentConfig
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return &cfg, nil
}

// validateAgentConfigForSave checks a config against the user's own checkout;
// at run time it is checked again against the reviewed checkout.
func validateAgentConfigForSave(ctx context.Context, agentName string, cfg *settings.ReviewAgentConfig) error {
	var roots []string
	if root, err := paths.WorktreeRoot(ctx); err == nil {
		roots = append(roots, root)
		if resolved, err := filepath.EvalSymlinks(root); err == nil {
			roots = append(roots, resolved)
		}
	}
	return ValidateAgentConfig(agentName, toAgentConfig(cfg), roots)
}

// stripAgentConfigs removes reviewer configs from a profile before it is
// written to the committed settings file, where they would not be honored and
// could carry secrets.
func stripAgentConfigs(profile settings.ReviewProfileConfig) settings.ReviewProfileConfig {
	if len(profile.Agents) == 0 {
		return profile
	}
	agents := make(map[string]settings.ReviewConfig, len(profile.Agents))
	for worker, cfg := range profile.Agents {
		cfg.Config = nil
		agents[worker] = cfg
	}
	profile.Agents = agents
	return profile
}

// saveReviewAgentConfigs stores reviewer configs on profileName in a
// developer-owned layer: .entire/settings.local.json when it already defines
// the profile, otherwise clone-local preferences. Profiles merge whole per
// layer, so the effective profile is copied there with the configs applied.
// A nil config removes it. It returns the file written.
func saveReviewAgentConfigs(ctx context.Context, profileName string, configs map[string]*settings.ReviewAgentConfig) (string, error) {
	s, err := settings.Load(reviewSettingsContext(ctx))
	if err != nil {
		return "", fmt.Errorf("load settings: %w", err)
	}
	if s == nil {
		s = &settings.EntireSettings{}
	}
	profile, ok := s.ReviewProfiles[profileName]
	if !ok {
		return "", fmt.Errorf("review profile %q not found", profileName)
	}
	agents := make(map[string]settings.ReviewConfig, len(profile.Agents))
	for worker, cfg := range profile.Agents {
		agents[worker] = cfg
	}
	for worker, cfg := range configs {
		existing, ok := agents[worker]
		if !ok {
			return "", fmt.Errorf("review profile %q has no reviewer %q", profileName, worker)
		}
		existing.Config = cfg
		agents[worker] = existing
	}
	profile.Agents = agents

	_, localRaw, err := loadReviewSettingsRaw(ctx, reviewScopeLocal)
	if err != nil {
		return "", err
	}
	localProfiles, err := decodeRawReviewProfiles(localRaw)
	if err != nil {
		return "", err
	}
	if _, localOwns := localProfiles[profileName]; localOwns {
		if err := saveReviewProfile(ctx, profileName, profile, false, reviewScopeLocal); err != nil {
			return "", err
		}
		return reviewScopeLocal.file(), nil
	}
	err = settings.ModifyClonePreferences(ctx, func(p *settings.ClonePreferences) error {
		if p.ReviewProfiles == nil {
			p.ReviewProfiles = map[string]settings.ReviewProfileConfig{}
		}
		p.ReviewProfiles[profileName] = profile
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("save clone-local review preferences: %w", err)
	}
	return "clone-local review preferences", nil
}

// parseSetConfig parses --set-config worker=<file|isolated|none>.
func parseSetConfig(value string) (worker, source string, err error) {
	worker, source, ok := strings.Cut(value, "=")
	worker, source = strings.TrimSpace(worker), strings.TrimSpace(source)
	if !ok || worker == "" || source == "" {
		return "", "", fmt.Errorf("--set-config %q: use reviewer=<config.json|isolated|none>", value)
	}
	return worker, source, nil
}

// agentConfigFromSource turns a --set-config source into a config: "none"
// removes it (removed is true), "isolated" isolates with nothing extra,
// anything else is a file.
func agentConfigFromSource(source string) (cfg *settings.ReviewAgentConfig, removed bool, err error) {
	switch source {
	case "none":
		return nil, true, nil
	case agentConfigIsolated:
		return &settings.ReviewAgentConfig{}, false, nil
	default:
		cfg, err := loadAgentConfigFile(source)
		return cfg, false, err
	}
}

func (o reviewConfigureOptions) withoutConfigs() reviewConfigureOptions {
	o.Configs = nil
	return o
}

// configureAgentConfigs applies --set-config values to an existing profile.
func configureAgentConfigs(ctx context.Context, cmd *cobra.Command, profileName string, values []string, silentErr func(error) error) error {
	fail := func(err error) error {
		cmd.SilenceUsage = true
		fmt.Fprintln(cmd.ErrOrStderr(), err.Error())
		return silentErr(err)
	}
	s, err := settings.Load(reviewSettingsContext(ctx))
	if err != nil {
		return fail(fmt.Errorf("load settings: %w", err))
	}
	if s == nil {
		s = &settings.EntireSettings{}
	}
	profile, ok := s.ReviewProfiles[profileName]
	if !ok {
		return fail(fmt.Errorf("review profile %q not found; create it with --configure --set-agents first", profileName))
	}
	configs := map[string]*settings.ReviewAgentConfig{}
	for _, value := range values {
		worker, source, err := parseSetConfig(value)
		if err != nil {
			return fail(err)
		}
		workerCfg, ok := profile.Agents[worker]
		if !ok {
			return fail(fmt.Errorf("review profile %q has no reviewer %q", profileName, worker))
		}
		cfg, _, err := agentConfigFromSource(source)
		if err != nil {
			return fail(err)
		}
		if err := validateAgentConfigForSave(ctx, reviewAgentName(worker, workerCfg), cfg); err != nil {
			return fail(fmt.Errorf("--set-config %s: %w", worker, err))
		}
		configs[worker] = cfg
	}
	where, err := saveReviewAgentConfigs(ctx, profileName, configs)
	if err != nil {
		return fail(err)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Saved reviewer agent config for %s to %s.\n", strings.Join(sortedStringKeys(configs), ", "), where)
	return nil
}
