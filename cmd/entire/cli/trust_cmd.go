package cli

import (
	"errors"
	"fmt"
	"io"

	"github.com/entireio/cli/cmd/entire/cli/gitremote"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/settings"
	"github.com/entireio/cli/cmd/entire/cli/strategy"

	"github.com/spf13/cobra"
)

func newTrustCmd() *cobra.Command {
	var revoke bool
	var remote string
	cmd := &cobra.Command{
		Use:   "trust",
		Short: "Allow this repo's checkpoints to sync while global tracking is on",
		Long: `Grant egress consent for the current repository: its checkpoints start
syncing to the checkpoint sync remote on your next 'git push'.
Consent is keyed on that remote — the one checkpoints actually go to (see
'entire status') — so it covers every clone on this machine that syncs to the
same place; a remote whose URL is a bare path is trusted by folder instead.

While global tracking is on, every tracked repo needs this consent before its
checkpoints leave the machine. 'entire enable' records it automatically, so
this command is for repos that are tracked globally or that were enabled
before global tracking was turned on.

--remote names a different git remote to trust — use it when a held push told
you so: the first push to a fork elects that fork as the sync remote only once
checkpoints land there, so consent for it has to be recorded by name.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runTrust(cmd, revoke, remote)
		},
	}
	cmd.Flags().BoolVar(&revoke, "revoke", false, "Withdraw trust; checkpoint sync is held again (not retroactive)")
	cmd.Flags().StringVar(&remote, "remote", "", "Git remote to record consent for, instead of the currently elected checkpoint sync remote")
	return cmd
}

func runTrust(cmd *cobra.Command, revoke bool, remote string) error {
	ctx := cmd.Context()
	out := cmd.OutOrStdout()
	root, err := paths.WorktreeRoot(ctx)
	if err != nil {
		cmd.SilenceUsage = true
		fmt.Fprintln(cmd.ErrOrStderr(), "Not a git repository. Run 'entire trust' from within a git repository.")
		return NewSilentError(errors.New("not a git repository"))
	}
	if remote != "" {
		_, fetchFound, fetchErr := gitremote.GetRemoteURLsInDirIfSet(ctx, root, remote)
		_, pushFound, pushErr := gitremote.GetRemotePushURLsInDirIfSet(ctx, root, remote)
		if fetchErr != nil || pushErr != nil {
			return fmt.Errorf("reading remote %q: %w", remote, errors.Join(fetchErr, pushErr))
		}
		if !fetchFound && !pushFound {
			return fmt.Errorf("--remote %q is not a configured git remote (see `git remote -v`)", remote)
		}
		ctx = strategy.WithSyncRemoteOverride(ctx, remote)
		cmd.SetContext(ctx)
	}
	if revoke {
		// Not guarded by the inactive-tier refusal: revoking in an inactive
		// repo is harmless cleanup of a stale grant.
		return runTrustRevoke(cmd, out)
	}
	if err := refuseTrustWhenInactive(cmd); err != nil {
		return err
	}

	id, err := settings.TrustCurrentRepo(ctx)
	if err != nil {
		if errors.Is(err, settings.ErrGlobalModeUnconfigured) {
			return unconfiguredTrustTierError(cmd, err)
		}
		return fmt.Errorf("recording trust: %w", err)
	}
	// If the gate still holds after a successful write, a ✓ would misreport
	// consent the sync paths don't see.
	if !settings.CheckpointEgressAllowed(ctx) {
		fmt.Fprintf(out, "Warning: trust for %s was written to %s, but checkpoint sync is still held for this repo — check that file's trust entries.\n",
			id.DisplayScope(), settings.UserSettingsPath())
		return nil
	}
	scopeNote := "this folder only"
	if id.OriginKeyed() {
		scopeNote = "all clones on this machine"
	}
	if id.RemoteName != "" {
		scopeNote += "; checkpoints sync to " + id.RemoteName
	}
	fmt.Fprintf(out, "✓ Trusted %s (%s)\n", id.DisplayScope(), scopeNote)
	// Best-effort: a count failure only omits the line.
	switch n := heldCheckpointCount(ctx); {
	case n == 1:
		fmt.Fprintln(out, "1 held checkpoint will sync on your next push.")
	case n > 1:
		fmt.Fprintf(out, "%d held checkpoints will sync on your next push.\n", n)
	}
	return nil
}

// refuseTrustWhenInactive rejects a trust grant in a repo the global tier is
// not actually capturing (disabled tier or excluded repo) — silently
// pre-consenting an excluded repo is the bug consent exists to prevent.
func refuseTrustWhenInactive(cmd *cobra.Command) error {
	ctx := cmd.Context()
	// Unreadable settings and an unconfigured tier fall through: the trust
	// writer owns those messages.
	us, err := settings.LoadUserSettings(ctx)
	if err != nil || !us.GlobalConfigured() {
		return nil //nolint:nilerr // deliberate fall-through, see above
	}
	errW := cmd.ErrOrStderr()
	if !us.GlobalEnabled() {
		// A repo-enabled repo is active regardless of the tier, and already
		// syncs while the tier is off — recording trust into a disabled tier
		// would be a no-op that prints "Trusted".
		cmd.SilenceUsage = true
		fmt.Fprintln(errW, "Not recording trust: global tracking is off, so checkpoint sync is not gated.")
		fmt.Fprintf(errW, "Enable global tracking in %s, then re-run 'entire trust'.\n", settings.UserSettingsPath())
		return NewSilentError(errors.New("global tracking is off"))
	}
	if active, reason := settings.IsActiveForRepoWithReason(ctx); !active {
		cmd.SilenceUsage = true
		if reason == settings.InactiveReasonGlobalExcluded {
			fmt.Fprintln(errW, "Not recording trust: this repo is excluded in your settings, so Entire is not capturing it.")
			fmt.Fprintf(errW, "Remove the exclude from %s first if you want it tracked and synced.\n", settings.UserSettingsPath())
			return NewSilentError(errors.New("repo is excluded from global tracking"))
		}
		fmt.Fprintln(errW, "Not recording trust: global tracking is off, so Entire is not capturing this repo.")
		fmt.Fprintf(errW, "Enable global tracking in %s, then re-run 'entire trust'.\n", settings.UserSettingsPath())
		return NewSilentError(errors.New("global tracking is off"))
	}
	return nil
}

// unconfiguredTrustTierError is the shared friendly rendering for
// settings.ErrGlobalModeUnconfigured on both the grant and revoke paths.
func unconfiguredTrustTierError(cmd *cobra.Command, err error) error {
	cmd.SilenceUsage = true
	fmt.Fprintln(cmd.ErrOrStderr(), "Global tracking is not set up on this machine, so there is nothing to trust yet.")
	fmt.Fprintf(cmd.ErrOrStderr(), "Enable global tracking in %s, or run 'entire enable' to set up this repo directly.\n", settings.UserSettingsPath())
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
	fmt.Fprintf(out, "✓ Revoked trust for %s — checkpoint sync held again; captured data stays local. Already-pushed checkpoints stay on the remote.\n", id.DisplayScope())
	// A key revoke under active trust_all changes nothing — say so.
	if settings.CurrentTrustSource(ctx) == settings.TrustSourceAll {
		fmt.Fprintln(out, "Note: trust_all is enabled in settings; this repo will still sync until you disable it.")
	}
	return nil
}
