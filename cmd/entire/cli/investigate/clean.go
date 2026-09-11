package investigate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
)

// CleanInput drives RunClean.
type CleanInput struct {
	// RunID, when non-empty, targets one run via exact-match-then-
	// unique-prefix. Ignored when All is true.
	RunID string

	// All targets every investigation found by the manifest store.
	All bool

	// Force skips the confirmation prompt.
	Force bool

	// Out / ErrOut sink the operator-facing output.
	Out    io.Writer
	ErrOut io.Writer
}

// CleanDeps is what RunClean needs that's test-injectable.
//
// The two removal hooks are deletions, not paths. They used to be functions
// returning an absolute path that RunClean handed to os.RemoveAll and os.Remove,
// which put the most destructive operation in the package on the far side of a
// string — exactly what StateStore.RunDir's doc comment warns about. Each store
// now performs its own delete through its own root, so a run id resolves as a
// name inside the git common dir instead of being joined into a path.
type CleanDeps struct {
	ManifestStore *LocalManifestStore
	// RemoveRun deletes the per-run directory for a given run id. In
	// production this is StateStore.RemoveRun; tests inject a fake.
	RemoveRun func(runID string) error
	// RemoveManifest deletes a manifest record. In production this is
	// LocalManifestStore.Remove.
	RemoveManifest func(m LocalManifest) error
	// Confirm prompts the user with the given message and returns the
	// y/N answer. Nil → real huh-backed prompt (use newAccessibleForm).
	Confirm func(ctx context.Context, message string) (bool, error)
}

// RunClean implements `entire investigate clean`.
func RunClean(ctx context.Context, in CleanInput, deps CleanDeps) error {
	if deps.ManifestStore == nil || deps.RemoveRun == nil || deps.RemoveManifest == nil {
		return errors.New("clean: deps not wired (manifest store, RemoveRun, RemoveManifest required)")
	}
	if in.RunID == "" && !in.All {
		return errors.New("clean: pass a run id (or unique prefix) or --all")
	}

	manifests, err := deps.ManifestStore.List(ctx)
	if err != nil {
		return fmt.Errorf("list manifests: %w", err)
	}
	if len(manifests) == 0 {
		fmt.Fprintln(in.Out, "No local investigations found.")
		return nil
	}

	targets, err := selectCleanTargets(manifests, in.RunID, in.All)
	if err != nil {
		return err
	}

	if !in.Force {
		printCleanSummary(in.Out, targets, in.All)
		confirm := deps.Confirm
		if confirm == nil {
			confirm = realConfirm
		}
		ok, confirmErr := confirm(ctx, "Proceed?")
		if confirmErr != nil {
			return fmt.Errorf("confirmation prompt: %w", confirmErr)
		}
		if !ok {
			fmt.Fprintln(in.Out, "Aborted.")
			return nil
		}
	}

	var deleted, failed int
	for _, m := range targets {
		if err := deleteOneInvestigation(m, deps); err != nil {
			failed++
			fmt.Fprintf(in.ErrOut, "warn: %s: %v\n", m.RunID, err)
			continue
		}
		deleted++
	}
	fmt.Fprintf(in.Out, "Deleted %d investigation(s)", deleted)
	if failed > 0 {
		fmt.Fprintf(in.Out, " (%d failed)", failed)
	}
	fmt.Fprintln(in.Out, ".")
	return nil
}

// selectCleanTargets resolves the manifest list to the target set.
// For --all, returns every manifest. For a run id (or prefix), defers
// to ResolveByRunID for exact-then-prefix matching.
func selectCleanTargets(manifests []LocalManifest, runID string, all bool) ([]LocalManifest, error) {
	if all {
		return manifests, nil
	}
	return ResolveByRunID(manifests, runID)
}

// printCleanSummary lists targets before the confirmation prompt.
func printCleanSummary(w io.Writer, targets []LocalManifest, all bool) {
	switch {
	case all:
		fmt.Fprintf(w, "This will delete ALL investigations (%d):\n", len(targets))
	case len(targets) == 1:
		fmt.Fprintln(w, "This will delete:")
	default:
		fmt.Fprintf(w, "This will delete %d investigations:\n", len(targets))
	}
	sorted := append([]LocalManifest(nil), targets...)
	sort.SliceStable(sorted, func(i, j int) bool {
		return sorted[i].StartedAt.After(sorted[j].StartedAt)
	})
	for _, m := range sorted {
		prompt := m.Topic
		if prompt == "" {
			prompt = "(no prompt)"
		}
		fmt.Fprintf(w, "  %s  %s\n", m.RunID, prompt)
	}
}

// deleteOneInvestigation removes a manifest + its per-run dir. Missing
// files / dirs are treated as a successful no-op so that calling clean
// against a partial state (e.g. previous interrupted cleanup) still
// converges. Errors aggregate so the caller can decide whether to keep
// going.
//
// Delete order: per-run dir first, manifest last. A failure removing the
// run dir leaves the manifest as a recoverable breadcrumb so a subsequent
// `clean` invocation can find and retry the same target — the reverse
// order would orphan a state.json with no manifest record.
func deleteOneInvestigation(m LocalManifest, deps CleanDeps) error {
	var errs []string

	if err := deps.RemoveRun(m.RunID); err != nil {
		// Both hooks treat an absent target as success, so anything reported
		// here is a real failure (permissions, a non-empty parent, etc.).
		errs = append(errs, fmt.Sprintf("run dir: %v", err))
	}

	if err := deps.RemoveManifest(m); err != nil {
		errs = append(errs, fmt.Sprintf("manifest: %v", err))
	}

	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	return nil
}

// realConfirm is the production y/N prompt for the clean confirmation.
func realConfirm(ctx context.Context, message string) (bool, error) {
	return realPromptYN(ctx, message, false)
}
