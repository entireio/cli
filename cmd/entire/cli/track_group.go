package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"charm.land/huh/v2"
	"github.com/spf13/cobra"

	"github.com/entireio/cli/cmd/entire/cli/interactive"
	"github.com/entireio/cli/cmd/entire/cli/jsonutil"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/review"
	"github.com/entireio/cli/cmd/entire/cli/settings"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
	"github.com/entireio/cli/cmd/entire/cli/tuiutil"
)

const trackLong = `Choose which repositories global enable records sessions in.

With global enable on, every git repository you work in is tracked unless you
deny it. Optionally, an org rule narrows tracking to repositories whose origin
lies in an allowed org (or that you allow by name). The rule lives only in your
user settings (track: {orgs, repos, deny}); nothing a repository commits can
change it.

Repositories are named by origin (github.com/owner/repo) or, without a remote,
by absolute path.`

// newTrackGroupCmd is `entire track`.
func newTrackGroupCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "track",
		Short: "Choose which repositories global enable tracks",
		Long:  trackLong,
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	cmd.AddCommand(newTrackListCmd(), newTrackSelectCmd(), newTrackOrgCmd(), newTrackResetCmd(),
		newTrackRepoCmd("deny", "Stop tracking a repository", denyTrackRepo),
		newTrackRepoCmd("allow", "Track a repository, even outside the org rule", allowTrackRepo))
	return cmd
}

// trackListJSON is `entire track list --json`.
type trackListJSON struct {
	Orgs    []string        `json:"orgs"`
	Repos   []string        `json:"repos"`
	Deny    []string        `json:"deny"`
	Current *trackRepoState `json:"current,omitempty"`
}

type trackRepoState struct {
	Repo    string `json:"repo"`
	Tracked bool   `json:"tracked"`
	Reason  string `json:"reason"`
}

func newTrackListCmd() *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "Show the tracking rule and whether this repository is tracked",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			rule, err := settings.LoadTrackRule(ctx)
			if err != nil {
				return err //nolint:wrapcheck // names the file and the reason
			}
			out := trackListJSON{Orgs: nonNil(rule.Orgs), Repos: nonNil(rule.Repos), Deny: nonNil(rule.Deny), Current: currentTrackState(ctx)}
			if jsonOut {
				data, err := jsonutil.MarshalIndentWithNewline(out, "", "  ")
				if err != nil {
					return fmt.Errorf("encoding: %w", err)
				}
				_, err = cmd.OutOrStdout().Write(data)
				return err //nolint:wrapcheck // stdout
			}
			writeTrackList(cmd.OutOrStdout(), out)
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Output as JSON")
	return cmd
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func writeTrackList(w io.Writer, out trackListJSON) {
	if len(out.Orgs) == 0 {
		fmt.Fprintln(w, "Rule: track every repository")
	} else {
		fmt.Fprintf(w, "Rule: only repositories in %s\n", tuiutil.SanitizeDisplayText(strings.Join(out.Orgs, ", ")))
	}
	for _, section := range []struct {
		title string
		keys  []string
	}{{"Allowed by name", out.Repos}, {"Denied", out.Deny}} {
		if len(section.keys) == 0 {
			continue
		}
		fmt.Fprintf(w, "%s:\n", section.title)
		for _, key := range section.keys {
			fmt.Fprintf(w, "  %s\n", tuiutil.SanitizeDisplayText(key))
		}
	}
	if c := out.Current; c != nil {
		state := "tracked"
		if !c.Tracked {
			state = "not tracked"
		}
		fmt.Fprintf(w, "This repository (%s): %s, %s\n", tuiutil.SanitizeDisplayText(c.Repo), state, c.Reason)
	}
}

// currentTrackState is the tracking answer for the repository the command
// runs in, or nil outside one.
func currentTrackState(ctx context.Context) *trackRepoState {
	root, err := paths.WorktreeRoot(ctx)
	if err != nil {
		return nil
	}
	key, err := settings.RepoKey(ctx, root)
	if err != nil {
		key = root
	}
	ok, reason := settings.RepoTrackable(ctx, root)
	return &trackRepoState{Repo: key, Tracked: ok, Reason: reason}
}

// refuseTrackFromAgent keeps the tracking rule the user's: an agent session
// widening or narrowing it would decide what gets recorded for them.
func refuseTrackFromAgent(cmd *cobra.Command) error {
	if caller := review.DetectAgentSession(); caller != "" {
		cmd.SilenceUsage = true
		return fmt.Errorf("refusing `%s` from %s: which repositories are tracked is the user's choice; ask the user to run it", cmd.CommandPath(), caller)
	}
	return nil
}

// newTrackRepoCmd is `entire track deny|allow <repo|path>`.
func newTrackRepoCmd(use, short string, apply func(*settings.TrackRule, string)) *cobra.Command {
	return &cobra.Command{
		Use:   use + " <repo|path>",
		Short: short,
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := refuseTrackFromAgent(cmd); err != nil {
				return err
			}
			key, err := resolveTrackRepo(cmd.Context(), args[0])
			if err != nil {
				cmd.SilenceUsage = true
				return err
			}
			path, err := settings.ModifyTrackRule(cmd.Context(), func(r *settings.TrackRule) { apply(r, key) })
			if err != nil {
				return err //nolint:wrapcheck // names the file and the reason
			}
			fmt.Fprintf(cmd.OutOrStdout(), "track %s %s (in %s)\n", use, tuiutil.SanitizeDisplayText(key), path)
			return nil
		},
	}
}

func denyTrackRepo(r *settings.TrackRule, key string) {
	r.Repos = slices.DeleteFunc(r.Repos, func(k string) bool { return k == key })
	if !slices.Contains(r.Deny, key) {
		r.Deny = append(r.Deny, key)
	}
}

func allowTrackRepo(r *settings.TrackRule, key string) {
	r.Deny = slices.DeleteFunc(r.Deny, func(k string) bool { return k == key })
	if len(r.Orgs) > 0 && !slices.Contains(r.Repos, key) {
		r.Repos = append(r.Repos, key)
	}
}

// resolveTrackRepo names a repository argument the way the rule stores it:
// a directory that is a git repository by its origin key (else its main
// worktree's path), anything else as an origin reference.
func resolveTrackRepo(ctx context.Context, arg string) (string, error) {
	if info, err := os.Stat(arg); err == nil && info.IsDir() {
		abs, err := filepath.Abs(arg)
		if err != nil {
			return "", fmt.Errorf("resolving %s: %w", arg, err)
		}
		if top, _, ok := repoOf(ctx, abs); ok {
			return settings.RepoKey(ctx, top) //nolint:wrapcheck // names the reason
		}
		return filepath.ToSlash(abs), nil
	}
	return settings.NormalizeTrackRepo(arg) //nolint:wrapcheck // names the reason
}

// trackOrgAddVerb is `entire track org add`.
const trackOrgAddVerb = "add"

func newTrackOrgCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "org",
		Short: "Limit tracking to repositories in an org",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	for _, verb := range []struct {
		use, short string
		add        bool
	}{{trackOrgAddVerb, "Track repositories in an org (e.g. github.com/entireio); only allowed orgs are then tracked", true},
		{"remove", "Remove an org from the rule; with none left, every repository is tracked", false}} {
		cmd.AddCommand(&cobra.Command{
			Use:   verb.use + " <host/org>",
			Short: verb.short,
			Args:  cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				if err := refuseTrackFromAgent(cmd); err != nil {
					return err
				}
				return setTrackOrg(cmd.Context(), cmd.OutOrStdout(), args[0], verb.add)
			},
		})
	}
	return cmd
}

// setTrackOrg adds org to, or removes it from, the org rule.
func setTrackOrg(ctx context.Context, w io.Writer, org string, add bool) error {
	key := settings.NormalizeTrackOrg(org)
	if key == "" {
		return fmt.Errorf("%q does not name an org; use host/org, e.g. github.com/entireio", org)
	}
	path, err := settings.ModifyTrackRule(ctx, func(r *settings.TrackRule) {
		r.Orgs = slices.DeleteFunc(r.Orgs, func(o string) bool { return settings.NormalizeTrackOrg(o) == key })
		if add {
			r.Orgs = append(r.Orgs, key)
		}
	})
	if err != nil {
		return err //nolint:wrapcheck // names the file and the reason
	}
	if add {
		fmt.Fprintf(w, "Tracking repositories in %s (track.orgs in %s).\n", tuiutil.SanitizeDisplayText(key), path)
	} else {
		fmt.Fprintf(w, "Removed %s from track.orgs in %s.\n", tuiutil.SanitizeDisplayText(key), path)
	}
	return nil
}

func newTrackResetCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "reset",
		Short: "Track every repository again (clears orgs, allowed and denied repositories)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := refuseTrackFromAgent(cmd); err != nil {
				return err
			}
			path, err := settings.ModifyTrackRule(cmd.Context(), func(r *settings.TrackRule) { *r = settings.TrackRule{} })
			if err != nil {
				return err //nolint:wrapcheck // names the file and the reason
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Every repository is tracked again (track cleared in %s).\n", path)
			return nil
		},
	}
}

func newTrackSelectCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "select",
		Short: "Pick tracked repositories interactively; deselected ones are denied",
		Long: "Lists the repositories Entire knows about (this one, ones it installed hooks in, and ones in your user settings), all tracked ones pre-selected. " +
			"Deselect a repository to deny it. Without a terminal, use `entire track deny` and `entire track allow`.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := refuseTrackFromAgent(cmd); err != nil {
				return err
			}
			return runTrackSelect(cmd)
		},
	}
}

func runTrackSelect(cmd *cobra.Command) error {
	if !interactive.CanPromptInteractively() {
		cmd.SilenceUsage = true
		return errors.New("entire track select needs a terminal; use `entire track deny <repo>` and `entire track allow <repo>`")
	}
	ctx := cmd.Context()
	rule, err := settings.LoadTrackRule(ctx)
	if err != nil {
		return err //nolint:wrapcheck // names the file and the reason
	}
	candidates := knownTrackRepos(ctx, rule)
	if len(candidates) == 0 {
		fmt.Fprintln(cmd.OutOrStdout(), "No known repositories yet; every repository is tracked.")
		return nil
	}
	options, selected := trackSelectOptions(candidates, rule)
	form := NewAccessibleForm(huh.NewGroup(
		huh.NewMultiSelect[string]().
			Title("Repositories to track").
			Description("Deselect a repository to stop tracking it. Space toggles, enter confirms.").
			Options(options...).
			Value(&selected),
	))
	if err := form.RunWithContext(ctx); err != nil {
		return handleFormCancellation(cmd.ErrOrStderr(), "Track selection", err)
	}
	path, err := settings.ModifyTrackRule(ctx, func(r *settings.TrackRule) { applyTrackSelection(r, candidates, selected) })
	if err != nil {
		return err //nolint:wrapcheck // names the file and the reason
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Saved the tracking rule to %s.\n", path)
	return nil
}

// knownTrackRepos lists the repository keys the picker offers: this
// repository, those Entire installed hooks in for other sessions, those named
// in the user's `repos` block, and every key already in the rule.
func knownTrackRepos(ctx context.Context, rule settings.TrackRule) []string {
	var keys []string
	add := func(key string) {
		if key != "" && !slices.Contains(keys, key) {
			keys = append(keys, key)
		}
	}
	if root, err := paths.WorktreeRoot(ctx); err == nil {
		if key, err := settings.RepoKey(ctx, root); err == nil {
			add(key)
		}
	}
	for _, root := range strategy.CrossRepoInstalls() {
		if key, err := settings.RepoKey(ctx, root); err == nil {
			add(key)
		}
	}
	for _, key := range settings.UserRepoKeys(ctx) {
		add(key)
	}
	for _, key := range append(slices.Clone(rule.Repos), rule.Deny...) {
		add(key)
	}
	return keys
}

// trackSelectOptions builds the picker: every candidate, pre-selected when
// the rule tracks it (with an org rule, a repository outside the orgs and
// not allowed by name starts deselected).
func trackSelectOptions(candidates []string, rule settings.TrackRule) ([]huh.Option[string], []string) {
	options := make([]huh.Option[string], 0, len(candidates))
	var selected []string
	for _, key := range candidates {
		on := rule.TracksKey(key)
		options = append(options, huh.NewOption(tuiutil.SanitizeDisplayText(key), key).Selected(on))
		if on {
			selected = append(selected, key)
		}
	}
	return options, selected
}

// applyTrackSelection writes the picker's answer so the rule tracks exactly
// the selected candidates: a selected one is no longer denied and, under an
// org rule it is not otherwise in, is allowed by name; a deselected one is
// denied and loses any allow by name, so selecting it again restores it.
// Keys the picker did not show keep their state.
func applyTrackSelection(r *settings.TrackRule, candidates, selected []string) {
	for _, key := range candidates {
		if slices.Contains(selected, key) {
			r.Deny = slices.DeleteFunc(r.Deny, func(k string) bool { return k == key })
			if !r.TracksKey(key) {
				r.Repos = append(r.Repos, key)
			}
			continue
		}
		r.Repos = slices.DeleteFunc(r.Repos, func(k string) bool { return k == key })
		if !slices.Contains(r.Deny, key) {
			r.Deny = append(r.Deny, key)
		}
	}
}

// trackRuleSummary is the one line `entire status` prints for the rule.
func trackRuleSummary(state *trackRepoState) string {
	if state == nil {
		return ""
	}
	if state.Tracked {
		return "Tracking: this repository is tracked (" + state.Reason + ")"
	}
	return "Tracking: this repository is not tracked (" + state.Reason + "); see `entire track list`"
}
