package cli

import (
	"context"
	"fmt"
	"io"
	"slices"
	"strings"

	"charm.land/huh/v2"

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

// promptCheckpointBackend asks the user to choose a checkpoint storage backend
// during first-time interactive setup. The default is the git-branch backend;
// the git-refs backend is offered as the selectable alternative. It returns the
// canonical backend type, or "" when the user kept the default (or cancelled) so
// the caller can skip writing a redundant config block. Callers must gate this
// on an interactive terminal.
//
// Cancellation (Ctrl+C or a cancelled ctx) is treated like keeping the default:
// it prints a "cancelled" line and returns ("", nil) so enable continues with
// the default backend, matching the optional-prompt behavior elsewhere in setup.
func promptCheckpointBackend(ctx context.Context, w io.Writer) (string, error) {
	choice := checkpoint.BackendTypeGitBranch
	form := NewAccessibleForm(
		huh.NewGroup(
			huh.NewSelect[string]().
				Title("Checkpoint storage backend").
				Description("How Entire stores committed session checkpoints in your repo.").
				Options(
					huh.NewOption("Branch — one shared branch, entire/checkpoints/v1 (default)", checkpoint.BackendTypeGitBranch),
					huh.NewOption("Refs — one git ref per checkpoint", checkpoint.BackendTypeGitRefs),
				).
				Value(&choice),
		),
	)
	if err := form.RunWithContext(ctx); err != nil {
		return "", handleFormCancellation(w, "Checkpoint backend selection", err)
	}
	if choice == checkpoint.BackendTypeGitBranch {
		return "", nil
	}
	return choice, nil
}
