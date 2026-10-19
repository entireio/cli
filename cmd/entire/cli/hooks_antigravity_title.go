package cli

import (
	"bytes"
	"encoding/base64"
	"io"

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
	var wrap, wrapB64 string
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

			original := wrap
			if wrapB64 != "" {
				decoded, decodeErr := base64.RawURLEncoding.DecodeString(wrapB64)
				if decodeErr != nil {
					logging.Debug(cmd.Context(), "antigravity title-tee: --wrap-b64 is not base64url; original title command not run", "error", decodeErr.Error())
					return nil
				}
				original = string(decoded)
			}
			if original == "" {
				return nil
			}
			// original is the user's own title command, preserved from agy's
			// settings.json by InstallTitleTee (quoted for sh, or base64url
			// for cmd.exe). Running it through the host's shell is
			// intentional — it is exactly what agy did with that string before
			// the tee took the slot — not an external-input injection surface.
			wrapped := wrappedTitleCommand(cmd.Context(), original)
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
	cmd.Flags().StringVar(&wrapB64, "wrap-b64", "", "original title command, base64url-encoded, to chain after capturing (written on Windows hosts)")
	cmd.MarkFlagsMutuallyExclusive("wrap", "wrap-b64")
	return cmd
}
