package cli

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/entiredir"
	"github.com/entireio/cli/cmd/entire/cli/osroot"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/runnerdefaults"

	"charm.land/huh/v2"
)

// createDefaultRunners writes the default runner set, so setup doubles as
// onboarding. It returns the IDs it created, so the caller can flag any that
// tailoring then leaves un-tailored.
//
// Two things it deliberately does not do. It does not ask — consent belongs to
// the mode decision in resolveRunnerSetupMode, so one answer covers every
// prompt the command can raise, and runnerSetupMode.writesRunnerFiles is what
// gates the call. And it does not re-check whether runners already exist: the
// caller has that answer already, and deriving it twice gave the invariant two
// homes that could drift.
func createDefaultRunners(w io.Writer, repoRoot string) (created []string, err error) {
	dir := runnersDir(repoRoot)

	defaults, err := runnerdefaults.Files()
	if err != nil {
		return nil, fmt.Errorf("loading default runners: %w", err)
	}

	root, err := entiredir.OpenAt(repoRoot)
	if err != nil {
		return nil, fmt.Errorf("creating %s: %w", dir, err)
	}
	if err := osroot.MkdirAllNoSymlink(root, runnersName, 0o755); err != nil {
		return nil, fmt.Errorf("creating %s: %w", dir, err)
	}
	for _, f := range defaults {
		dest := filepath.Join(dir, f.Name)
		if err := entiredir.WriteFile(root, runnersName+"/"+f.Name, f.Data, 0o644); err != nil {
			return nil, fmt.Errorf("writing %s: %w", dest, err)
		}
		fmt.Fprintf(w, "created %s\n", filepath.Join(paths.EntireDir, runnersName, f.Name))
		created = append(created, strings.TrimSuffix(f.Name, ".json"))
	}
	return created, nil
}

// runnerConfigsExist reports whether the repo already has runner configs. It
// reads through the shared .entire root like every other .entire access; a
// missing root or directory simply means none.
func runnerConfigsExist(repoRoot string) bool {
	root, err := entiredir.OpenAtForRead(repoRoot)
	if err != nil {
		return false
	}
	entries, err := osroot.ReadDirNoSymlinks(root, runnersName)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".json") {
			return true
		}
	}
	return false
}

// chooseRunnerSetupAction asks a terminal what setup should do. The choice is
// the user's own framing of the command — generic defaults, or defaults
// tailored to this repo — and it is asked once, whether or not the repo already
// has runners, so that the answer settles both creating and tailoring.
// Cancelling the form yields setupModeNone, which authorizes nothing.
func chooseRunnerSetupAction(ctx context.Context, errW io.Writer, haveRunners bool) (runnerSetupMode, error) {
	title := "Runners are already configured for this repo. Tailor them to this repo now?"
	adaptLabel := "Tailor them to this repo"
	keepLabel := "Leave them as they are"
	if !haveRunners {
		defaults, err := runnerdefaults.Files()
		if err != nil {
			return setupModeNone, fmt.Errorf("loading default runners: %w", err)
		}
		title = fmt.Sprintf("No trail runners found. What should setup do? (%d runners)", len(defaults))
		adaptLabel = "Create the defaults and tailor them to this repo"
		keepLabel = "Create the generic defaults only"
	}

	mode := setupModeAdapt
	form := NewAccessibleForm(
		huh.NewGroup(
			huh.NewSelect[runnerSetupMode]().
				Title(title).
				Description("Tailoring rewrites each runner's prompt from this repo's docs, history and past findings — one call to your configured summary provider.").
				Options(
					huh.NewOption(adaptLabel, setupModeAdapt),
					huh.NewOption(keepLabel, setupModeDefaults),
				).
				Value(&mode),
		),
	)
	// RunWithContext, not Run: the command's context must be able to abort the
	// form, and it is what puts context.Canceled in reach of the handler.
	if err := form.RunWithContext(ctx); err != nil {
		return setupModeNone, handleFormCancellation(errW, "Runner setup", err)
	}
	return mode, nil
}
