package cli

import (
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/entiredir"
	"github.com/entireio/cli/cmd/entire/cli/interactive"
	"github.com/entireio/cli/cmd/entire/cli/osroot"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/runnerdefaults"

	"charm.land/huh/v2"
)

// ensureRunnersPresent scaffolds the default runner set when a repo has none
// yet, so `tune` doubles as onboarding. It returns the IDs it created (nil when
// runners already existed) so the caller can flag any that tuning then leaves
// un-tailored. It is a no-op when runners already exist, and errors when the
// user declined or creation failed. Writing is gated on confirmation
// (interactive prompt, or the --yes flag for non-interactive runs).
func ensureRunnersPresent(w, errW io.Writer, repoRoot string, assumeYes bool) (created []string, err error) {
	dir := runnersDir(repoRoot)
	if runnerConfigsExist(repoRoot) {
		return nil, nil
	}

	defaults, err := runnerdefaults.Files()
	if err != nil {
		return nil, fmt.Errorf("loading default runners: %w", err)
	}

	if !assumeYes {
		if !interactive.CanPromptInteractively() {
			return nil, fmt.Errorf("no runner configs found under %s; re-run with --yes to create the default set (%d runners)", dir, len(defaults))
		}
		confirmed, err := confirmCreateRunners(len(defaults))
		if err != nil {
			return nil, err
		}
		if !confirmed {
			return nil, errors.New("no runner configs created (declined)")
		}
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
	fmt.Fprintf(errW, "Created %d default runner(s); tailoring them to this repo…\n", len(defaults))
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
	entries, err := osroot.ReadDir(root, runnersName)
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

func confirmCreateRunners(n int) (bool, error) {
	var ok bool
	form := NewAccessibleForm(
		huh.NewGroup(
			huh.NewConfirm().
				Title(fmt.Sprintf("No trail runners found. Create the default set (%d runners) in .entire/runners/?", n)).
				Description("Written from the built-in defaults, then tailored to this repo.").
				Value(&ok),
		),
	)
	if err := form.Run(); err != nil {
		return false, fmt.Errorf("runner-creation prompt cancelled: %w", err)
	}
	return ok, nil
}

// confirmTuneExisting asks whether to re-tailor runners that already exist —
// the re-run case where there is nothing to scaffold.
func confirmTuneExisting() (bool, error) {
	var ok bool
	form := NewAccessibleForm(
		huh.NewGroup(
			huh.NewConfirm().
				Title("Runners are already configured for this repo. Tune them to this repo now?").
				Description("Re-tailors the runner prompts using fresh repository signal.").
				Value(&ok),
		),
	)
	if err := form.Run(); err != nil {
		return false, fmt.Errorf("runner-tune prompt cancelled: %w", err)
	}
	return ok, nil
}
