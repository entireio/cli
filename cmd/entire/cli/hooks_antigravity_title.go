package cli

import (
	"bytes"
	"io"
	"os/exec"

	"github.com/entireio/cli/cmd/entire/cli/agent/antigravity"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/spf13/cobra"
)

// newAntigravityTitleTeeCmd implements `entire hooks antigravity title-tee`.
//
// Antigravity invokes the configured title command on every agent state
// change, piping a state JSON (the only agy surface exposing token usage —
// same payload as the statusline script) to stdin. This command tees that
// JSON into the snapshot store and, with --wrap, pipes it through to the
// user's original title command so their window title is preserved.
//
// Contract: NEVER exit non-zero and NEVER write noise to stdout — stdout is
// rendered verbatim as the terminal window title. It also must work outside
// git repos and without entire being enabled (the title config is global).
func newAntigravityTitleTeeCmd() *cobra.Command {
	var wrap string
	cmd := &cobra.Command{
		Use:    "title-tee",
		Short:  "Tee agy state JSON (title/statusline payload) into the token snapshot store",
		Hidden: true,
		// NoArgs is safe: agy invokes the title command with stdin only, never positional args.
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			payload, err := io.ReadAll(cmd.InOrStdin())
			if err != nil {
				return nil //nolint:nilerr // never break agy's title rendering
			}
			if err := antigravity.AppendStatusSnapshot(payload); err != nil {
				// Best-effort capture: a persistent I/O failure (disk full,
				// unwritable cache dir) leaves a breadcrumb without ever
				// affecting stdout or the exit code.
				logging.Debug(cmd.Context(), "antigravity title-tee: snapshot append failed", "error", err.Error())
			}

			if wrap == "" {
				return nil
			}
			// wrap is the user's own original title command, preserved from
			// settings.json and round-tripped via shellSingleQuote, so running
			// it under `sh -c` is intentional — not an external-input injection
			// surface. POSIX-only: a stock Windows PATH has no `sh`, so a
			// wrapped user title command would fail there (shellSingleQuote's
			// POSIX quoting wouldn't survive cmd.exe either); revisit shell
			// selection if agy ships on Windows.
			wrapped := exec.CommandContext(cmd.Context(), "sh", "-c", wrap)
			wrapped.Stdin = bytes.NewReader(payload)
			wrapped.Stdout = cmd.OutOrStdout()
			wrapped.Stderr = cmd.ErrOrStderr()
			if err := wrapped.Run(); err != nil {
				// A failing user title script must not fail the tee (token
				// capture already succeeded), but leave a breadcrumb — the
				// user's own title rendering silently disappearing is
				// otherwise undiagnosable.
				logging.Debug(cmd.Context(), "antigravity title-tee: wrapped title command failed", "error", err.Error())
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&wrap, "wrap", "", "original title command to chain after capturing")
	return cmd
}
