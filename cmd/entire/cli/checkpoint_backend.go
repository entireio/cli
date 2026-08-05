package cli

import (
	"context"
	"errors"
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

// checkpointBackendChoices returns the storage picker's options — git-refs
// first, labeled recommended — and the recommended value the caller
// pre-selects.
// Split from promptCheckpointBackend so the ordering/labeling contract is
// unit-testable without a TTY.
func checkpointBackendChoices() (opts []huh.Option[string], recommended string) {
	return []huh.Option[string]{
		huh.NewOption("Refs — one git ref per checkpoint (recommended)", checkpoint.BackendTypeGitRefs),
		huh.NewOption("Branch — one shared branch, entire/checkpoints/v1", checkpoint.BackendTypeGitBranch),
	}, checkpoint.BackendTypeGitRefs
}

// promptCheckpointBackend asks the user to choose a checkpoint storage backend
// during first-time interactive setup, with the git-refs backend pre-selected
// as the recommendation — most users should just press Enter. It returns the
// chosen canonical backend type; cancellation (Ctrl+C or a cancelled ctx)
// prints a cancellation note and returns "" (a soft skip, nil error, like
// other setup prompts) so the caller falls through to the recommended
// default. Callers must gate this on
// an interactive terminal (and skip it when ENTIRE_CHECKPOINTS_PRIMARY is
// active — the env fully replaces settings, so an answer could not take
// effect and would only write diverging config).
//
// Package-level var so tests can substitute it — the real prompt needs a TTY
// that `go test` doesn't have.
var promptCheckpointBackend = func(ctx context.Context, w io.Writer) (string, error) {
	opts, recommended := checkpointBackendChoices()
	choice := recommended
	form := NewAccessibleForm(
		huh.NewGroup(
			huh.NewSelect[string]().
				Title("Checkpoint storage").
				Description("How Entire stores committed session checkpoints in your repo.").
				Options(opts...).
				Value(&choice),
		),
	)
	if err := form.RunWithContext(ctx); err != nil {
		if errors.Is(err, huh.ErrUserAborted) || errors.Is(err, context.Canceled) {
			// Cancelling the storage question cancels `entire enable`; it must
			// not silently adopt a default and march on to later prompts. No
			// settings are persisted until after this prompt, so re-running
			// `entire enable` resumes here. Deliberately NOT a soft
			// handleFormCancellation skip like the optional prompts elsewhere.
			return "", errSetupCancelled
		}
		return "", handleFormCancellation(w, "Checkpoint storage selection", err)
	}
	return choice, nil
}

// errSetupCancelled marks a first-run setup prompt the user cancelled (Ctrl+C).
// It propagates up through runEnableInteractive so `entire enable` stops
// instead of persisting a default and continuing; runSetupFlow turns it into a
// friendly "run enable again to resume" line and a SilentError so the process
// exits cleanly. Because no settings are written until after the storage
// prompt, the next `entire enable` is still a fresh run and lands back here.
var errSetupCancelled = errors.New("setup cancelled")

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
