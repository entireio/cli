package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"strings"

	"github.com/spf13/cobra"

	"github.com/entireio/cli/cmd/entire/cli/gitremote"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
	"github.com/entireio/cli/internal/coreapi"
)

// defaultMirrorRemote is the remote `remote add` reads the repo's identity from
// when the name being written does not exist yet — which, for an add, is the
// usual case. It is git's own default for fetch and push, so it is the remote
// that names the repo the user is standing in.
const defaultMirrorRemote = "origin"

// gitRemoteNameRe is the remote-name charset `remote add` accepts. Git itself is
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
		// cmd.Output() captures stderr on an ExitError and nowhere else, and
		// without it the wrap renders as a bare "exit status 128" — git's own
		// sentence ("remote.origin.url has multiple values") is the only thing
		// that says what to fix. Redacted like the argv, for the same reason.
		if detail := gitStderr(err); detail != "" {
			return "", fmt.Errorf("git %s: %w: %s", strings.Join(redactGitArgs(args), " "), err, detail)
		}
		return "", fmt.Errorf("git %s: %w", strings.Join(redactGitArgs(args), " "), err)
	}
	return strings.TrimSpace(string(out)), nil
}

// stderrURLRe matches a URL embedded in a line of prose, so credentials can be
// stripped from it without touching the sentence around it.
//
// Only whitespace ends the match. Ending it on a quote as well would read
// better against git's own `to 'https://…'` quoting, but a password containing
// a quote would then split the match inside the credential and leave the rest
// of it in the output — the one thing this must never do. A trailing quote
// swept into the path is the harmless side of that trade.
//
// Deliberately not applied to scp-style remotes (git@host:path): they carry no
// password, and a pattern loose enough to match one would match far more.
var stderrURLRe = regexp.MustCompile(`[a-zA-Z][a-zA-Z0-9+.\-]*://\S*`)

// gitStderr returns what git wrote to stderr before failing, flattened to one
// line with any embedded URL redacted. Empty when the error carries none.
//
// Redaction is per URL, never per line: RedactURLOrPath routes anything
// containing "://" through RedactURL, which rebuilds the value from a parsed
// scheme/host/path — and a prose line parses as an opaque URL whose scheme is
// its first word, so "fatal: could not set 'x' to 'https://h/p'" collapses to
// "fatal://". That destroys the very sentence this function exists to surface.
func gitStderr(err error) string {
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		return ""
	}
	var parts []string
	for _, line := range strings.Split(string(exitErr.Stderr), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		parts = append(parts, stderrURLRe.ReplaceAllStringFunc(trimmed, gitremote.RedactURL))
	}
	return strings.Join(parts, "; ")
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

// mirrorRemotePlan is the resolved set of git-config writes `remote add` will
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
	// repointed. Empty when add is true. It is echoed by the report, which is
	// the only record of it once the write lands.
	replacedURL string
	// noop is true when remote already points at mirrorURL.
	noop bool
	// strandedPushURLs are the remote's configured pushurl values that do not
	// name mirrorURL. git falls back to the fetch URL for pushes only when no
	// pushurl is set, so an explicit one survives `git remote set-url` and keeps
	// sending pushes to the forge — which this command says it is replacing.
	strandedPushURLs []string
}

// planMirrorRemote resolves what to write for a `remote add` invocation.
// remotes is the set of already-configured remote names and currentURL the
// URL of the target remote ("" when it does not exist).
//
// An occupied name is refused unless override is set, matching what `git remote
// add` itself does with one: an add that silently repoints an existing remote is
// not an add. A remote that already carries this exact URL is not a collision —
// it is the requested end state — so it reports as a no-op and a re-run stays
// safe.
//
// A repointed URL is not copied anywhere. `--override` is the caller saying they
// mean to overwrite this remote, so saving the old value under a remote they
// never named would be a write they did not ask for — and the name to save it
// under can itself be taken, which is how the previous design could report a
// clean ✓ over a lost URL. The report echoes the replaced URL instead.
func planMirrorRemote(remote, mirrorURL, currentURL string, pushURLs []string, override bool, remotes map[string]bool) (mirrorRemotePlan, error) {
	plan := mirrorRemotePlan{remote: remote, mirrorURL: mirrorURL, strandedPushURLs: strandedPushURLs(mirrorURL, pushURLs)}
	if !remotes[remote] {
		plan.add = true
		return plan, nil
	}
	if strings.EqualFold(strings.TrimSpace(currentURL), mirrorURL) {
		plan.noop = true
		return plan, nil
	}
	if !override {
		return mirrorRemotePlan{}, occupiedRemoteError(remote, currentURL)
	}
	plan.replacedURL = currentURL
	return plan, nil
}

// explicitPushURLs returns the remote's configured `pushurl` values, which are
// the only ones that survive the `git remote set-url` this command performs.
//
// It reads the config key rather than `git remote get-url --push`, which echoes
// the FETCH url when no pushurl is set: against that output every ordinary
// repoint looks like a stranded push target, since the fetch URL is by
// definition about to change. The key is absent far more often than not, and
// git exits 1 for an absent key, so that is reported as "none" rather than as a
// failure — this feeds a warning, not the write.
func explicitPushURLs(ctx context.Context, dir, remote string) []string {
	out, err := gitRunner(ctx, dir, "config", "--get-all", "remote."+remote+".pushurl")
	if err != nil {
		return nil
	}
	var urls []string
	for _, line := range strings.Split(out, "\n") {
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			urls = append(urls, trimmed)
		}
	}
	return urls
}

// strandedPushURLs returns the explicitly configured push URLs that will not
// point at mirrorURL after the write. One already naming mirrorURL is not
// stranded — pushes through it already reach the mirror.
func strandedPushURLs(mirrorURL string, pushURLs []string) []string {
	var stranded []string
	for _, u := range pushURLs {
		if trimmed := strings.TrimSpace(u); trimmed != "" && !strings.EqualFold(trimmed, mirrorURL) {
			stranded = append(stranded, trimmed)
		}
	}
	return stranded
}

// applyMirrorRemotePlan performs the plan's single git-config write.
func applyMirrorRemotePlan(ctx context.Context, dir string, plan mirrorRemotePlan) error {
	if plan.noop {
		return nil
	}
	defer strategy.InvalidateGitRemoteCache(ctx)
	verb := "set-url"
	if plan.add {
		verb = "add"
	}
	if _, err := gitRunner(ctx, dir, "remote", verb, plan.remote, plan.mirrorURL); err != nil {
		return fmt.Errorf("point remote %q at the mirror: %w", plan.remote, err)
	}
	return nil
}

// reportMirrorRemotePlan echoes what was written. A replaced URL is printed
// because this output is now the only record of it: nothing copies it into git
// config. It is redacted (a remote URL can embed a token, and this text reaches
// logs and pasted transcripts), so a caller who needs the credentialed original
// has to keep it themselves — which is what --override asks them to accept.
func reportMirrorRemotePlan(out io.Writer, plan mirrorRemotePlan) {
	if plan.noop {
		fmt.Fprintf(out, "Remote %q already points at the mirror:\n  %s\n", plan.remote, plan.mirrorURL)
		// Not a return: the fetch URL needing no change says nothing about an
		// explicit pushurl, and a remote that fetches from the mirror while
		// pushing to the forge is exactly what the note below exists to catch.
		// This is the case an idempotent script hits on its second run.
		reportStrandedPushURLs(out, plan)
		return
	}
	if plan.add {
		fmt.Fprintf(out, "✓ Added remote %q\n  %s\n", plan.remote, plan.mirrorURL)
	} else {
		fmt.Fprintf(out, "✓ Repointed remote %q at the mirror\n  %s\n", plan.remote, plan.mirrorURL)
		fmt.Fprintf(out, "  was: %s\n", gitremote.RedactURLOrPath(plan.replacedURL))
	}
	fmt.Fprintf(out, "\nFetch through it:\n  git fetch %s\n", plan.remote)

	reportStrandedPushURLs(out, plan)
}

// reportStrandedPushURLs names any explicit pushurl that will not reach the
// mirror. It outranks the URL this command writes, so without it the command
// would report that fetch and push both go through Entire while pushes still
// reach the forge. Named rather than rewritten: the push target is the user's,
// and clearing it is not what they asked for.
func reportStrandedPushURLs(out io.Writer, plan mirrorRemotePlan) {
	if len(plan.strandedPushURLs) == 0 {
		return
	}
	fmt.Fprintf(out, "\nNOTE: remote %q still pushes elsewhere — `git remote set-url --push` was set on it:\n", plan.remote)
	for _, u := range plan.strandedPushURLs {
		fmt.Fprintf(out, "  %s\n", gitremote.RedactURLOrPath(u))
	}
	fmt.Fprintf(out, "To push through the mirror too:\n  git remote set-url --push %s %s\n", plan.remote, plan.mirrorURL)
}

// resolveRemoteRepoRef determines the repository `remote add` should look for
// placements of. An explicit [repo] wins. Otherwise the coordinates are
// read from a configured remote — which already names the repo the user is
// standing in.
//
// Both forges resolve: a GitHub remote names a mirrorable upstream, and an
// entire:// remote names the repo it was cloned from, native or not. Which one
// came back decides how placements are looked up.
//
// Note the two distinct roles a remote name plays here: `remote` is the *write
// target* (the name being added or repointed), while repo identity can come
// from any remote that names the upstream. So the target remote is consulted
// first (re-running `add entire --override` on an already-mirrored remote must
// resolve), then `origin` — otherwise `add entire` on a fresh clone would fail
// purely because the remote it is about to create does not exist yet.
//
// entire:// remotes resolve as readily as forge remotes (their forge lives in
// the URL path), so switching clusters never needs the repo retyped.
func resolveRemoteRepoRef(ctx context.Context, dir, remote, arg string) (mirrorRepoRef, error) {
	if arg != "" {
		return parseMirrorRepoRef(arg)
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
		switch info.Forge {
		case mirrorCloneForge:
			// GitHub owners and repos are stored lowercase server-side.
			return mirrorRepoRef{forge: mirrorCloneForge, owner: strings.ToLower(info.Owner), repo: strings.ToLower(info.Repo)}, nil
		case nativeCloneForge:
			// Native names keep the spelling the remote carries; both lookups
			// behind them fold case server-side.
			return mirrorRepoRef{forge: nativeCloneForge, owner: info.Owner, repo: info.Repo}, nil
		default:
			tried = append(tried, name+" (not an Entire or GitHub repo)")
		}
	}
	return mirrorRepoRef{}, fmt.Errorf("cannot tell which repo to act on from the git remotes (tried %s); pass a repository reference explicitly (for example, /gh/owner/repo or /et/project/repo)", strings.Join(tried, ", "))
}

// occupiedRemoteError is the refusal `remote add` gives for a name that is
// already configured, shared by the pre-flight check and planMirrorRemote so
// the two cannot word it differently.
func occupiedRemoteError(remote, currentURL string) error {
	return fmt.Errorf("remote %q already exists and points at %s; pass --override to repoint it, or name a remote that does not exist yet",
		remote, gitremote.RedactURLOrPath(currentURL))
}

// runRemoteLookup runs the placement lookup against the right control plane.
//
// An explicit --cluster may name a cluster in a federation the active context
// cannot see, whose mirrors its core will not list — so the lookup dials the
// core fronting that host instead, discovered from its well-known and
// authenticated with the matching local context. This is the same routing
// `repo clone` performs, and without it `remote add … --cluster <other-fed>`
// fails on a mirror `repo clone --cluster <other-fed>` fetches happily.
//
// A host that does not resolve is the one failure that is evidence about the
// host rather than the network, and it is also what a typo looks like; fall
// back to the active context there so the user gets "not mirrored on …;
// available: …" rather than a DNS error. Native refs always resolve within
// their own federation, so they never take the detour.
func runRemoteLookup(cmd *cobra.Command, repoRef mirrorRepoRef, clusterHost string, lookup func(context.Context, *coreapi.Client) error) error {
	if clusterHost == "" || repoRef.forge == nativeCloneForge {
		return runCore(cmd, lookup)
	}
	err := runCoreForCluster(cmd, clusterHost, lookup)
	if err == nil {
		return nil
	}
	if !hostDoesNotResolve(err) {
		return err
	}
	logging.Debug(cmd.Context(), "cluster host does not resolve; listing placements from the active context", "cluster", clusterHost, "error", err)
	if fallbackErr := runCore(cmd, lookup); fallbackErr != nil {
		// Each half names a different thing the user may have to fix — a
		// mistyped host, and whatever stopped the active context from standing
		// in — and they would otherwise learn the second only after the first.
		return errors.Join(err, fallbackErr)
	}
	return nil
}

// runRepoRemoteAdd is `remote add`'s body, lifted out of the RunE closure so
// the command literal stays readable and each step below can be named.
func runRepoRemoteAdd(cmd *cobra.Command, args []string, cluster string, override bool) error {
	cmd.SilenceUsage = true
	remote := strings.TrimSpace(args[0])
	if err := validateGitRemoteName(remote); err != nil {
		return fmt.Errorf("invalid remote name: %w", err)
	}
	// Arguments are validated before the repo is resolved so a
	// malformed invocation fails identically inside and outside a clone.
	var repoArg string
	if len(args) > 1 {
		repoArg = strings.TrimSpace(args[1])
	}
	clusterHost := strings.TrimSpace(cluster)
	if clusterHost != "" {
		if err := validateClusterHost(clusterHost); err != nil {
			return fmt.Errorf("invalid --cluster: %w", err)
		}
	}

	ctx := cmd.Context()
	repoRoot, err := paths.WorktreeRoot(ctx)
	if err != nil {
		fmt.Fprintln(cmd.ErrOrStderr(), "Not a git repository. Run `entire repo remote add` from inside the clone whose remote you want to write.")
		return NewSilentError(errors.New("not a git repository"))
	}

	// Both inputs to the occupied-name refusal are local, so it is
	// settled before anything dials or prompts. Leaving it until after
	// planMirrorRemote — where the old `use` put its confirm form —
	// would make `remote add origin` pay a control-plane round trip and
	// a cluster picker only to be told the name is taken.
	remotes, err := listGitRemotes(ctx, repoRoot)
	if err != nil {
		return err
	}
	// GetRemoteURLInDir errors when the remote is absent; that is the "add"
	// case, which carries neither a current URL nor a pushurl.
	currentURL := ""
	var pushURLs []string
	if remotes[remote] {
		if currentURL, err = gitremote.GetRemoteURLInDir(ctx, repoRoot, remote); err != nil {
			return fmt.Errorf("read current URL of remote %q: %w", remote, err)
		}
		// A remote already holding the URL this run would write is not a
		// collision, but which URL that is takes the lookup below. Only a
		// remote that cannot be the no-op case is refused here; the rest is
		// left to planMirrorRemote.
		if !override && !isEntireCloneURL(currentURL) {
			return occupiedRemoteError(remote, currentURL)
		}
		pushURLs = explicitPushURLs(ctx, repoRoot, remote)
	}

	repoRef, err := resolveRemoteRepoRef(ctx, repoRoot, remote, repoArg)
	if err != nil {
		return err
	}
	qualified := "/" + repoRef.forge + "/" + repoRef.owner + "/" + repoRef.repo

	mirrorURL, err := resolveRemoteMirrorURL(cmd, repoRef, qualified, clusterHost)
	if err != nil {
		return err
	}
	plan, err := planMirrorRemote(remote, mirrorURL, currentURL, pushURLs, override, remotes)
	if err != nil {
		return err
	}
	if err := applyMirrorRemotePlan(ctx, repoRoot, plan); err != nil {
		return err
	}
	reportMirrorRemotePlan(cmd.OutOrStdout(), plan)
	return nil
}

// resolveRemoteMirrorURL resolves the entire:// URL `remote add` will write:
// the repo's placements, the one this run acts on, and the URL for it. Split
// out of runRepoRemoteAdd because it is the whole network half of the command —
// two forges, a picker and a validation step — and nothing below it needs
// anything but the URL.
func resolveRemoteMirrorURL(cmd *cobra.Command, repoRef mirrorRepoRef, qualified, clusterHost string) (string, error) {
	// For GitHub, the pull-gated placement lookup is the same authority
	// the clone's STS exchange enforces, so anything the user could clone
	// resolves here — public mirrors included. For a native repo the
	// placements are the repo's own primary plus its ready mirrors,
	// which the catalog turns into the cluster hosts a placement is
	// addressed by.
	var (
		placements []coreapi.ResolvedPlacement
		nativeRepo *coreapi.Repo
		// The primary's host as the placement list spells it. Resolved
		// through the catalog, exactly as nativeUsePlacements resolves
		// every host it offers, so the default is byte-identical to the
		// entry it has to match. Repo.ClusterHost names the same cluster
		// but is a second derivation of it, and any drift between the
		// two (an explicit port in the catalog's publicUrl, say) would
		// make the no-terminal branch report that the repo is on no
		// cluster it is plainly on.
		nativePrimaryHost string
	)
	lookup := func(ctx context.Context, c *coreapi.Client) error {
		if repoRef.forge == nativeCloneForge {
			repo, cat, lerr := loadNativeRepo(ctx, c, repoRef)
			if lerr != nil {
				return lerr
			}
			mirrors, lerr := listNativeMirrors(ctx, c, repo.ID)
			if lerr != nil {
				return lerr
			}
			nativeRepo = repo
			nativePrimaryHost = clusterHostBySlug(cat)[repo.ClusterSlug.Or("")]
			placements = nativeUsePlacements(repo, mirrors, cat)
			return nil
		}
		ps, lerr := resolvePullablePlacements(ctx, c, repoRef.owner, repoRef.repo)
		if lerr != nil {
			return lerr
		}
		placements = ps
		return nil
	}
	if err := runRemoteLookup(cmd, repoRef, clusterHost, lookup); err != nil {
		return "", err
	}
	if len(placements) == 0 {
		// A native repo always has a primary, so an empty list here
		// means the catalog could not map its slug to a usable host —
		// nativeUsePlacements drops those rather than guessing. Telling
		// that user to create a mirror sends them to fix the wrong
		// thing, so name the cause instead.
		if repoRef.forge == nativeCloneForge {
			return "", fmt.Errorf("%s is on a cluster the catalog cannot resolve to a usable host; check `entire cluster list` for its public URL", qualified)
		}
		return "", fmt.Errorf("%s has no cluster you can fetch from; create a mirror first:\n  entire repo mirror add %s", qualified, qualified)
	}

	// The repo's primary cluster, which a run with no terminal repoints
	// to: a native repo's own cluster, and defaultClusterHost for a
	// GitHub repo, which onboarding always places.
	defaultHost := defaultClusterHost
	if nativeRepo != nil {
		defaultHost = nativePrimaryHost
	}
	chosen, err := selectPlacement(cmd, placements, clusterHost, defaultHost, placementPicker{
		selector: clusterSelectorFlag,
		title:    qualified + " is on more than one cluster — pick the one to use",
		action:   "Remote update",
	})
	if err != nil {
		return "", err
	}
	// chosen.ClusterHost is server-supplied and is about to be written
	// into .git/config, where every later fetch and push dials it
	// carrying the repo-scoped STS token. `git clone` reads
	// "real-host@evil.com" as userinfo, so an unguarded host is a token
	// handed to evil.com — the same vector the clone path guards at its
	// own interpolation, and worse here because this one persists.
	// It also rejects "", which forgeCloneURL would otherwise render as
	// entire:///gh/owner/repo and `git remote add` would accept.
	if err := validateClusterHost(chosen.ClusterHost); err != nil {
		return "", fmt.Errorf("%s has an invalid cluster host %q: %w", qualified, chosen.ClusterHost, err)
	}
	mirrorURL := forgeCloneURL(mirrorCloneForge, chosen.ClusterHost, repoRef.owner, repoRef.repo)
	// A native URL is the server's own path, not a reconstruction from
	// the ref, so `repo view`'s remote and this one cannot disagree.
	if repoRef.forge == nativeCloneForge {
		mirrorURL = nativeRepoURLAt(nativeRepo, chosen.ClusterHost)
	}
	// A native URL needs the repo's server-provided path, which a repo that is
	// still provisioning does not have yet. `git remote set-url` accepts an
	// empty URL and exits 0, so without this the command would report
	// "✓ Repointed remote" while leaving the remote pointing nowhere — the one
	// outcome worse than refusing.
	if mirrorURL == "" {
		return "", fmt.Errorf("%s has no clone URL on %s yet (still provisioning?); no remote was written", qualified, chosen.ClusterHost)
	}
	return mirrorURL, nil
}

// newRepoRemoteCmd is the `entire repo remote` subtree: the git remote of an
// Entire repository. `add` points a remote in the current clone at one.
func newRepoRemoteCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "remote",
		Short: "Work with an Entire repository's git remote",
	}
	cmd.AddCommand(newRepoRemoteAddCmd())
	return requireSubcommand(cmd)
}
func newRepoRemoteAddCmd() *cobra.Command {
	var cluster string
	var override bool
	cmd := &cobra.Command{
		Use:   "add <remote-name> [repo]",
		Short: "Point a git remote in this clone at an Entire cluster",
		Long: "Adds a git remote so fetch and push go through Entire instead of " +
			"the forge.\n\n" +
			"The repo is resolved from the current clone's remotes; pass [repo] to " +
			"name a different one. With more than one cluster to choose from, a " +
			"terminal gets a picker and a script gets the repo's own cluster — " +
			"--cluster selects one either way, the same as `entire repo clone`.\n\n" +
			"<remote-name> must not already exist: as with `git remote add`, an " +
			"occupied name is refused rather than repointed. --override repoints " +
			"it instead, printing the URL it replaced — the only record of it, " +
			"and with any credentials redacted, so keep the original yourself if " +
			"you need it.\n\n" +
			"It only ever edits local git config — the cluster must already serve " +
			"the repo (`entire repo mirror add`); nothing server-side is " +
			"changed.\n\n" + mirrorRepoRefHelp,
		Example: "  entire repo remote add entire\n" +
			"  entire repo remote add entire --cluster aws-us-east-2.entire.io\n" +
			"  entire repo remote add entire /gh/octocat/hello-world\n" +
			"  entire repo remote add origin --override",
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runRepoRemoteAdd(cmd, args, cluster, override)
		},
	}
	cmd.Flags().StringVar(&cluster, "cluster", "", "Cluster host to use when the repo is on several")
	cmd.Flags().BoolVar(&override, "override", false, "Repoint <remote-name> when it already exists instead of refusing")
	return cmd
}
