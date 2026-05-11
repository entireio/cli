package review

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/entireio/cli/cmd/entire/cli/jsonutil"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/settings"
)

type projectReviewSettings struct {
	path        string
	raw         map[string]json.RawMessage
	review      json.RawMessage
	fixAgent    json.RawMessage
	hasReview   bool
	hasFixAgent bool
}

func maybePromptReviewSettingsMigration(
	ctx context.Context,
	out io.Writer,
	errOut io.Writer,
	canPrompt bool,
	promptYN func(context.Context, string, bool) (bool, error),
) error {
	project, ok, err := loadProjectReviewSettings(ctx)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}

	if !canPrompt {
		fmt.Fprintln(errOut, "Review preferences are stored in project settings (.entire/settings.json). Run `entire review --edit` interactively to move them to local preferences.")
		return nil
	}

	if promptYN == nil {
		promptYN = realPromptYN
	}
	migrate, err := promptYN(ctx, "Review preferences are stored in project settings (.entire/settings.json). Move them to local preferences now?", false)
	if err != nil {
		return fmt.Errorf("review settings migration prompt: %w", err)
	}
	if !migrate {
		return nil
	}

	if err := migrateProjectReviewSettings(ctx, project); err != nil {
		return err
	}
	fmt.Fprintln(out, "Moved review preferences from project settings to local preferences.")
	return nil
}

func loadProjectReviewSettings(ctx context.Context) (*projectReviewSettings, bool, error) {
	path, err := paths.AbsPath(ctx, settings.EntireSettingsFile)
	if err != nil {
		path = settings.EntireSettingsFile
	}

	data, err := os.ReadFile(path) //nolint:gosec // path is resolved from repo settings path
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("reading project settings for review migration: %w", err)
	}

	raw := map[string]json.RawMessage{}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, false, fmt.Errorf("parsing project settings for review migration: %w", err)
	}

	reviewRaw, hasReview := raw["review"]
	fixAgentRaw, hasFixAgent := raw["review_fix_agent"]
	if !hasReview && !hasFixAgent {
		return nil, false, nil
	}
	return &projectReviewSettings{
		path:        path,
		raw:         raw,
		review:      reviewRaw,
		fixAgent:    fixAgentRaw,
		hasReview:   hasReview,
		hasFixAgent: hasFixAgent,
	}, true, nil
}

func migrateProjectReviewSettings(ctx context.Context, project *projectReviewSettings) error {
	if project == nil {
		return nil
	}

	prefs, err := settings.LoadClonePreferences(ctx)
	if err != nil {
		return fmt.Errorf("load review preferences for migration: %w", err)
	}
	if prefs == nil {
		prefs = &settings.ClonePreferences{}
	}

	preferencesChanged := false
	if project.hasReview && len(prefs.Review) == 0 && !isJSONNull(project.review) {
		var review map[string]settings.ReviewConfig
		if err := json.Unmarshal(project.review, &review); err != nil {
			return fmt.Errorf("parsing project review settings: %w", err)
		}
		if len(review) > 0 {
			prefs.Review = review
			preferencesChanged = true
		}
	}
	if project.hasFixAgent && prefs.ReviewFixAgent == "" && !isJSONNull(project.fixAgent) {
		var fixAgent string
		if err := json.Unmarshal(project.fixAgent, &fixAgent); err != nil {
			return fmt.Errorf("parsing project review_fix_agent: %w", err)
		}
		if fixAgent != "" {
			prefs.ReviewFixAgent = fixAgent
			preferencesChanged = true
		}
	}

	if preferencesChanged {
		if err := settings.SaveClonePreferences(ctx, prefs); err != nil {
			return fmt.Errorf("save review preferences for migration: %w", err)
		}
	}

	delete(project.raw, "review")
	delete(project.raw, "review_fix_agent")
	data, err := jsonutil.MarshalIndentWithNewline(project.raw, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal project settings after review migration: %w", err)
	}
	//nolint:gosec // G306: settings file is config, not secrets; 0o644 is appropriate
	if err := os.WriteFile(project.path, data, 0o644); err != nil {
		return fmt.Errorf("write project settings after review migration: %w", err)
	}
	return nil
}

func isJSONNull(raw json.RawMessage) bool {
	return bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}
