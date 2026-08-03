package settings

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/logging"
)

// ErrInvalidCheckpointsConfig is returned when a present "checkpoints" settings
// block is malformed (e.g. a backend with no type).
var ErrInvalidCheckpointsConfig = errors.New("invalid checkpoints config")

// Environment overrides for checkpoint backend selection. When EnvCheckpointsPrimary
// is set, it (and the optional comma-separated EnvCheckpointsMirrors) fully
// replaces any checkpoints block in settings — env wins over file, matching the
// other ENTIRE_* overrides (ENTIRE_LOG_LEVEL, ENTIRE_TOKEN, …). Primarily for
// driving e2e/CI and rollout against a specific backend without editing settings.
const (
	EnvCheckpointsPrimary = "ENTIRE_CHECKPOINTS_PRIMARY"
	EnvCheckpointsMirrors = "ENTIRE_CHECKPOINTS_MIRRORS"
)

// CheckpointsConfig selects checkpoint storage backends: one primary (source of
// truth, serves all reads and writes) and zero or more mirrors (independent
// backends that receive best-effort write fan-out). When absent, the checkpoint
// layer defaults to the built-in git-branch backend with no mirrors.
type CheckpointsConfig struct {
	Primary BackendConfig   `json:"primary"`
	Mirrors []BackendConfig `json:"mirrors,omitempty"`
}

// BackendConfig is a discriminated backend selector: Type names the registered
// backend and Config carries the backend-specific options block (opaque here,
// decoded by the backend factory).
type BackendConfig struct {
	Type   string          `json:"type"`
	Config json.RawMessage `json:"config,omitempty"`
}

// checkpointsEnvelope extracts only the "checkpoints" key, leaving every other
// settings field untouched so unrelated malformed/unknown settings cannot break
// checkpoint backend resolution.
type checkpointsEnvelope struct {
	Checkpoints json.RawMessage `json:"checkpoints"`
}

// LoadCheckpointsConfig reads the checkpoint backend selection from settings
// without the strict whole-settings validation that Load performs. It is
// deliberately fail-soft: a missing settings file, a whole-file JSON syntax
// error, or unrelated invalid fields all resolve to "no checkpoints config"
// (nil), so checkpoint construction falls back to the default git backend. It
// errors only when a "checkpoints" block is present but itself invalid.
//
// Precedence mirrors Load: a "checkpoints" block in settings.local.json
// replaces the one in settings.json wholesale (this is a selection config, not
// a deep-merged document). Clone preferences carry no checkpoint config and are
// not consulted.
func LoadCheckpointsConfig(ctx context.Context) (*CheckpointsConfig, error) {
	// Env override wins over any settings file (precedence like ENTIRE_LOG_LEVEL).
	if cfg, ok := checkpointsConfigFromEnv(); ok {
		if err := cfg.validate(); err != nil {
			return nil, err
		}
		return cfg, nil
	}

	base, local := checkpointsSettingsPaths(ctx)

	// "local replaces base wholesale": prefer a checkpoints block from local
	// settings; fall back to base only when local has none. We extract the raw
	// blocks fail-soft, then decode/validate just the one that wins — so a
	// malformed block in the overridden file never blocks the file that wins.
	raw, src := rawCheckpointsBlock(ctx, local), local
	if raw == nil {
		raw, src = rawCheckpointsBlock(ctx, base), base
	}
	if raw == nil {
		return nil, nil //nolint:nilnil // no checkpoints block present => default git backend
	}

	var cfg CheckpointsConfig
	dec := json.NewDecoder(bytes.NewReader(raw))
	// DisallowUnknownFields surfaces typos (e.g. "primry") instead of silently
	// ignoring them. The trade-off is that this CLI is not forward-compatible
	// with checkpoints fields added by a newer CLI: an unknown field errors here.
	// Adding a field is therefore a coordinated rollout — ship the reader before
	// any writer emits the field. The error below points users at that cause.
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("%w in %s: %w; an unrecognized field can also mean this file was written by a newer CLI — confirm you are on the latest version", ErrInvalidCheckpointsConfig, src, err)
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// rawCheckpointsBlock returns the raw "checkpoints" JSON block from filePath, or
// nil when the file is absent/unreadable, has a whole-file syntax error, or has
// no checkpoints block. It never errors: unrelated breakage in a settings file
// must not block checkpoint construction (the strict Load path surfaces it for
// normal commands).
func rawCheckpointsBlock(ctx context.Context, filePath string) json.RawMessage {
	data, err := readConfined(filePath)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			// A non-ENOENT read error (bad perms, settings.json is a directory or
			// an escaping symlink, etc.) is a broken/untrusted setup; stay
			// fail-soft so checkpoint construction defaults to git rather than
			// newly failing resume/explain/hooks.
			logging.Debug(ctx, "checkpoints config unreadable; defaulting to git backend",
				slog.String("path", filePath), slog.String("error", err.Error()))
		}
		return nil
	}

	var env checkpointsEnvelope
	if err := json.Unmarshal(data, &env); err != nil {
		// Whole-file parse failure is unrelated breakage; stay fail-soft.
		return nil
	}
	if len(env.Checkpoints) == 0 {
		return nil
	}
	return env.Checkpoints
}

// checkpointsConfigFromEnv builds a CheckpointsConfig from the environment when
// EnvCheckpointsPrimary is set. Mirrors are taken from EnvCheckpointsMirrors as a
// comma-separated list of backend types (no per-backend config blocks — the env
// override is for backend selection only). Returns ok=false when no primary is
// set, leaving file-based resolution in charge.
func checkpointsConfigFromEnv() (*CheckpointsConfig, bool) {
	primary := strings.TrimSpace(os.Getenv(EnvCheckpointsPrimary))
	if primary == "" {
		return nil, false
	}
	cfg := &CheckpointsConfig{Primary: BackendConfig{Type: primary}}
	for _, m := range strings.Split(os.Getenv(EnvCheckpointsMirrors), ",") {
		if t := strings.TrimSpace(m); t != "" {
			cfg.Mirrors = append(cfg.Mirrors, BackendConfig{Type: t})
		}
	}
	return cfg, true
}

func (c *CheckpointsConfig) validate() error {
	if c.Primary.Type == "" {
		return fmt.Errorf("%w: checkpoints.primary.type is required", ErrInvalidCheckpointsConfig)
	}
	for i, m := range c.Mirrors {
		if m.Type == "" {
			return fmt.Errorf("%w: checkpoints.mirrors[%d].type is required", ErrInvalidCheckpointsConfig, i)
		}
	}
	return nil
}

// checkpointsSettingsPaths resolves the base and local settings file paths the
// same way Load does (minus clone preferences, which carry no checkpoint config).
func checkpointsSettingsPaths(ctx context.Context) (base, local string) {
	if worktreeRoot, ok := worktreeRootFromContext(ctx); ok {
		return worktreeSettingsPaths(worktreeRoot)
	}
	return settingsAbsPaths(ctx)
}
