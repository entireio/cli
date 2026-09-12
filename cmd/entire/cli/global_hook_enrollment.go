package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"charm.land/huh/v2"
	"github.com/entireio/cli/cmd/entire/cli/agent/globalhooks"
	"github.com/entireio/cli/cmd/entire/cli/interactive"
	"github.com/entireio/cli/cmd/entire/cli/uiform"
)

const globalHookRestartNotice = "Upgrade every Entire installation used by hooks and restart all agent hosts after changing hook scope."

func chooseGlobalHookInstallation(ctx context.Context) (bool, error) {
	if !interactive.CanPromptInteractively() {
		return false, nil
	}
	var action string
	form := uiform.New(huh.NewGroup(huh.NewSelect[string]().
		Title("Agent integrations").
		Options(huh.NewOption("Manage agents in this repository", "repo"), huh.NewOption("Select this installation for global hooks", "global")).
		Value(&action)))
	if err := form.RunWithContext(ctx); err != nil {
		return false, fmt.Errorf("select agent action: %w", err)
	}
	return action == "global", nil
}

func enrollGlobalHookInstallation(ctx context.Context, w io.Writer, executable func() (string, error), confirm func(context.Context, string, bool) (bool, error)) error {
	path, err := executable()
	if err != nil {
		return fmt.Errorf("resolve this installation: %w", err)
	}
	selected, err := globalhooks.New(path)
	if err != nil {
		return fmt.Errorf("select global hook installation: %w", err)
	}
	previous, previousErr := globalhooks.Load()
	if previousErr != nil && !errors.Is(previousErr, os.ErrNotExist) {
		fmt.Fprintf(w, "Previous selection is unavailable: %v\n", previousErr)
	}
	if previous.Executable != "" {
		fmt.Fprintf(w, "Previously selected installation: %s\n", previous.Executable)
	}
	fmt.Fprintln(w, globalHookRestartNotice)
	accepted, err := confirm(ctx, fmt.Sprintf("Have you upgraded every hook-used installation, and do you want to use %s for global hooks?", selected.Executable), false)
	if err != nil {
		return err
	}
	if !accepted {
		return nil
	}
	if err := globalhooks.Save(ctx, selected); err != nil {
		return fmt.Errorf("enroll global hook installation: %w", err)
	}
	fmt.Fprintf(w, "Selected %s for global hooks. Global activation remains controlled by your user settings.\n", selected.Executable)
	globalPostRun(ctx, w)
	return nil
}

func enrollCurrentGlobalHookInstallation(ctx context.Context, w io.Writer) error {
	return enrollGlobalHookInstallation(ctx, w, os.Executable, uiform.PromptYN)
}
