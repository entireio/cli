package settings

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"

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

var hookPIIFieldPolicy = map[string]hookFieldDisposition{
	"enabled":         hookFieldAllowed,
	"email":           hookFieldAllowed,
	"phone":           hookFieldAllowed,
	"address":         hookFieldAllowed,
	"custom_patterns": hookFieldAllowed,
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
		if err := mergeHookSettingsData(settings, provenance.ProjectData); err != nil {
			return nil, fmt.Errorf("reading project hook settings: %w", err)
		}
	}
	if provenance.LocalVerified {
		if err := mergeHookSettingsData(settings, provenance.LocalData); err != nil {
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
	preferencesPath, err := clonePreferencesPathForWorktreeRoot(ctx, policy.WorktreeRoot)
	if err != nil {
		return nil, fmt.Errorf("resolving clone preferences: %w", err)
	}
	return loadVerifiedPolicySettings(provenance, preferencesPath)
}

func mergeHookSettingsData(settings *EntireSettings, data []byte) error {
	var raw map[string]json.RawMessage
	if err := decodeHookObject(data, &raw); err != nil {
		return fmt.Errorf("parsing settings: %w", err)
	}
	if value, ok := raw["enabled"]; ok {
		if err := unmarshalHookNonNullField("enabled", value, &settings.Enabled); err != nil {
			return err
		}
	}
	if value, ok := raw["log_level"]; ok {
		var level string
		if err := unmarshalHookNonNullField("log_level", value, &level); err != nil {
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
		if err := unmarshalHookNonNullField("redaction.externalize_images", value, &settings.ExternalizeImages); err != nil {
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
		if err := unmarshalHookNonNullField("redaction.pii.enabled", value, &settings.Enabled); err != nil {
			return err
		}
	}
	for key, destination := range map[string]**bool{
		"email": &settings.Email, "phone": &settings.Phone, "address": &settings.Address,
	} {
		if value, ok := raw[key]; ok {
			var enabled *bool
			if err := unmarshalHookField("redaction.pii."+key, value, &enabled); err != nil {
				return err
			}
			*destination = enabled
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
		*destination = values
		return nil
	}
	if values == nil {
		return nil
	}
	for label, pattern := range values {
		(*destination)[label] = pattern
	}
	return nil
}

func loadVerifiedPolicySettings(provenance repopolicy.SettingsProvenance, preferencesPath string) (*EntireSettings, error) {
	settings := &EntireSettings{Enabled: true}
	var err error
	if provenance.ProjectVerified {
		settings, err = loadSettingsData(provenance.ProjectData)
		if err != nil {
			return nil, fmt.Errorf("reading settings file: %w", err)
		}
	}
	if preferencesPath != "" {
		preferences, preferencesErr := loadClonePreferencesFromFile(preferencesPath)
		if preferencesErr != nil {
			return nil, fmt.Errorf("reading clone preferences file: %w", preferencesErr)
		}
		applyClonePreferences(settings, preferences)
	}
	if provenance.LocalVerified {
		if err := mergeJSON(settings, provenance.LocalData); err != nil {
			return nil, fmt.Errorf("merging local settings: %w", err)
		}
	} else if provenance.LocalPathSafe {
		settings.localLayerRejection = localLayerTrackedReason
	}
	enforceOPFCommandTrustForVerifiedData(settings, provenance.LocalData, provenance.LocalOPFVerified)
	if err := settings.SummaryGeneration.Validate(); err != nil {
		return nil, fmt.Errorf("merged settings invalid: %w", err)
	}
	if err := validateScannerSettings(settings); err != nil {
		return nil, fmt.Errorf("merged settings invalid: %w", err)
	}
	return settings, nil
}

func unmarshalHookField(name string, data json.RawMessage, destination any) error {
	if err := json.Unmarshal(data, destination); err != nil {
		return fmt.Errorf("parsing %s field: %w", name, err)
	}
	return nil
}

func unmarshalHookNonNullField(name string, data json.RawMessage, destination any) error {
	if bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		return fmt.Errorf("parsing %s field: null is not allowed", name)
	}
	return unmarshalHookField(name, data, destination)
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
