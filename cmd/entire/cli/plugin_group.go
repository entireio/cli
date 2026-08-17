package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"slices"
	"strings"

	"charm.land/huh/v2"
	"github.com/entireio/cli/cmd/entire/cli/interactive"
	"github.com/spf13/cobra"
)

// newPluginGroupCmd builds `entire plugin` and its subcommands. The kubectl
// dispatcher in plugin.go is the runtime mechanism — these commands manage a
// per-user managed directory that the dispatcher discovers because main.go
// prepends it to PATH at startup.
func newPluginGroupCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "plugin",
		Short: "Manage Entire plugins (install, list, upgrade, search, remove)",
		Long: `Manage Entire plugins.

Plugins are external executables named 'entire-<name>'. The CLI discovers
plugins on $PATH and from a per-user managed directory which is
auto-prepended to PATH at startup. The managed directory is, in order of
precedence:

  $ENTIRE_PLUGIN_DIR/bin (override)
  $XDG_DATA_HOME/entire/plugins/bin (Linux/macOS, when set)
  ~/.local/share/entire/plugins/bin (Linux/macOS default)
  %LOCALAPPDATA%\entire\plugins\bin (Windows, when set)
  ~\AppData\Local\entire\plugins\bin (Windows fallback when LOCALAPPDATA is unset)

Install sources:
  entire plugin install run                                    index lookup
  entire plugin install https://github.com/entireio/entire-run repository URL
  entire plugin install ./dist/entire-run                      local executable

Remote installs resolve the newest semver tag over the git protocol, then
download the platform's release asset (verified against the release's
checksums.txt when published). Discovery uses a git-synced plugin index;
see 'entire plugin search' and 'entire plugin index update'.`,
	}

	cmd.AddCommand(newPluginInstallCmd())
	cmd.AddCommand(newPluginListCmd())
	cmd.AddCommand(newPluginRemoveCmd())
	cmd.AddCommand(newPluginUpgradeCmd())
	cmd.AddCommand(newPluginSearchCmd())
	cmd.AddCommand(newPluginInfoCmd())
	cmd.AddCommand(newPluginBrowseCmd())
	cmd.AddCommand(newPluginDoctorCmd())
	cmd.AddCommand(newPluginIndexCmd())
	return cmd
}

// installArgKind classifies the install argument.
type installArgKind int

const (
	installFromPath installArgKind = iota
	installFromURL
	installFromIndex
)

// installSource is a parsed `plugin install` argument.
type installSource struct {
	Kind installArgKind
	// Ref is the repository URL, the filesystem path, or the catalog name,
	// according to Kind.
	Ref string
}

// parseInstallSource classifies an install argument and validates it in one
// pass. URLs are anything with a scheme or git's scp-like user@host:path form;
// paths must be explicit — a separator or a leading dot (./entire-foo) — and
// everything else is a bare name for index lookup.
//
// Classifying and validating together is deliberate: they were two functions
// that answered overlapping questions, and they drifted. The classifier matched
// a literal "git@" prefix while validatePluginRepoURL's scpLikeGitURL accepts
// any SSH username, so deploy@git.corp.io:group/entire-foo.git was a URL the
// validator would install from but the classifier sent down the path branch,
// failing with a confusing "stat: no such file". One function cannot disagree
// with itself. The scp-like test still has to precede the separator test, since
// these URLs contain a separator too.
//
// Parsing once and carrying the result also stops the argument being classified
// twice — the install command and runRemoteInstall each used to re-derive the
// kind from the raw string.
//
// Deliberately NOT stat-based: a stray file or directory in the CWD sharing a
// plugin's name must not shadow the index (and could never install anyway —
// path installs require an entire- basename). The spaces stay disjoint because
// validatePluginName rejects separators in plugin names.
func parseInstallSource(arg string) (installSource, error) {
	// An option-shaped argument is not a legitimate source of any kind: the URL
	// validator rejects it, validatePluginName rejects it, and a path would be
	// written ./-foo. Refusing it here beats letting it reach the path branch
	// and surface as "stat: no such file".
	if strings.HasPrefix(arg, "-") {
		return installSource{}, fmt.Errorf("install source %q must not start with '-'", arg)
	}
	switch {
	case strings.Contains(arg, "://") || scpLikeGitURL.MatchString(arg):
		if err := validatePluginRepoURL(arg); err != nil {
			return installSource{}, err
		}
		return installSource{Kind: installFromURL, Ref: arg}, nil
	case strings.ContainsAny(arg, `/\`) || strings.HasPrefix(arg, "."):
		return installSource{Kind: installFromPath, Ref: arg}, nil
	default:
		// Validating the name here means a reserved or malformed one says so,
		// instead of going to the catalog and coming back "not in the index".
		if err := validatePluginName(arg); err != nil {
			return installSource{}, err
		}
		return installSource{Kind: installFromIndex, Ref: arg}, nil
	}
}

func newPluginInstallCmd() *cobra.Command {
	var force, yes, noDeps, allowUnverified bool
	var pin, indexFlag string
	cmd := &cobra.Command{
		Use:   "install <name|url|path>",
		Short: "Install a plugin from the index, a git repository URL, or a local executable",
		Long: `Install a plugin.

Three source forms:

  name   Bare names resolve through the plugin index:
             entire plugin install run
  url    Full git repository URLs install from any git host. The newest
         semver tag is resolved with 'git ls-remote'; the platform's release
         asset is downloaded over HTTPS and verified against the release's
         checksums.txt. A release that publishes no checksums is refused
         unless --allow-unverified is passed:
             entire plugin install https://github.com/entireio/entire-run
  path   Local executables are linked into the managed directory (symlink
         first, so rebuilds are picked up immediately). Paths must be
         explicit — a separator or leading ./ — so a stray local file can
         never shadow an index name:
             entire plugin install ./dist/entire-run
             entire plugin install ./entire-run

Installing from a URL that is not listed in the plugin index asks for
confirmation; pass --yes to skip (required in non-interactive runs).

Plugins may declare dependencies in entire-plugin.yml. Missing dependencies
are listed and installed after a single confirmation (or with --yes);
--no-deps opts out.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			src, err := parseInstallSource(args[0])
			if err != nil {
				return fmt.Errorf("install plugin: %w", err)
			}

			if src.Kind == installFromPath {
				p, err := InstallPluginFromPath(InstallPluginOptions{SourcePath: src.Ref, Force: force})
				if err != nil {
					return fmt.Errorf("install plugin: %w", err)
				}
				fmt.Fprintf(cmd.OutOrStdout(), "Installed plugin %q → %s\n", p.Name, p.Path)
				warnIfShadowsBuiltin(cmd, p.Name)
				return nil
			}
			return silencePluginCancel(ctx, runRemoteInstall(ctx, cmd, src, remoteInstallFlags{
				force: force, yes: yes, noDeps: noDeps, allowUnverified: allowUnverified,
				pin: pin, index: indexFlag,
			}))
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "Replace an existing entry with the same name")
	cmd.Flags().BoolVar(&yes, "yes", false, "Skip confirmation prompts (non-index sources, dependency installs)")
	cmd.Flags().BoolVar(&noDeps, "no-deps", false, "Do not install declared dependencies")
	cmd.Flags().StringVar(&pin, "pin", "", "Install exactly this tag and skip it during 'plugin upgrade'")
	addIndexFlag(cmd, &indexFlag)
	cmd.Flags().BoolVar(&allowUnverified, "allow-unverified", false,
		"Install even when the release publishes no "+checksumsFileName+" to authenticate the download")
	return cmd
}

// addIndexFlag registers --index on cmd. Five subcommands accept it; the help
// text lived in five places before this.
func addIndexFlag(cmd *cobra.Command, target *string) {
	cmd.Flags().StringVar(target, "index", "",
		"Plugin index URL (overrides "+pluginIndexEnvVar+" and the built-in default)")
}

type remoteInstallFlags struct {
	force, yes, noDeps, allowUnverified bool
	pin, index                          string
}

func runRemoteInstall(ctx context.Context, cmd *cobra.Command, src installSource, flags remoteInstallFlags) error {
	out, errOut := cmd.OutOrStdout(), cmd.ErrOrStderr()

	repoURL := src.Ref
	var trusted bool
	var expectedName string

	// Both paths need the catalog: one to resolve a name, the other for the
	// trust check. Sync once. An unreachable index is fatal only for the
	// name-resolution path; a URL install degrades to "not listed".
	idx, idxErr := SyncPluginIndex(ctx, resolvePluginIndexURL(flags.index), false)

	if src.Kind == installFromIndex {
		if idxErr != nil {
			return fmt.Errorf("resolve %q via plugin index: %w", src.Ref, idxErr)
		}
		entry := idx.Find(src.Ref)
		if entry == nil {
			// Bare names never resolve to local files (see
			// parseInstallSource), but a user who typed one expecting a
			// path install deserves the pointer.
			if _, statErr := os.Stat(src.Ref); statErr == nil {
				return fmt.Errorf("plugin %q is not in the index; to install the local file, use an explicit path: entire plugin install ./%s", src.Ref, src.Ref)
			}
			return fmt.Errorf("plugin %q is not in the index; pass the repository URL to install from a specific repo (try 'entire plugin search %s')", src.Ref, src.Ref)
		}
		if len(entry.Platforms) > 0 && !slices.Contains(entry.Platforms, runtime.GOOS) {
			fmt.Fprintf(errOut, "Warning: index lists %q for %s only; this is %s — continuing anyway.\n",
				src.Ref, strings.Join(entry.Platforms, "/"), runtime.GOOS)
		}
		repoURL = entry.RepoURL
		// The user asked for this catalog name; the repo must not install
		// under a different one. Index installs never prompt, so this is the
		// only thing tying the request to what lands on PATH.
		expectedName = entry.Name
		trusted = true
	} else if entry := idx.FindByRepoURL(repoURL); idxErr == nil && entry != nil {
		// A listed URL installs without a prompt, so the catalog entry is the
		// only thing tying the request to what lands on PATH. Taking the name
		// from it means the reconciliation guard applies here too — otherwise
		// the remote names itself and --force replaces whatever it picked, with
		// no prompt at all.
		expectedName = entry.Name
		trusted = true
	}

	if !trusted {
		// An untrusted source cannot proceed unconfirmed: automation never
		// reaches this prompt, because the non-interactive path fails above
		// with the --yes hint.
		proceed, err := confirmInstallOrCancel(ctx, out,
			fmt.Sprintf("Install from %s? The repository is not listed in the plugin index.", redactURL(repoURL)),
			flags.yes)
		if err != nil || !proceed {
			return err
		}
	}

	// expectedName is set on the index path and empty on the URL path, where
	// the repository names itself.
	res, err := InstallPluginFromRepo(ctx, repoURL, expectedName, RemoteInstallOptions{
		Pin: flags.pin, Force: flags.force, AllowUnverified: flags.allowUnverified,
	})
	if err != nil {
		return fmt.Errorf("install plugin: %w", err)
	}
	if res.ReplacedFrom != "" {
		fmt.Fprintf(errOut, "Warning: replaced plugin %q, which was installed from %s.\n",
			res.Manifest.Name, redactURL(res.ReplacedFrom))
	}
	for _, t := range res.SkippedTags {
		fmt.Fprintf(errOut, "Warning: tag %s has no release asset for this platform; fell back to %s.\n", t, res.Manifest.Tag)
	}
	fmt.Fprintf(out, "Installed plugin %q %s from %s\n", res.Manifest.Name, res.Manifest.Tag, redactURL(repoURL))
	if res.Manifest.Unverified {
		fmt.Fprintf(errOut, "Warning: %s published no %s; the download was not authenticated.\n", res.Manifest.Tag, checksumsFileName)
	}
	warnIfShadowsBuiltin(cmd, res.Manifest.Name)

	if flags.noDeps || res.Metadata == nil || len(res.Metadata.Requires) == 0 {
		return nil
	}
	if idx == nil {
		// The index never loaded (offline, no cache). Dependencies resolve by
		// name through it and nowhere else, so planning would report every one
		// as "not in the plugin index" — blaming the catalog for a fetch that
		// failed, and failing the command after the plugin already installed
		// and said so. Degrade to a warning, as this path's contract says.
		fmt.Fprintln(errOut, "Warning: the plugin index is unavailable, so dependencies were not resolved. 'entire plugin doctor' will report what's missing.")
		return nil
	}
	return installPlannedDeps(ctx, cmd, res.Metadata.Requires, idx, flags)
}

// installPlannedDeps plans, confirms once (apt-style), and executes
// dependency installs. The main plugin is already installed at this point,
// so a declined or non-confirmable plan degrades to a warning, not an
// error — doctor reports the gap afterwards.
func installPlannedDeps(ctx context.Context, cmd *cobra.Command, reqs []PluginRequirement, idx *PluginIndex, flags remoteInstallFlags) error {
	out, errOut := cmd.OutOrStdout(), cmd.ErrOrStderr()
	plan, err := PlanDependencyInstalls(ctx, reqs, idx)
	if err != nil {
		return fmt.Errorf("resolve dependencies: %w", err)
	}
	for _, w := range plan.Warnings {
		fmt.Fprintf(errOut, "Warning: %s\n", w)
	}
	if len(plan.Actions) == 0 {
		return nil
	}

	// No trust annotation per entry: a missing dependency resolves only
	// through the index, and an upgrade reinstalls from the source the user
	// already accepted at that plugin's own install time. There is no longer a
	// path by which an author-chosen URL can appear in this list.
	fmt.Fprintf(out, "\nThis plugin requires %d additional plugin(s):\n", len(plan.Actions))
	for _, a := range plan.Actions {
		switch {
		case a.Upgrade:
			fmt.Fprintf(out, "  %s  (installed %s, needs >= %s — will upgrade from %s)\n",
				a.Name, a.CurrentTag, a.MinVersion, redactURL(a.RepoURL))
		default:
			fmt.Fprintf(out, "  %s  (%s)\n", a.Name, redactURL(a.RepoURL))
		}
	}
	ok, err := confirmPluginAction(ctx, "Install them now?", flags.yes)
	switch {
	case errors.Is(err, errConfirmNeedsTerminal):
		// Non-interactive without --yes: the main install already
		// succeeded, so skip with a pointer instead of failing late.
		fmt.Fprintln(errOut, "Skipping dependency installs (no terminal for confirmation; re-run with --yes). 'entire plugin doctor' will report what's missing.")
		return nil
	case err != nil:
		// User abort prints "Dependency install cancelled." and falls
		// through to the skip note; real prompt failures are returned —
		// claiming "skipped" for an error the user never saw would be
		// misreporting.
		if cancelErr := handleFormCancellation(errOut, "Dependency install", err); cancelErr != nil {
			return cancelErr
		}
		fmt.Fprintln(errOut, "'entire plugin doctor' will report what's missing.")
		return nil
	case !ok:
		fmt.Fprintln(errOut, "Skipping dependency installs; 'entire plugin doctor' will report what's missing.")
		return nil
	}
	results, err := ExecuteDepPlan(ctx, plan, flags.allowUnverified)
	if err != nil {
		return err
	}
	for _, res := range results {
		fmt.Fprintf(out, "Installed dependency %q\n", res.Manifest.Name)
		if res.ReplacedFrom != "" {
			// The top-level install names what it replaced; this path must too,
			// and it matters more here. Dependency installs are confirmed once
			// as a batch, apt-style, and their repo URLs come from the catalog —
			// so an upgrade action can repoint a plugin the user had installed
			// from their own fork without ever naming the swap.
			fmt.Fprintf(errOut, "Warning: dependency %q replaced an existing install from %s\n",
				res.Manifest.Name, redactURL(res.ReplacedFrom))
		}
	}
	return nil
}

// errConfirmNeedsTerminal signals that a confirmation was required but no
// terminal is available and --yes was not passed. Callers decide whether
// that is fatal (untrusted install) or an informed skip (dependency
// installs after the main install already succeeded).
var errConfirmNeedsTerminal = errors.New("confirmation required but no terminal available; re-run with --yes")

// confirmPluginAction asks a yes/no question. assumeYes short-circuits;
// non-interactive runs without --yes return errConfirmNeedsTerminal rather
// than guessing. Prompt errors (including huh.ErrUserAborted on Ctrl+C/Esc)
// are returned raw for callers to map via handleFormCancellation.
func confirmPluginAction(ctx context.Context, prompt string, assumeYes bool) (bool, error) {
	if assumeYes {
		return true, nil
	}
	if !interactive.CanPromptInteractively() {
		return false, fmt.Errorf("%w (%s)", errConfirmNeedsTerminal, prompt)
	}
	confirmed := false
	form := NewAccessibleForm(huh.NewGroup(
		huh.NewConfirm().Title(prompt).Value(&confirmed),
	))
	if err := form.RunWithContext(ctx); err != nil {
		// %w keeps huh.ErrUserAborted reachable for handleFormCancellation.
		return false, fmt.Errorf("confirm: %w", err)
	}
	return confirmed, nil
}

// confirmInstallOrCancel asks an install confirmation and collapses the three
// non-proceed outcomes onto their shared handling. An abort (Ctrl+C/Esc) and a
// plain "no" both print "Install cancelled." and exit cleanly: the exit code
// must not depend on which one the user chose. Real prompt failures surface
// wrapped, and errConfirmNeedsTerminal propagates unchanged so the caller
// decides whether an unattended run may proceed without an answer.
func confirmInstallOrCancel(ctx context.Context, out io.Writer, prompt string, assumeYes bool) (bool, error) {
	ok, err := confirmPluginAction(ctx, prompt, assumeYes)
	switch {
	case errors.Is(err, errConfirmNeedsTerminal):
		return false, err
	case err != nil:
		return false, handleFormCancellation(out, "Install", err)
	case !ok:
		fmt.Fprintln(out, "Install cancelled.")
		return false, nil
	}
	return true, nil
}

// silencePluginCancel maps Ctrl+C-induced failures to a SilentError per the
// codebase convention (clean.go, session_tokens.go) — printing "context
// canceled" at a user who just interrupted a clone or download is noise.
// The ctx.Err() check matters because a killed git child surfaces as
// "signal: killed", not context.Canceled, when the cancellation raced the
// subprocess.
func silencePluginCancel(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	if ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return NewSilentError(err)
	}
	return err
}

// warnIfShadowsBuiltin prints a one-line note to stderr when the just-installed
// plugin name matches a built-in command. The dispatcher's resolvePlugin gates
// dispatch on rootCmd.Find, so the built-in always wins at runtime — without
// this hint, a user who installed a shadowed plugin would silently get the
// built-in and have no idea their install was inert. We mirror the dispatcher's
// help/completion priming so names like "help" surface the warning too.
func warnIfShadowsBuiltin(cmd *cobra.Command, name string) {
	root := cmd.Root()
	if root == nil {
		return
	}
	root.InitDefaultHelpCmd()
	root.InitDefaultCompletionCmd(name)
	if c, _, err := root.Find([]string{name}); err == nil && c != root {
		fmt.Fprintf(cmd.ErrOrStderr(), "Note: %q shadows the built-in command; the built-in will take precedence at runtime.\n", name)
	}
}

func newPluginListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List plugins installed in the managed directory",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runPluginList(cmd.OutOrStdout())
		},
	}
}

func runPluginList(w io.Writer) error {
	plugins, err := ListInstalledPlugins()
	if err != nil {
		return fmt.Errorf("list plugins: %w", err)
	}
	dir, err := PluginBinDir()
	if err != nil {
		return fmt.Errorf("plugin bin dir: %w", err)
	}
	if len(plugins) == 0 {
		fmt.Fprintf(w, "No plugins installed in %s.\n", dir)
		fmt.Fprintln(w, "Install one with 'entire plugin install <name|url|path>', or drop an entire-<name> binary anywhere on $PATH.")
		return nil
	}
	manifestTag := map[string]string{}
	if manifests, err := ListPluginManifests(); err == nil {
		for _, m := range manifests {
			tag := m.Tag
			if m.Pinned {
				tag += " (pinned)"
			}
			manifestTag[m.Name] = tag
		}
	}
	fmt.Fprintf(w, "Managed plugin directory: %s\n\n", dir)
	for _, p := range plugins {
		tag := manifestTag[p.Name]
		if p.Symlink {
			fmt.Fprintf(w, "  %-20s %-18s → %s\n", p.Name, tag, p.LinkTarget)
		} else {
			fmt.Fprintf(w, "  %-20s %-18s %s\n", p.Name, tag, p.Path)
		}
	}
	return nil
}

func newPluginRemoveCmd() *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:   "remove <name>",
		Short: "Remove a plugin from the managed directory",
		Long: `Remove a plugin from the managed directory.

Only entries in the managed directory are affected. Plugins installed by
dropping a binary elsewhere on $PATH are unmanaged — remove those by hand.

When other installed plugins declare the target as a dependency, removal
is refused unless --force is given.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			if !force {
				dependents, err := DependentsOf(name)
				if err != nil {
					return err
				}
				if len(dependents) > 0 {
					return fmt.Errorf("plugin %q is required by %s; use --force to remove anyway", name, strings.Join(dependents, ", "))
				}
			}
			if err := RemoveManagedPlugin(name); err != nil {
				return fmt.Errorf("remove plugin: %w", err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Removed plugin %q\n", name)
			return nil
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "Remove even when other plugins depend on it")
	return cmd
}

func newPluginUpgradeCmd() *cobra.Command {
	var all bool
	cmd := &cobra.Command{
		Use:   "upgrade [name]",
		Short: "Upgrade remote-installed plugins to their newest tag",
		Long: `Upgrade remote-installed plugins to their newest semver tag.

Only plugins installed from a repository URL or the index carry the install
manifest upgrades need; local-dev symlink installs are skipped. Plugins
installed with --pin are skipped until reinstalled without the pin.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			out := cmd.OutOrStdout()
			var names []string
			switch {
			case len(args) == 1:
				names = []string{args[0]}
			case all:
				manifests, err := ListPluginManifests()
				if err != nil {
					return err
				}
				for _, m := range manifests {
					names = append(names, m.Name)
				}
				if len(names) == 0 {
					fmt.Fprintln(out, "No upgradable plugins (none were installed from a repository).")
					return nil
				}
			default:
				return errors.New("specify a plugin name or --all")
			}
			var firstErr error
			for _, name := range names {
				o, err := UpgradeInstalledPlugin(ctx, name)
				if err != nil {
					fmt.Fprintf(cmd.ErrOrStderr(), "Upgrade %q failed: %v\n", name, err)
					if firstErr == nil {
						firstErr = err
					}
					continue
				}
				switch {
				case o.Pinned:
					fmt.Fprintf(out, "%-20s pinned, skipped\n", name)
				case o.UpToDate:
					fmt.Fprintf(out, "%-20s up to date\n", name)
				default:
					fmt.Fprintf(out, "%-20s %s → %s\n", name, o.FromTag, o.ToTag)
				}
			}
			return silencePluginCancel(ctx, firstErr)
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "Upgrade every remote-installed plugin")
	return cmd
}

func newPluginSearchCmd() *cobra.Command {
	var indexFlag string
	cmd := &cobra.Command{
		Use:   "search [term]",
		Short: "Search the plugin index",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			term := ""
			if len(args) == 1 {
				term = args[0]
			}
			idx, err := SyncPluginIndex(ctx, resolvePluginIndexURL(indexFlag), false)
			if err != nil {
				return silencePluginCancel(ctx, err)
			}
			entries := idx.Search(term)
			if len(entries) == 0 {
				fmt.Fprintf(cmd.OutOrStdout(), "No plugins matching %q in the index.\n", term)
				return nil
			}
			printIndexEntries(cmd.OutOrStdout(), entries)
			return nil
		},
	}
	addIndexFlag(cmd, &indexFlag)
	return cmd
}

func printIndexEntries(w io.Writer, entries []PluginIndexEntry) {
	installedNames := map[string]bool{}
	if installed, err := ListInstalledPlugins(); err == nil {
		for _, p := range installed {
			installedNames[p.Name] = true
		}
	}
	for _, e := range entries {
		mark := " "
		if installedNames[e.Name] {
			mark = "*"
		}
		official := ""
		if e.Official {
			official = " [official]"
		}
		fmt.Fprintf(w, "%s %-20s %s%s\n", mark, e.Name, e.Description, official)
	}
	fmt.Fprintln(w, "\n* = installed. Install with 'entire plugin install <name>'.")
}

func newPluginInfoCmd() *cobra.Command {
	var indexFlag string
	cmd := &cobra.Command{
		Use:   "info <name>",
		Short: "Show index and install details for a plugin",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			name := args[0]
			out := cmd.OutOrStdout()

			entry := (*PluginIndexEntry)(nil)
			if idx, err := SyncPluginIndex(ctx, resolvePluginIndexURL(indexFlag), false); err == nil {
				entry = idx.Find(name)
			}
			m, err := LoadPluginManifest(name)
			if err != nil {
				return err
			}
			installed, err := FindInstalledPlugin(name)
			if err != nil {
				return err
			}
			if entry == nil && m == nil && installed == nil {
				return fmt.Errorf("plugin %q: not installed and not in the index", name)
			}

			fmt.Fprintf(out, "Name:        %s\n", name)
			if entry != nil {
				fmt.Fprintf(out, "Description: %s\n", entry.Description)
				fmt.Fprintf(out, "Repository:  %s\n", redactURL(entry.RepoURL))
				fmt.Fprintf(out, "Official:    %t\n", entry.Official)
				if len(entry.Platforms) > 0 {
					fmt.Fprintf(out, "Platforms:   %s\n", strings.Join(entry.Platforms, ", "))
				}
			}
			switch {
			case m != nil:
				fmt.Fprintf(out, "Installed:   %s (from %s", m.Tag, redactURL(m.RepoURL))
				if m.Pinned {
					fmt.Fprint(out, ", pinned")
				}
				fmt.Fprintln(out, ")")
				for _, r := range m.Requires {
					line := "Requires:    " + r.Name
					if r.MinVersion != "" {
						line += " >= " + r.MinVersion
					}
					fmt.Fprintln(out, line)
				}
			case installed != nil:
				fmt.Fprintf(out, "Installed:   local (%s)\n", installed.Path)
			default:
				fmt.Fprintln(out, "Installed:   no")
			}
			return nil
		},
	}
	addIndexFlag(cmd, &indexFlag)
	return cmd
}

func newPluginBrowseCmd() *cobra.Command {
	var indexFlag string
	cmd := &cobra.Command{
		Use:   "browse",
		Short: "Interactively browse the plugin index and install",
		Long: `Browse the plugin index in an interactive picker and install a selection.

The picker shows each plugin's name and description; the repository the binary
would come from is named in a confirmation before anything is downloaded.

Needs a terminal. Use 'entire plugin search' and 'entire plugin install <name>'
in scripts and non-interactive runs.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			if !interactive.CanPromptInteractively() {
				return errors.New("browse needs a terminal; use 'entire plugin search' instead")
			}
			idx, err := SyncPluginIndex(ctx, resolvePluginIndexURL(indexFlag), false)
			if err != nil {
				return silencePluginCancel(ctx, err)
			}
			if len(idx.Plugins) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "The plugin index is empty.")
				return nil
			}
			options := make([]huh.Option[string], 0, len(idx.Plugins)+1)
			for _, e := range idx.Plugins {
				label := e.Name
				if e.Description != "" {
					label = fmt.Sprintf("%s — %s", e.Name, e.Description)
				}
				options = append(options, huh.NewOption(label, e.Name))
			}
			options = append(options, huh.NewOption("(cancel)", ""))
			choice := ""
			form := NewAccessibleForm(huh.NewGroup(
				huh.NewSelect[string]().Title("Install a plugin").Options(options...).Value(&choice),
			))
			if err := form.RunWithContext(ctx); err != nil {
				return handleFormCancellation(cmd.OutOrStdout(), "Browse", err)
			}
			if choice == "" {
				return nil
			}

			// Confirm before installing. An index-resolved install is trusted
			// and so never prompts inside runRemoteInstall — which means
			// without this, highlighting a row and pressing Enter downloads a
			// binary and links it onto PATH in one keystroke. The picker also
			// only shows name and description, so the repository the binary
			// actually comes from is named here for the first time.
			out := cmd.OutOrStdout()
			prompt := fmt.Sprintf("Install %q?", choice)
			if entry := idx.Find(choice); entry != nil {
				prompt = fmt.Sprintf("Install %q from %s?", choice, redactURL(entry.RepoURL))
			}
			// Reachable only behind the CanPromptInteractively gate above, so
			// the errConfirmNeedsTerminal arm can never fire here.
			proceed, err := confirmInstallOrCancel(ctx, out, prompt, false)
			if err != nil || !proceed {
				return err
			}

			// The choice came from the catalog, so it is an index source by
			// construction — no re-parse needed.
			src := installSource{Kind: installFromIndex, Ref: choice}
			return silencePluginCancel(ctx, runRemoteInstall(ctx, cmd, src, remoteInstallFlags{index: indexFlag}))
		},
	}
	addIndexFlag(cmd, &indexFlag)
	return cmd
}

func newPluginDoctorCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Check installed plugins for missing dependencies and broken entries",
		RunE: func(cmd *cobra.Command, _ []string) error {
			issues, err := RunPluginDoctor(cmd.Context())
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if len(issues) == 0 {
				fmt.Fprintln(out, "All plugins healthy.")
				return nil
			}
			for _, i := range issues {
				fmt.Fprintf(out, "%s: %s\n", i.Plugin, i.Problem)
				if i.Fix != "" {
					fmt.Fprintf(out, "    fix: %s\n", i.Fix)
				}
			}
			cmd.SilenceUsage = true
			return NewSilentError(fmt.Errorf("%d plugin issue(s) found", len(issues)))
		},
	}
}

func newPluginIndexCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "index",
		Short: "Manage the plugin index",
	}
	var indexFlag string
	update := &cobra.Command{
		Use:   "update",
		Short: "Force a refresh of the plugin index",
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			url := resolvePluginIndexURL(indexFlag)
			idx, err := SyncPluginIndex(ctx, url, true)
			if err != nil {
				return silencePluginCancel(ctx, err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Index %s: %d plugin(s).\n", redactURL(url), len(idx.Plugins))
			return nil
		},
	}
	addIndexFlag(update, &indexFlag)
	cmd.AddCommand(update)
	return cmd
}
