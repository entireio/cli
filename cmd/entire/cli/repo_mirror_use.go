package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"strings"

	"charm.land/huh/v2"
	"github.com/spf13/cobra"

	"github.com/entireio/cli/cmd/entire/cli/gitremote"
	"github.com/entireio/cli/cmd/entire/cli/interactive"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
	"github.com/entireio/cli/internal/coreapi"
)

// defaultMirrorRemote is the remote `mirror use` repoints by default: the one
// git itself defaults to for fetch/push, so pointing it at the mirror is what
// "use the mirror" means with no further flags.
const defaultMirrorRemote = "origin"

// defaultMirrorUpstreamRemote is where a replaced URL is preserved, so
// repointing origin is never a lossy operation — the forge stays reachable under
// the name git's own fork workflow uses for it.
const defaultMirrorUpstreamRemote = "upstream"

// defaultMirrorSideRemote is the suggested name when the user opts to add the
// mirror alongside their existing remote rather than replace it.
const defaultMirrorSideRemote = "entire"

// gitRemoteNameRe is the remote-name charset `mirror use` accepts. Git itself is
// laxer, but these names are written into `.git/config` section headers and
// passed as argv to `git remote`, so the value is pinned to a conservative
// shape: it must start alphanumeric (so it can never be read as a flag) and
// carries no path or glob metacharacters.
var gitRemoteNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]*$`)

// validateGitRemoteName rejects names git would refuse (or that would land
// somewhere unintended in .git/config) before they reach `git remote`.
func validateGitRemoteName(name string) error {
	if name == "" {
		return errors.New("remote name cannot be empty")
	}
	if !gitRemoteNameRe.MatchString(name) {
		return fmt.Errorf("%q is not a valid remote name (letters, digits, and . _ - / after a leading alphanumeric)", name)
	}
	// ".." would escape the intended config path; a ".lock" suffix collides with
	// git's own lockfile naming.
	if strings.Contains(name, "..") || strings.HasSuffix(name, ".lock") {
		return fmt.Errorf("%q is not a valid remote name", name)
	}
	return nil
}

// redactGitArgs returns args with anything that could carry credentials replaced
// by its redacted form, so the argv echoed in an error message is safe to print.
// A replaced remote URL can embed a token (https://user:token@host/...), and
// these errors reach stderr through main.go and from there into logs and pasted
// transcripts — the same reason reportMirrorRemotePlan redacts what it prints.
//
// Non-URL args (bare words like "remote", local paths) pass through untouched;
// see gitremote.RedactURLOrPath for why RedactURL cannot be applied blanket-fashion.
func redactGitArgs(args []string) []string {
	safe := make([]string, len(args))
	for i, a := range args {
		safe[i] = gitremote.RedactURLOrPath(a)
	}
	return safe
}

// gitRunner runs a git subcommand in dir. A package var so tests exercise the
// planning and prompt logic without mutating a real repository's config.
var gitRunner = func(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git %s: %w", strings.Join(redactGitArgs(args), " "), err)
	}
	return strings.TrimSpace(string(out)), nil
}

// listGitRemotes returns the names of every configured remote in dir.
func listGitRemotes(ctx context.Context, dir string) (map[string]bool, error) {
	out, err := gitRunner(ctx, dir, "remote")
	if err != nil {
		return nil, fmt.Errorf("list git remotes: %w", err)
	}
	remotes := make(map[string]bool)
	for _, line := range strings.Split(out, "\n") {
		if name := strings.TrimSpace(line); name != "" {
			remotes[name] = true
		}
	}
	return remotes, nil
}

// mirrorRemotePlan is the resolved set of git-config writes `mirror use` will
// perform. It is computed in full before anything is written so the command can
// echo exactly what it is about to do (and so the planning is unit-testable
// without touching a repo).
type mirrorRemotePlan struct {
	// remote is the remote that ends up pointing at mirrorURL.
	remote string
	// mirrorURL is the entire:// clone URL being adopted.
	mirrorURL string
	// add is true when remote does not exist yet (`git remote add` rather than
	// `git remote set-url`).
	add bool
	// replacedURL is the URL remote currently holds, when it is being
	// repointed. Empty when add is true.
	replacedURL string
	// preserveAs, when non-empty, is a new remote that will be created holding
	// replacedURL so the previous URL stays reachable.
	preserveAs string
	// preserveSkipped names the remote replacedURL would have been kept under,
	// when preservation was asked for but could not be done (the name is already
	// taken). Mutually exclusive with preserveAs, and empty when preservation was
	// never requested (`--upstream ''`). Set so the report can say out loud that
	// the previous URL did not make it into git config — the difference matters:
	// this is the one path where a successful-looking run drops the old URL.
	preserveSkipped string
	// noop is true when remote already points at mirrorURL.
	noop bool
}

// planMirrorRemote resolves what to write for a `mirror use` invocation.
// remotes is the set of already-configured remote names and currentURL the
// URL of the target remote ("" when it does not exist).
//
// upstream is the requested preserve-under name; it is honored only when the
// target remote is actually being repointed and the name is free. An occupied
// name is never clobbered — silently rewriting an existing `upstream` would be
// the one genuinely destructive thing this command could do — but it is recorded
// in preserveSkipped rather than dropped quietly, because a fork checkout
// (`origin` + `upstream` both already configured) hits that path by default and
// would otherwise see a clean ✓ while the replaced URL left git config for good.
func planMirrorRemote(remote, mirrorURL, currentURL, upstream string, remotes map[string]bool) mirrorRemotePlan {
	plan := mirrorRemotePlan{remote: remote, mirrorURL: mirrorURL}
	if !remotes[remote] {
		plan.add = true
		return plan
	}
	if strings.EqualFold(strings.TrimSpace(currentURL), mirrorURL) {
		plan.noop = true
		return plan
	}
	plan.replacedURL = currentURL
	if upstream != "" {
		// `remote` is known to exist in this branch, so an upstream naming it is
		// "occupied" too and lands in the skipped case — no separate check needed.
		if remotes[upstream] {
			plan.preserveSkipped = upstream
		} else {
			plan.preserveAs = upstream
		}
	}
	return plan
}

// applyMirrorRemotePlan performs the plan's git-config writes. The preserve step
// runs first so a failure there aborts before the original URL is overwritten.
func applyMirrorRemotePlan(ctx context.Context, dir string, plan mirrorRemotePlan) error {
	if plan.noop {
		return nil
	}
	// Deferred, not tail-positioned: the preserve step below is itself a remote
	// mutation, so a failure in the second write would otherwise leave the first
	// one uninvalidated and the invocation's memoized remote reads stale.
	defer strategy.InvalidateGitRemoteCache(ctx)
	if plan.preserveAs != "" {
		if _, err := gitRunner(ctx, dir, "remote", "add", plan.preserveAs, plan.replacedURL); err != nil {
			return fmt.Errorf("preserve current %s URL as %q: %w", plan.remote, plan.preserveAs, err)
		}
	}
	verb := "set-url"
	if plan.add {
		verb = "add"
	}
	if _, err := gitRunner(ctx, dir, "remote", verb, plan.remote, plan.mirrorURL); err != nil {
		return fmt.Errorf("point remote %q at the mirror: %w", plan.remote, err)
	}
	return nil
}

// reportMirrorRemotePlan echoes what was written, in recovery-friendly terms:
// every replaced URL is printed even when it was also preserved under another
// remote, so the previous value is always visible in the transcript.
//
// When preservation was requested but skipped, that gets an explicit stderr
// warning rather than just the absence of the "Kept the previous URL" line — the
// old URL is then only in this output, and an omitted line is far too quiet a
// signal for "your previous remote URL is no longer in git config" (a reader, or
// an agent scanning for ✓, would miss it).
func reportMirrorRemotePlan(out, errW io.Writer, plan mirrorRemotePlan) {
	if plan.noop {
		fmt.Fprintf(out, "Remote %q already points at the mirror:\n  %s\n", plan.remote, plan.mirrorURL)
		return
	}
	if plan.add {
		fmt.Fprintf(out, "✓ Added remote %q\n  %s\n", plan.remote, plan.mirrorURL)
	} else {
		fmt.Fprintf(out, "✓ Repointed remote %q at the mirror\n  %s\n", plan.remote, plan.mirrorURL)
		fmt.Fprintf(out, "  was: %s\n", gitremote.RedactURL(plan.replacedURL))
		if plan.preserveAs != "" {
			fmt.Fprintf(out, "✓ Kept the previous URL as remote %q\n", plan.preserveAs)
		}
	}
	fmt.Fprintf(out, "\nFetch through it:\n  git fetch %s\n", plan.remote)

	if plan.preserveSkipped != "" {
		// The URL is redacted here for the same reason it is on the "was:" line:
		// a replaced URL can carry credentials, and this warning is as likely to
		// end up in a log or a pasted transcript as anything else we print. Say so,
		// so a reader who needs the credentialed original knows to reconstruct it.
		fmt.Fprintf(errW, "\nWARNING: the previous URL of %q was NOT saved to git config — remote %q already exists.\n", plan.remote, plan.preserveSkipped)
		fmt.Fprintf(errW, "         It now only appears in the output above. To keep it under another name:\n")
		fmt.Fprintf(errW, "           git remote add <name> %s\n", gitremote.RedactURL(plan.replacedURL))
		fmt.Fprintf(errW, "         (credentials, if the URL had any, are redacted and must be re-supplied.)\n")
	}
}

// mirrorUseChoice is the outcome of the interactive replace-or-add prompt.
type mirrorUseChoice struct {
	// remote is the remote name to write (the target remote when replacing, a
	// new side remote when adding).
	remote string
	// upstream is the preserve-under name, or "" when adding a side remote
	// (nothing is being replaced, so there is nothing to preserve).
	upstream string
}

// promptMirrorRemoteChoice asks whether to repoint the existing target remote or
// add the mirror under a separate name. It is only reached on a terminal, and
// only when the target remote already exists with a different URL — the two
// cases where the write is not self-evidently what the user wanted.
func promptMirrorRemoteChoice(cmd *cobra.Command, remote, currentURL, mirrorURL, upstream string, remotes map[string]bool) (mirrorUseChoice, error) {
	const (
		choiceReplace = "replace"
		choiceAdd     = "add"
	)
	// Replace is listed first deliberately. huh answers an unreadable accessible
	// prompt with the first option (see the comment on `selected` below), so the
	// first option decides what a Ctrl+D / closed-stdin prompt does — and the only
	// self-consistent answer is the same thing the non-interactive path does with
	// these exact flags: repoint `remote`, preserving the old URL under
	// `upstream`. Putting "add" first would make an interrupted prompt diverge
	// from the documented default. The write is reported in full either way
	// (reportMirrorRemotePlan echoes the replaced URL), and it is local git
	// config, so it stays trivially reversible.
	replaceLabel := fmt.Sprintf("Replace %q — point it at the mirror", remote)
	if upstream != "" && upstream != remote && !remotes[upstream] {
		replaceLabel = fmt.Sprintf("Replace %q — point it at the mirror, keep the current URL as %q", remote, upstream)
	}
	sideName := defaultMirrorSideRemote
	for remotes[sideName] {
		sideName += "-mirror"
	}
	// Left empty rather than pre-seeded so the switch below can tell "huh handed
	// back something we don't recognise" from a real choice. Note this does NOT
	// make EOF safe: huh's accessible mode answers an unreadable prompt by
	// writing the FIRST option's value and returning a nil error (verified
	// behavior), so at EOF `selected` becomes choiceReplace regardless of what
	// it started as. That is why the option order matters below.
	var selected string
	if err := runMirrorUseForm(cmd, "Remote update", NewAccessibleForm(
		huh.NewGroup(
			huh.NewSelect[string]().
				Title(fmt.Sprintf("%q currently points at %s", remote, gitremote.RedactURL(currentURL))).
				Description("Mirror: "+mirrorURL).
				Options(
					huh.NewOption(replaceLabel, choiceReplace),
					huh.NewOption("Add the mirror as a separate remote instead", choiceAdd),
				).
				Value(&selected),
		),
	)); err != nil {
		return mirrorUseChoice{}, err
	}
	switch selected {
	case choiceReplace:
		return mirrorUseChoice{remote: remote, upstream: upstream}, nil
	case choiceAdd:
		// fall through to the name prompt
	default:
		// Unreachable with the options above (huh always writes one of them), and
		// kept so an unrecognised value can never fall through into a write.
		// Deliberately a plain error, not a SilentError: nothing has been printed
		// on this path, and main.go suppresses SilentError — so a silent one would
		// exit non-zero with no message at all, which is undiagnosable.
		return mirrorUseChoice{}, errors.New("no remote update selected")
	}

	name := sideName
	if err := runMirrorUseForm(cmd, "Remote update", NewAccessibleForm(
		huh.NewGroup(
			huh.NewInput().
				Title("Name for the new remote").
				Value(&name).
				Validate(func(v string) error {
					v = strings.TrimSpace(v)
					if err := validateGitRemoteName(v); err != nil {
						return err
					}
					if remotes[v] {
						return fmt.Errorf("remote %q already exists", v)
					}
					return nil
				}),
		),
	)); err != nil {
		return mirrorUseChoice{}, err
	}
	// Re-check outside the form: an unreadable accessible prompt leaves an Input
	// at its default without running Validate. The default computed above is
	// already free and well-formed, so this is belt-and-braces — but it keeps the
	// "never write an unvalidated remote name" invariant local to this function
	// instead of resting on how the default was derived.
	name = strings.TrimSpace(name)
	if err := validateGitRemoteName(name); err != nil {
		return mirrorUseChoice{}, fmt.Errorf("invalid remote name: %w", err)
	}
	if remotes[name] {
		return mirrorUseChoice{}, fmt.Errorf("remote %q already exists", name)
	}
	// A side remote replaces nothing, so there is no URL to preserve.
	return mirrorUseChoice{remote: name}, nil
}

// runMirrorUseForm runs a huh form, mapping a Ctrl+C / cancelled-context abort
// to a SilentError so the caller stops instead of falling through to write a
// zero-value remote name.
func runMirrorUseForm(cmd *cobra.Command, action string, form *huh.Form) error {
	if err := form.RunWithContext(cmd.Context()); err != nil {
		if cerr := handleFormCancellation(cmd.ErrOrStderr(), action, err); cerr != nil {
			return cerr
		}
		return NewSilentError(fmt.Errorf("%s cancelled", strings.ToLower(action)))
	}
	return nil
}

// mirrorUseForge is the only forge mirrors support today; a remote pointing
// anywhere else cannot name a mirrorable upstream.
const mirrorUseForge = "gh"

// resolveMirrorUseUpstream determines the GitHub upstream `mirror use` should
// look for mirrors of. An explicit [github-url] wins. Otherwise the coordinates
// are read from a configured remote — which already names the repo the user is
// standing in.
//
// Note the two distinct roles a remote name plays here: `remote` is the *write
// target* (what gets pointed at the mirror), while repo identity can come from
// any remote that names the upstream. So the target remote is consulted first
// (re-running `use --remote entire` on an already-mirrored side remote must
// resolve), then `origin` — otherwise `--remote entire` on a fresh clone would
// fail purely because the remote it is about to create does not exist yet.
//
// entire:// remotes resolve as readily as forge remotes (their forge lives in
// the URL path), so switching clusters never needs the repo retyped.
func resolveMirrorUseUpstream(ctx context.Context, dir, remote, arg string) (owner, repo string, err error) {
	if arg != "" {
		owner, repo, err = parseGitHubURL(arg)
		if err != nil {
			return "", "", fmt.Errorf("invalid <github-url>: %w", err)
		}
		return owner, repo, nil
	}

	candidates := []string{remote}
	if remote != defaultMirrorRemote {
		candidates = append(candidates, defaultMirrorRemote)
	}
	// Track why each candidate was rejected so the error can say which remotes
	// were tried and what was wrong with them, rather than a bare "not found".
	var tried []string
	for _, name := range candidates {
		rawURL, gerr := gitremote.GetRemoteURLInDir(ctx, dir, name)
		if gerr != nil {
			tried = append(tried, name+" (not configured)")
			continue
		}
		info, perr := gitremote.ParseURL(rawURL)
		if perr != nil {
			tried = append(tried, name+" (unparseable URL)")
			continue
		}
		if info.Forge != mirrorUseForge {
			tried = append(tried, name+" (not a GitHub repo — mirrors are GitHub-only)")
			continue
		}
		return strings.ToLower(info.Owner), strings.ToLower(info.Repo), nil
	}
	return "", "", fmt.Errorf("cannot tell which repo to mirror from the git remotes (tried %s); pass the GitHub URL explicitly", strings.Join(tried, ", "))
}

func newRepoMirrorUseCmd() *cobra.Command {
	var remote, upstream, cluster string
	cmd := &cobra.Command{
		Use:   "use [github-url] [cluster-host]",
		Short: "Point this clone's git remote at an Entire mirror",
		Long: "Rewrites the local git remote so fetch and push go through an " +
			"Entire mirror instead of the forge.\n\n" +
			"With no arguments, resolves the repo from the current clone's " +
			"`origin` remote, lists the clusters it is mirrored on, and — when " +
			"there is more than one — asks which to use. On a terminal it then " +
			"asks whether to repoint `origin` or add the mirror as a separate " +
			"remote; when repointing, the previous URL is kept as `upstream` so " +
			"the forge stays reachable.\n\n" +
			"Non-interactively it repoints --remote (default `origin`) directly, " +
			"preserving the replaced URL under --upstream. It only ever edits " +
			"local git config — the mirror must already exist (`entire repo " +
			"mirror create`); nothing server-side is changed.",
		Example: "  entire repo mirror use\n" +
			"  entire repo mirror use --cluster aws-us-east-2.entire.io\n" +
			"  entire repo mirror use github.com/octocat/hello-world\n" +
			"  entire repo mirror use github.com/octocat/hello-world aws-us-east-2.entire.io\n" +
			"  entire repo mirror use --remote entire\n" +
			"  entire repo mirror use --upstream ''",
		Args: cobra.RangeArgs(0, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			cmd.SilenceUsage = true
			if err := validateGitRemoteName(remote); err != nil {
				return fmt.Errorf("invalid --remote: %w", err)
			}
			// An empty --upstream is the documented opt-out of preserving the
			// replaced URL, so only a non-empty value is validated.
			if upstream != "" {
				if err := validateGitRemoteName(upstream); err != nil {
					return fmt.Errorf("invalid --upstream: %w", err)
				}
			}
			// Positional args are validated before the repo is resolved so a
			// malformed invocation fails identically inside and outside a clone.
			var upstreamArg, clusterArg string
			if len(args) > 0 {
				upstreamArg = strings.TrimSpace(args[0])
			}
			if len(args) > 1 {
				clusterArg = strings.TrimSpace(args[1])
			}
			// --cluster is the way to pin a cluster without also naming the repo
			// (the positional slot is second, so it would otherwise need an empty
			// first arg). Both forms setting different hosts is a contradiction,
			// not a precedence question.
			if cluster = strings.TrimSpace(cluster); cluster != "" {
				if clusterArg != "" && !strings.EqualFold(clusterArg, cluster) {
					return fmt.Errorf("[cluster-host] (%s) and --cluster (%s) disagree; pass only one", clusterArg, cluster)
				}
				clusterArg = cluster
			}
			if clusterArg != "" {
				if err := validateClusterHost(clusterArg); err != nil {
					return fmt.Errorf("invalid cluster host: %w", err)
				}
			}

			ctx := cmd.Context()
			repoRoot, err := paths.WorktreeRoot(ctx)
			if err != nil {
				fmt.Fprintln(cmd.ErrOrStderr(), "Not a git repository. Run `entire repo mirror use` from inside the clone whose remote you want to repoint.")
				return NewSilentError(errors.New("not a git repository"))
			}

			owner, repo, err := resolveMirrorUseUpstream(ctx, repoRoot, remote, upstreamArg)
			if err != nil {
				return err
			}

			// The pull-gated placement lookup is the same authority the clone's
			// STS exchange enforces, so anything the user could clone resolves
			// here — public mirrors included.
			var placements []coreapi.ResolvedPlacement
			if err := runCore(cmd, func(ctx context.Context, c *coreapi.Client) error {
				ps, lerr := resolvePullablePlacements(ctx, c, owner, repo)
				if lerr != nil {
					return lerr
				}
				placements = ps
				return nil
			}); err != nil {
				return err
			}
			if len(placements) == 0 {
				return fmt.Errorf("%s/%s is not mirrored (or you have no access to its mirrors); create one first:\n  entire repo mirror create github.com/%s/%s", owner, repo, owner, repo)
			}

			chosen, err := selectPlacement(cmd, placements, clusterArg, placementPicker{
				selector: "--cluster",
				title:    fmt.Sprintf("%s/%s is mirrored on more than one cluster — pick the one to use", owner, repo),
				action:   "Remote update",
			})
			if err != nil {
				return err
			}
			mirrorURL := mirrorCloneURL(chosen.ClusterHost, owner, repo)

			remotes, err := listGitRemotes(ctx, repoRoot)
			if err != nil {
				return err
			}
			// GetRemoteURLInDir errors when the remote is absent; that is the
			// "add" case, which carries no current URL.
			currentURL := ""
			if remotes[remote] {
				if currentURL, err = gitremote.GetRemoteURLInDir(ctx, repoRoot, remote); err != nil {
					return fmt.Errorf("read current URL of remote %q: %w", remote, err)
				}
			}

			target, preserve := remote, upstream
			// Prompt only when the write is ambiguous: the remote exists and
			// holds a different URL. A missing remote, or one already pointing
			// at this mirror, has exactly one sensible outcome.
			if remotes[remote] && !strings.EqualFold(strings.TrimSpace(currentURL), mirrorURL) && interactive.CanPromptInteractively() {
				choice, perr := promptMirrorRemoteChoice(cmd, remote, currentURL, mirrorURL, upstream, remotes)
				if perr != nil {
					return perr
				}
				target, preserve = choice.remote, choice.upstream
			}

			// currentURL was read for `remote`. When the prompt selected a
			// different (side) remote, that name was validated as free, so it
			// carries no current URL of its own.
			targetURL := currentURL
			if target != remote {
				targetURL = ""
			}
			plan := planMirrorRemote(target, mirrorURL, targetURL, preserve, remotes)
			if err := applyMirrorRemotePlan(ctx, repoRoot, plan); err != nil {
				return err
			}
			reportMirrorRemotePlan(cmd.OutOrStdout(), cmd.ErrOrStderr(), plan)
			return nil
		},
	}
	cmd.Flags().StringVar(&remote, "remote", defaultMirrorRemote, "Git remote to point at the mirror")
	cmd.Flags().StringVar(&upstream, "upstream", defaultMirrorUpstreamRemote, "Remote to preserve the replaced URL under; empty to discard it")
	cmd.Flags().StringVar(&cluster, "cluster", "", "Cluster host to use when the repo is mirrored on several (same as [cluster-host])")
	return cmd
}
