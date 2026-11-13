package settings

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"

	"github.com/entireio/cli/cmd/entire/cli/logging"
)

// syncPromptsEnvelope extracts only strategy_options.sync_prompts, leaving
// every other field (and its validity) to the strict Load path.
type syncPromptsEnvelope struct {
	StrategyOptions map[string]json.RawMessage `json:"strategy_options"`
}

// LoadPromptSyncDisabled resolves strategy_options.sync_prompts for checkpoint
// store construction. It is the narrow counterpart of Load used by
// checkpoint.Open, which must stay cheap (no git subprocesses) and must not
// fail on unrelated settings breakage, exactly like LoadCheckpointsConfig.
//
// Precedence mirrors Load: a sync_prompts key in settings.local.json wins over
// settings.json. Unlike Load, a tracked settings.local.json is not discarded
// here: the value can only withhold data, never authorize anything, so honoring
// it is the conservative reading.
//
// Because sync_prompts=false is a privacy opt-out, every state in which the
// answer cannot be determined fails closed (returns true, prompts withheld):
// an unreadable file, a whole-file syntax error, or a non-boolean value. The
// strict Load path reports those problems to normal commands. It errors only
// when the settings paths themselves cannot be resolved.
func LoadPromptSyncDisabled(ctx context.Context) (bool, error) {
	base, local, err := checkpointsSettingsPaths(ctx)
	if err != nil {
		return false, err
	}
	for _, path := range []string{local, base} {
		disabled, found := promptSyncDisabledIn(ctx, path)
		if found {
			return disabled, nil
		}
	}
	return false, nil
}

// promptSyncDisabledIn reads sync_prompts from one settings file. found is
// false only when the file is absent or definitely has no sync_prompts key.
func promptSyncDisabledIn(ctx context.Context, filePath string) (disabled, found bool) {
	data, err := readConfined(filePath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, false
		}
		logging.Warn(ctx, "settings unreadable; withholding prompts from checkpoints",
			slog.String("path", filePath), slog.String("error", err.Error()))
		return true, true
	}

	var env syncPromptsEnvelope
	if err := json.Unmarshal(data, &env); err != nil {
		logging.Warn(ctx, "settings unparseable; withholding prompts from checkpoints",
			slog.String("path", filePath), slog.String("error", err.Error()))
		return true, true
	}
	raw, ok := env.StrategyOptions[SyncPromptsOptionKey]
	if !ok {
		return false, false
	}
	var value bool
	if err := json.Unmarshal(raw, &value); err != nil {
		logging.Warn(ctx, "strategy_options.sync_prompts is not a boolean; withholding prompts from checkpoints",
			slog.String("path", filePath))
		return true, true
	}
	return !value, true
}
