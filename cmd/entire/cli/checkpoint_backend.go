package cli

import (
	"context"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/settings"
)

// Friendly aliases for the two selectable checkpoint backends. Users type these
// on --checkpoint-backend; they map to the canonical backend types stored in
// settings (checkpoint.BackendTypeGitBranch / checkpoint.BackendTypeGitRefs).
const (
	checkpointBackendBranchAlias = "branch"
	checkpointBackendRefsAlias   = "refs"
)

// checkpointBackendFlagUsage is the shared --checkpoint-backend help text. Both
// `enable` and `configure` register the flag, so it lives here rather than being
// spelled twice: the two commands drifting would advertise two different stories
// about which backend a repo gets.
//
// It names refs as the default rather than merely "recommended", because that is
// what a first-time `entire enable` writes, with no prompt (see
// resolveFirstRunCheckpointBackend). branch is labelled legacy: it is an explicit
// escape hatch for interoperating with older clients, not a co-equal choice.
const checkpointBackendFlagUsage = "Checkpoint storage backend: " +
	checkpointBackendRefsAlias + " (default; one git ref per checkpoint) or " +
	checkpointBackendBranchAlias + " (legacy shared entire/checkpoints/v1 branch)"

// resolveCheckpointBackendType maps a user-facing backend name to the canonical
// settings backend type and validates it may serve as the primary. It accepts
// the friendly aliases "branch"/"refs" and the canonical "git-branch"/"git-refs"
// (case-insensitive). An unknown or non-git-backed value is rejected via the
// checkpoint registry, so the error text stays in sync with the backend list.
func resolveCheckpointBackendType(name string) (string, error) {
	typ := strings.ToLower(strings.TrimSpace(name))
	switch typ {
	case checkpointBackendBranchAlias:
		typ = checkpoint.BackendTypeGitBranch
	case checkpointBackendRefsAlias:
		typ = checkpoint.BackendTypeGitRefs
	}
	if err := checkpoint.ValidatePrimaryBackend(typ); err != nil {
		return "", fmt.Errorf("invalid --%s: %w", flagCheckpointBackend, err)
	}
	return typ, nil
}

// applyCheckpointBackend sets the primary checkpoint backend on settings,
// preserving any existing mirrors except one whose type would collide with the
// new primary (the one-of-each-type topology rule enforced in checkpoint.Open).
// Switching the primary on an existing repo is safe: new checkpoints use the new
// backend while read routing keeps prior checkpoints readable in their original
// format.
func applyCheckpointBackend(s *EntireSettings, typ string) {
	cfg := s.Checkpoints
	if cfg == nil {
		cfg = &settings.CheckpointsConfig{}
	}
	cfg.Primary = settings.BackendConfig{Type: typ}
	cfg.Mirrors = slices.DeleteFunc(cfg.Mirrors, func(m settings.BackendConfig) bool {
		return m.Type == typ
	})
	s.Checkpoints = cfg
}

// applyCheckpointBackendFlag resolves and applies a --checkpoint-backend value to
// settings when it is non-empty; a no-op otherwise. Used by the fresh-repo enable
// paths (interactive setup and --agent), which mutate an in-memory settings
// object before their own save. Existing-repo enable and configure use
// updateCheckpointBackend instead.
func applyCheckpointBackendFlag(s *EntireSettings, backend string) error {
	if backend == "" {
		return nil
	}
	typ, err := resolveCheckpointBackendType(backend)
	if err != nil {
		return err
	}
	applyCheckpointBackend(s, typ)
	return nil
}

// updateCheckpointBackend persists opts.CheckpointBackend to the target settings
// file. Used by `entire configure` and by `entire enable` on repos that are
// already set up (both operate on an on-disk file rather than the in-memory
// settings the fresh-setup flow builds).
func updateCheckpointBackend(ctx context.Context, w io.Writer, opts EnableOptions) error {
	typ, err := resolveCheckpointBackendType(opts.CheckpointBackend)
	if err != nil {
		return err
	}

	targetFile, configDisplay := settingsTargetFile(ctx, opts.UseLocalSettings, opts.UseProjectSettings)
	targetFileAbs, err := paths.AbsPath(ctx, targetFile)
	if err != nil {
		targetFileAbs = targetFile
	}

	s, err := settings.LoadFromFile(targetFileAbs)
	if err != nil {
		return fmt.Errorf("failed to load settings: %w", err)
	}

	applyCheckpointBackend(s, typ)

	if err := saveSettingsToTarget(ctx, s, targetFile); err != nil {
		return fmt.Errorf("failed to save settings: %w", err)
	}

	fmt.Fprintf(w, "✓ Checkpoint backend set to %s (%s)\n", typ, configDisplay)
	return nil
}
