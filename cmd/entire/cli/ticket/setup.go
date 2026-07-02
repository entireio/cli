package ticket

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"charm.land/huh/v2"
	"github.com/spf13/cobra"

	"github.com/entireio/cli/cmd/entire/cli/interactive"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/settings"
	"github.com/entireio/cli/cmd/entire/cli/uiform"
)

// setupFlags collects the non-interactive flag values for `ticket setup`.
type setupFlags struct {
	platform string
	team     string
	token    string
}

// newSetupCmd builds `entire ticket setup`.
func newSetupCmd() *cobra.Command {
	flags := setupFlags{}
	cmd := &cobra.Command{
		Use:   "setup",
		Short: "Choose a ticket platform and store credentials",
		Long: `Configure the ticket platform for this repository.

Runs an interactive prompt to pick a platform (Linear today), the team or
workspace key, and an API token. The platform and team are saved to
.entire/settings.json; the token is stored in your OS credential store, never
in settings.

Non-interactive:
  entire ticket setup --platform linear --team ENG --token <api-key>`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runSetup(cmd.Context(), cmd.OutOrStdout(), flags)
		},
	}
	cmd.Flags().StringVar(&flags.platform, "platform", "", "ticket platform (linear)")
	cmd.Flags().StringVar(&flags.team, "team", "", "team/workspace key on the platform")
	cmd.Flags().StringVar(&flags.token, "token", "", "API token (stored in the OS credential store)")
	return cmd
}

// runSetup resolves the platform/team/token either from flags or an
// interactive prompt, then persists them.
func runSetup(ctx context.Context, out io.Writer, flags setupFlags) error {
	// Non-interactive path: all required values supplied via flags.
	if flags.platform != "" && flags.team != "" && flags.token != "" {
		return applySetup(ctx, out, flags.platform, flags.team, flags.token)
	}

	if !interactive.CanPromptInteractively() {
		return errors.New("ticket setup needs an interactive terminal, or pass --platform, --team, and --token")
	}

	return runSetupInteractive(ctx, out, flags)
}

// runSetupInteractive drives the huh form, pre-filling from flags and any
// existing configuration.
func runSetupInteractive(ctx context.Context, out io.Writer, flags setupFlags) error {
	platform := flags.platform
	team := flags.team
	token := flags.token

	if platform == "" {
		if s, err := settings.Load(ctx); err == nil {
			if tc := s.TicketConfig(); !tc.IsZero() {
				platform = tc.Platform
				if team == "" {
					team = tc.Team
				}
			}
		}
	}
	if platform == "" {
		platform = string(SupportedPlatforms[0])
	}

	options := make([]huh.Option[string], 0, len(SupportedPlatforms))
	for _, p := range SupportedPlatforms {
		options = append(options, huh.NewOption(p.DisplayName(), string(p)))
	}

	form := uiform.New(huh.NewGroup(
		huh.NewSelect[string]().
			Title("Ticket platform").
			Description("Only Linear is supported today.").
			Options(options...).
			Value(&platform),
		huh.NewInput().
			Title("Team / workspace key").
			Description("e.g. ENG").
			Value(&team),
		huh.NewInput().
			Title("API token").
			Description("Stored in your OS credential store.").
			EchoMode(huh.EchoModePassword).
			Value(&token),
	))
	if err := form.RunWithContext(ctx); err != nil {
		if errors.Is(err, huh.ErrUserAborted) {
			return errors.New("ticket setup canceled")
		}
		return fmt.Errorf("ticket setup form: %w", err)
	}

	return applySetup(ctx, out, platform, strings.TrimSpace(team), token)
}

// applySetup validates the inputs and persists the platform/team to settings
// and the token to the credential store.
func applySetup(ctx context.Context, out io.Writer, platformStr, team, token string) error {
	platform, err := ParsePlatform(platformStr)
	if err != nil {
		return err
	}
	if team == "" {
		return errors.New("team/workspace key is required")
	}
	if strings.TrimSpace(token) == "" {
		return errors.New("API token is required")
	}

	if err := saveTicketConfig(ctx, &settings.TicketConfig{Platform: string(platform), Team: team}); err != nil {
		return err
	}
	if err := SaveToken(platform, token); err != nil {
		return err
	}

	fmt.Fprintf(out, "✓ Ticket platform configured: %s (team %s)\n", platform.DisplayName(), team)
	return nil
}

// saveTicketConfig writes the ticket section into .entire/settings.json. It
// reads the committed file directly (not the merged view) so clone-local
// preferences and local overrides are never baked into the committed file —
// mirroring how `entire investigate` persists its config section.
func saveTicketConfig(ctx context.Context, cfg *settings.TicketConfig) error {
	basePath, err := paths.AbsPath(ctx, settings.EntireSettingsFile)
	if err != nil {
		basePath = settings.EntireSettingsFile
	}

	base := &settings.EntireSettings{}
	data, readErr := os.ReadFile(basePath) //nolint:gosec // path is from AbsPath
	if readErr != nil && !os.IsNotExist(readErr) {
		return fmt.Errorf("read settings: %w", readErr)
	}
	if len(data) > 0 {
		base, err = settings.LoadFromBytes(data)
		if err != nil {
			return fmt.Errorf("parse settings: %w", err)
		}
	}

	base.Ticket = cfg
	if err := settings.Save(ctx, base); err != nil {
		return fmt.Errorf("save settings: %w", err)
	}
	return nil
}
