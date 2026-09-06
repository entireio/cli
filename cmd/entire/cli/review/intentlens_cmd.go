package review

import (
	"errors"
	"io"
	"os"

	"github.com/spf13/cobra"

	"github.com/entireio/cli/cmd/entire/cli/review/intentlens"
)

func newIntentLensAuditCommand() *cobra.Command {
	var demo bool
	var inputFile string
	cmd := &cobra.Command{
		Use:   "audit",
		Short: "Display a structured IntentLens audit result",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !demo && inputFile == "" {
				intentlens.Render(cmd.OutOrStdout(), intentlens.ViewState{})
				return errors.New("pass --demo or --file <path>; use --file - to read stdin")
			}
			var data []byte
			var err error
			switch {
			case demo:
				data = intentlens.DemoAuditJSON()
			case inputFile == "-":
				data, err = io.ReadAll(cmd.InOrStdin())
			default:
				data, err = os.ReadFile(inputFile)
			}
			if err != nil {
				intentlens.Render(cmd.ErrOrStderr(), intentlens.ViewState{Err: err})
				return err
			}
			audit, err := intentlens.ParseAuditJSON(data)
			if err != nil {
				if errors.Is(err, intentlens.ErrEmptyAudit) {
					intentlens.Render(cmd.ErrOrStderr(), intentlens.ViewState{})
				} else {
					intentlens.Render(cmd.ErrOrStderr(), intentlens.ViewState{Err: err})
				}
				return err
			}
			intentlens.Render(cmd.OutOrStdout(), intentlens.ViewState{Audit: &audit, Demo: demo})
			return nil
		},
	}
	cmd.Flags().BoolVar(&demo, "demo", false, "display the bundled synthetic evidence fixture")
	cmd.Flags().StringVar(&inputFile, "file", "", "read a validated audit JSON result from a file, or - for stdin")
	cmd.MarkFlagsMutuallyExclusive("demo", "file")
	return cmd
}
