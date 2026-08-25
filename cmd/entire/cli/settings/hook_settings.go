package settings

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/entireio/cli/cmd/entire/cli/settings/repopolicy"
)

type hookFieldDisposition uint8

const (
	hookFieldIgnored hookFieldDisposition = iota
	hookFieldAllowed
)

// These maps are intentionally exhaustive. The reflection-backed test forces
// every new JSON field to receive an explicit hook-time policy decision.
var hookEntireFieldPolicy = map[string]hookFieldDisposition{
	"enabled":                 hookFieldAllowed,
	"local_dev":               hookFieldIgnored,
	"log_level":               hookFieldAllowed,
	"strategy_options":        hookFieldIgnored,
	"absolute_git_hook_path":  hookFieldIgnored,
	"telemetry":               hookFieldIgnored,
	"redaction":               hookFieldAllowed,
	"review_profiles":         hookFieldIgnored,
	"review_default_profile":  hookFieldIgnored,
	"review":                  hookFieldIgnored,
	"review_fix_agent":        hookFieldIgnored,
	"investigate":             hookFieldIgnored,
	"commit_linking":          hookFieldIgnored,
	"external_agents":         hookFieldIgnored,
	"summary_generation":      hookFieldIgnored,
	"vercel":                  hookFieldIgnored,
	"summary_timeout_seconds": hookFieldIgnored,
	"sign_checkpoint_commits": hookFieldIgnored,
	"checkpoints":             hookFieldIgnored,
	"strategy":                hookFieldIgnored,
}

var hookRedactionFieldPolicy = map[string]hookFieldDisposition{
	"pii":                   hookFieldAllowed,
	"custom_redactions":     hookFieldAllowed,
	"openai_privacy_filter": hookFieldIgnored,
	"externalize_images":    hookFieldAllowed,
	"betterleaks":           hookFieldIgnored,
	"goredact":              hookFieldIgnored,
}

// LoadForRepoPolicy returns the hook-time settings view for one already
// classified repository. Locally activated repositories retain the complete
// merged loader. Global-only repositories receive a positive whitelist of
// non-executable declarative fields.
func LoadForRepoPolicy(ctx context.Context, policy repopolicy.RepoPolicy) (*EntireSettings, error) {
	if policy.ActivationSource == repopolicy.ActivationLocal {
		return loadLocalPolicySettings(ctx, policy)
	}
	repository := repopolicy.Repository{
		WorktreeRoot: policy.WorktreeRoot,
		GitCommonDir: policy.GitCommonDir,
		WorktreeKey:  policy.WorktreeKey,
	}
	provenance, err := repopolicy.VerifySettingsProvenance(ctx, repository)
	if err != nil {
		return nil, fmt.Errorf("verifying hook settings provenance: %w", err)
	}
	settings := &EntireSettings{Enabled: true}
	if provenance.ProjectVerified {
		if err := mergeHookSettingsFile(settings, provenance.ProjectPath); err != nil {
			return nil, fmt.Errorf("reading project hook settings: %w", err)
		}
	}
	if provenance.LocalVerified {
		if err := mergeHookSettingsFile(settings, provenance.LocalPath); err != nil {
			return nil, fmt.Errorf("reading local hook settings: %w", err)
		}
	}
	return settings, nil
}

func loadLocalPolicySettings(ctx context.Context, policy repopolicy.RepoPolicy) (*EntireSettings, error) {
	repository := repopolicy.Repository{
		WorktreeRoot: policy.WorktreeRoot,
		GitCommonDir: policy.GitCommonDir,
		WorktreeKey:  policy.WorktreeKey,
	}
	provenance, err := repopolicy.VerifySettingsProvenance(ctx, repository)
	if err != nil {
		return nil, fmt.Errorf("verifying local hook settings provenance: %w", err)
	}
	projectPath := ""
	if provenance.ProjectPathSafe {
		projectPath = provenance.ProjectPath
	}
	localPath := ""
	if provenance.LocalPathSafe {
		localPath = provenance.LocalPath
	}
	preferencesPath, err := clonePreferencesPathForWorktreeRoot(ctx, policy.WorktreeRoot)
	if err != nil {
		return nil, fmt.Errorf("resolving clone preferences: %w", err)
	}
	return loadMergedSettings(ctx, projectPath, preferencesPath, localPath)
}

func mergeHookSettingsFile(settings *EntireSettings, path string) error {
	data, err := os.ReadFile(path) //nolint:gosec // fixed path verified from the canonical worktree root
	if err != nil {
		return fmt.Errorf("reading settings file: %w", err)
	}
	var raw map[string]json.RawMessage
	if err := decodeHookObject(data, &raw); err != nil {
		return fmt.Errorf("parsing settings: %w", err)
	}
	if value, ok := raw["enabled"]; ok {
		if err := unmarshalHookField("enabled", value, &settings.Enabled); err != nil {
			return err
		}
	}
	if value, ok := raw["log_level"]; ok {
		var level string
		if err := unmarshalHookField("log_level", value, &level); err != nil {
			return err
		}
		if level != "" {
			settings.LogLevel = level
		}
	}
	if value, ok := raw["redaction"]; ok {
		if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return nil
		}
		if settings.Redaction == nil {
			settings.Redaction = &RedactionSettings{}
		}
		if err := mergeHookRedaction(settings.Redaction, value); err != nil {
			return fmt.Errorf("parsing redaction field: %w", err)
		}
	}
	return nil
}

func mergeHookRedaction(settings *RedactionSettings, data json.RawMessage) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("parsing redaction: %w", err)
	}
	if value, ok := raw["pii"]; ok {
		if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			settings.PII = nil
		} else {
			if settings.PII == nil {
				settings.PII = &PIISettings{}
			}
			if err := mergeHookPII(settings.PII, value); err != nil {
				return err
			}
		}
	}
	if value, ok := raw["custom_redactions"]; ok {
		if err := mergeHookStringMap("redaction.custom_redactions", value, &settings.CustomRedactions); err != nil {
			return err
		}
	}
	if value, ok := raw["externalize_images"]; ok {
		if err := unmarshalHookField("redaction.externalize_images", value, &settings.ExternalizeImages); err != nil {
			return err
		}
	}
	return nil
}

func mergeHookPII(settings *PIISettings, data json.RawMessage) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("parsing pii: %w", err)
	}
	if value, ok := raw["enabled"]; ok {
		if err := unmarshalHookField("redaction.pii.enabled", value, &settings.Enabled); err != nil {
			return err
		}
	}
	for key, destination := range map[string]**bool{
		"email": &settings.Email, "phone": &settings.Phone, "address": &settings.Address,
	} {
		if value, ok := raw[key]; ok {
			var enabled bool
			if err := unmarshalHookField("redaction.pii."+key, value, &enabled); err != nil {
				return err
			}
			*destination = &enabled
		}
	}
	if value, ok := raw["custom_patterns"]; ok {
		if err := mergeHookStringMap("redaction.pii.custom_patterns", value, &settings.CustomPatterns); err != nil {
			return err
		}
	}
	return nil
}

func mergeHookStringMap(name string, data json.RawMessage, destination *map[string]string) error {
	var values map[string]string
	if err := unmarshalHookField(name, data, &values); err != nil {
		return err
	}
	if *destination == nil {
		*destination = map[string]string{}
	}
	for label, pattern := range values {
		(*destination)[label] = pattern
	}
	return nil
}

func unmarshalHookField(name string, data json.RawMessage, destination any) error {
	if err := json.Unmarshal(data, destination); err != nil {
		return fmt.Errorf("parsing %s field: %w", name, err)
	}
	return nil
}

func decodeHookObject(data []byte, destination any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(destination); err != nil {
		return fmt.Errorf("decoding JSON object: %w", err)
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("multiple JSON values")
		}
		return fmt.Errorf("decoding trailing JSON value: %w", err)
	}
	return nil
}
