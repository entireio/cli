package cli

import (
	"errors"
	"fmt"
	"io"

	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/settings"

	"github.com/spf13/cobra"
)

func newTrustCmd() *cobra.Command {
	var revoke bool
	cmd := &cobra.Command{
		Use:   "trust",
		Short: "Sync this repo's checkpoints captured via global mode",
		Long: `Grant egress consent for the current repository: checkpoints captured via
global mode start syncing to the checkpoint sync remote on your next 'git push'.
Repos with an origin remote are trusted by origin (covers every clone of the
project on this machine); repos without a usable origin are trusted by folder.

Only applies to globally tracked repos — a repo with its own Entire setup
('entire enable') already syncs.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runTrust(cmd, revoke)
		},
	}
	cmd.Flags().BoolVar(&revoke, "revoke", false, "Withdraw trust; checkpoint sync is held again (not retroactive)")
	return cmd
}

func runTrust(cmd *cobra.Command, revoke bool) error {
	ctx := cmd.Context()
	out := cmd.OutOrStdout()
	if _, err := paths.WorktreeRoot(ctx); err != nil {
		cmd.SilenceUsage = true
		fmt.Fprintln(cmd.ErrOrStderr(), "Not a git repository. Run 'entire trust' from within a git repository.")
		return NewSilentError(errors.New("not a git repository"))
	}
	if settings.IsSetUpAny(ctx) {
		fmt.Fprintln(out, "not applicable — this repo is explicitly enabled; trust applies only to globally tracked repos")
		return nil
	}
	if revoke {
		return runTrustRevoke(cmd, out)
	}

	id, err := settings.TrustCurrentRepo(ctx)
	if err != nil {
		if errors.Is(err, settings.ErrGlobalModeUnconfigured) {
			return unconfiguredTrustTierError(cmd, err)
		}
		return fmt.Errorf("recording trust: %w", err)
	}
	if len(id.OriginKeys) > 0 {
		fmt.Fprintf(out, "✓ Trusted %s (all clones on this machine)\n", id.OriginKeys[0])
	} else {
		fmt.Fprintf(out, "✓ Trusted %s (this folder only)\n", id.Path)
	}
	// Best-effort: a count failure only omits the line — trust is already
	// recorded, and failing here would misreport that.
	switch n := heldCheckpointCount(ctx); {
	case n == 1:
		fmt.Fprintln(out, "1 held checkpoint will sync on your next push.")
	case n > 1:
		fmt.Fprintf(out, "%d held checkpoints will sync on your next push.\n", n)
	}
	return nil
}

// unconfiguredTrustTierError is the shared friendly rendering for
// settings.ErrGlobalModeUnconfigured: both the grant and revoke paths must
// swap the raw settings error for guidance, and revoke especially must not
// print a "revoked" confirmation on a machine where nothing was ever
// trustable.
func unconfiguredTrustTierError(cmd *cobra.Command, err error) error {
	cmd.SilenceUsage = true
	fmt.Fprintln(cmd.ErrOrStderr(), "Global tracking is not set up on this machine, so there is nothing to trust yet.")
	fmt.Fprintln(cmd.ErrOrStderr(), "Run 'entire enable --global' to track repos machine-wide, or 'entire enable' to set up this repo directly.")
	return NewSilentError(err)
}

func runTrustRevoke(cmd *cobra.Command, out io.Writer) error {
	ctx := cmd.Context()
	id, err := settings.RevokeCurrentRepo(ctx)
	if err != nil {
		if errors.Is(err, settings.ErrGlobalModeUnconfigured) {
			return unconfiguredTrustTierError(cmd, err)
		}
		return fmt.Errorf("revoking trust: %w", err)
	}
	scope := id.Path
	if len(id.OriginKeys) > 0 {
		scope = id.OriginKeys[0]
	}
	fmt.Fprintf(out, "✓ Revoked trust for %s — checkpoint sync held again; captured data stays local. Already-pushed checkpoints stay on the remote.\n", scope)
	// Masking honesty: a key revoke under active trust_all changes nothing —
	// implying effect would be a lie about what still egresses.
	if settings.CurrentTrustSource(ctx) == settings.TrustSourceAll {
		fmt.Fprintln(out, "Note: trust_all is enabled in settings; this repo will still sync until you disable it.")
	}
	return nil
}
