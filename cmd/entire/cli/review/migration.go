package review

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"

	"github.com/entireio/cli/cmd/entire/cli/logging"
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

	// Skip the prompt entirely if the user has already declined. Without this,
	// teams who intentionally commit review prefs would be re-prompted on
	// every invocation of `entire review`.
	prefs, prefsErr := settings.LoadClonePreferences(ctx)
	if prefsErr != nil {
		return fmt.Errorf("load review preferences for migration: %w", prefsErr)
	}
	if prefs != nil && prefs.ReviewMigrationDismissed {
		return nil
	}

	// Bail before prompting if .entire/settings.local.json already has review
	// keys. settings.local.json overrides clone-local preferences (mergeJSON
	// wholesale-replaces the review map), so migrating without cleaning the
	// local file first would silently nullify the migration on the very next
	// settings.Load — the user clicks "yes", their config moves to clone
	// prefs, then the local override hides it. Better to surface the
	// precondition up front than to leave the user wondering why their
	// migrated config disappeared.
	//
	// Intentionally does NOT set ReviewMigrationDismissed: this is a fixable
	// precondition, not a user-rejected migration; the prompt should fire
	// again on the next run after the user cleans settings.local.json.
	if localHas, localPath, localErr := localSettingsHasReviewKeys(ctx); localErr != nil {
		return fmt.Errorf("inspect local settings for migration: %w", localErr)
	} else if localHas {
		fmt.Fprintln(errOut, "Cannot migrate review preferences: .entire/settings.local.json also has review keys.")
		fmt.Fprintf(errOut, "Those override clone-local preferences and would mask the migration. Remove the\n")
		fmt.Fprintf(errOut, "`review` / `review_fix_agent` keys from %s, then re-run `entire review`.\n", localPath)
		return nil
	}

	if !canPrompt {
		// Log at Warn so operators tailing .entire/logs/ catch the pending
		// migration on scripted/CI invocations where the stderr hint may
		// scroll past unnoticed.
		logging.Warn(ctx, "review migration pending: project settings has review keys that may be committed",
			slog.String("project_settings_path", project.path),
			slog.Bool("has_review", project.hasReview),
			slog.Bool("has_fix_agent", project.hasFixAgent))
		fmt.Fprintln(errOut, "Review preferences are stored in project settings (.entire/settings.json).")
		fmt.Fprintln(errOut, "These are typically committed and may be visible to teammates.")
		fmt.Fprintln(errOut, "Run `entire review --edit` interactively to move them to clone-local preferences.")
		return nil
	}

	if promptYN == nil {
		promptYN = realPromptYN
	}
	migrate, err := promptYN(ctx, "Review preferences are stored in project settings (.entire/settings.json), which is typically committed. Move them to clone-local preferences so they stay private?", false)
	if err != nil {
		return fmt.Errorf("review settings migration prompt: %w", err)
	}
	if !migrate {
		if prefs == nil {
			prefs = &settings.ClonePreferences{}
		}
		prefs.ReviewMigrationDismissed = true
		if err := settings.SaveClonePreferences(ctx, prefs); err != nil {
			return fmt.Errorf("save migration dismissal: %w", err)
		}
		return nil
	}

	moved, err := migrateProjectReviewSettings(ctx, project)
	if err != nil {
		return err
	}
	if moved {
		fmt.Fprintln(out, "Moved review preferences from project settings to clone-local preferences.")
	} else {
		fmt.Fprintln(out, "Removed unused review keys from project settings; nothing to move.")
	}
	return nil
}

func loadProjectReviewSettings(ctx context.Context) (*projectReviewSettings, bool, error) {
	path, raw, exists, err := settings.LoadProjectRaw(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("review migration: %w", err)
	}
	if !exists {
		return nil, false, nil
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

// migrateProjectReviewSettings copies review keys from the project settings
// file into clone-local preferences and strips them from the project file.
//
// Returns moved=true when any review data was copied into prefs. When the
// project file's review keys are empty/null (or fully conflict with existing
// prefs, which is rejected upstream), moved=false but the project keys are
// still stripped as cleanup.
//
// Write ordering: prefs are saved first (atomic), then the project file is
// rewritten (atomic). Both writes use temp-then-rename so a crash mid-write
// leaves the original file intact rather than truncated. If the project
// rewrite fails after the prefs write succeeded, prefs precedence covers
// the gap until the next run.
func migrateProjectReviewSettings(ctx context.Context, project *projectReviewSettings) (moved bool, err error) {
	if project == nil {
		return false, nil
	}

	prefs, err := settings.LoadClonePreferences(ctx)
	if err != nil {
		return false, fmt.Errorf("load review preferences for migration: %w", err)
	}
	if prefs == nil {
		prefs = &settings.ClonePreferences{}
	}

	preferencesChanged := false
	if project.hasReview && !isJSONNull(project.review) {
		var projectReview map[string]settings.ReviewConfig
		if err := json.Unmarshal(project.review, &projectReview); err != nil {
			return false, fmt.Errorf("parsing project review settings: %w", err)
		}
		if len(projectReview) > 0 {
			merged, mergedOK, conflicts := mergeProjectReviewIntoPrefs(prefs.Review, projectReview)
			if len(conflicts) > 0 {
				return false, fmt.Errorf(
					"review settings exist in both %s and clone-local preferences for agent(s) %v; "+
						"reconcile manually by removing the redundant keys from %s, then re-run `entire review`",
					project.path, conflicts, project.path,
				)
			}
			if mergedOK {
				prefs.Review = merged
				preferencesChanged = true
			}
		}
	}
	if project.hasFixAgent && !isJSONNull(project.fixAgent) {
		var fixAgent string
		if err := json.Unmarshal(project.fixAgent, &fixAgent); err != nil {
			return false, fmt.Errorf("parsing project review_fix_agent: %w", err)
		}
		if fixAgent != "" {
			if prefs.ReviewFixAgent != "" && prefs.ReviewFixAgent != fixAgent {
				return false, fmt.Errorf(
					"review_fix_agent differs between %s (%q) and clone-local preferences (%q); "+
						"reconcile manually by removing review_fix_agent from %s, then re-run `entire review`",
					project.path, fixAgent, prefs.ReviewFixAgent, project.path,
				)
			}
			if prefs.ReviewFixAgent == "" {
				prefs.ReviewFixAgent = fixAgent
				preferencesChanged = true
			}
		}
	}

	if preferencesChanged {
		if err := settings.SaveClonePreferences(ctx, prefs); err != nil {
			return false, fmt.Errorf("save review preferences for migration: %w", err)
		}
	}

	delete(project.raw, "review")
	delete(project.raw, "review_fix_agent")
	if err := settings.SaveProjectRaw(project.path, project.raw); err != nil {
		return false, fmt.Errorf("save project settings after review migration: %w", err)
	}
	return preferencesChanged, nil
}

// mergeProjectReviewIntoPrefs merges projectReview into the current prefs map.
// Per-agent conflicts (same key, different value) are surfaced rather than
// silently resolved — the caller can then refuse the migration with a clear
// message. Non-overlapping entries are merged. Returns ok=false when nothing
// would change (prefs already had every project entry verbatim).
func mergeProjectReviewIntoPrefs(prefs, projectReview map[string]settings.ReviewConfig) (merged map[string]settings.ReviewConfig, ok bool, conflicts []string) {
	merged = make(map[string]settings.ReviewConfig, len(prefs)+len(projectReview))
	for k, v := range prefs {
		merged[k] = v
	}
	changed := false
	for k, projectV := range projectReview {
		if existing, present := merged[k]; present {
			if !reviewConfigEqual(existing, projectV) {
				conflicts = append(conflicts, k)
			}
			continue
		}
		merged[k] = projectV
		changed = true
	}
	if len(conflicts) > 0 {
		return nil, false, conflicts
	}
	return merged, changed, nil
}

func reviewConfigEqual(a, b settings.ReviewConfig) bool {
	if a.Prompt != b.Prompt {
		return false
	}
	if len(a.Skills) != len(b.Skills) {
		return false
	}
	for i := range a.Skills {
		if a.Skills[i] != b.Skills[i] {
			return false
		}
	}
	return true
}

func isJSONNull(raw json.RawMessage) bool {
	return bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}

// localSettingsHasReviewKeys reports whether .entire/settings.local.json
// exists and contains either a "review" or "review_fix_agent" key. Both keys
// override clone-local preferences via mergeJSON's wholesale-replace path,
// so the migration must surface their presence rather than silently produce
// a state where the migrated config never takes effect.
//
// Returns the absolute path of the local settings file too, so callers can
// quote the exact location in the warning they show the user.
func localSettingsHasReviewKeys(ctx context.Context) (has bool, path string, err error) {
	path, raw, exists, loadErr := settings.LoadLocalRaw(ctx)
	if loadErr != nil {
		return false, path, fmt.Errorf("local settings review-keys check: %w", loadErr)
	}
	if !exists {
		return false, path, nil
	}
	_, hasReview := raw["review"]
	_, hasFixAgent := raw["review_fix_agent"]
	return hasReview || hasFixAgent, path, nil
}
