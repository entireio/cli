package cli

import (
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"sort"
	"strings"

	"charm.land/huh/v2"
	"github.com/spf13/cobra"

	"github.com/entireio/cli/cmd/entire/cli/interactive"
	"github.com/entireio/cli/internal/coreapi"
)

// mirrorCloneRefRe parses the clone-ref shape `entire repo clone` accepts:
// the `/gh/<owner>/<repo>` path of a mirror's clone URL, with or without the
// leading slash. owner/repo reuse the GitHub identifier charsets from
// parseGitHubURL so the same metacharacter vectors are closed at the boundary
// (owner/repo flow unescaped into the synthesised entire:// clone URL).
var mirrorCloneRefRe = regexp.MustCompile(`^/?gh/` + gitHubOwnerPat + `/` + gitHubRepoPat + `$`)

// mirrorCloneProviderGitHub is the upstream provider the `gh` path token maps to
// — the value the control plane records and the list API filters on. Kept local
// to the clone path so the provider mapping is self-contained rather than
// borrowing a constant named for an unrelated (checkpoint) concern.
const mirrorCloneProviderGitHub = "github"

// entireCloneURLScheme is the scheme of a full mirror clone URL, which
// git-remote-entire resolves directly. Such a URL already names the cluster, so
// `repo clone` passes it through to `git clone` untouched.
const entireCloneURLScheme = "entire://"

// isEntireCloneURL reports whether ref is a full entire:// clone URL (vs. the
// `/gh/<owner>/<repo>` shorthand that needs a mirror lookup).
func isEntireCloneURL(ref string) bool {
	return strings.HasPrefix(strings.TrimSpace(ref), entireCloneURLScheme)
}

// mirrorCloneURL synthesizes the entire:// clone URL for a GitHub mirror from
// its cluster host and owner/repo — the form `git clone` accepts, which the
// mirror list API doesn't return. Shared by the mirror table view (mirrorRow)
// and `repo clone` so the wire format lives in one place.
//
// HARDCODED ASSUMPTIONS (revisit if either stops holding):
//   - The provider path segment is fixed to "gh". Mirrors are GitHub-only
//     today; the list/index API (RepoPlacement) records no provider, so there
//     is nothing to key off. If a non-GitHub provider ever lands, this must
//     take a provider argument or the URL will lie.
//   - This is a client-side RECONSTRUCTION, not the server's canonical URL.
//     The list index gives only a cluster slug, so callers resolve slug->host
//     (clusterHostBySlug, via a separate ListClusters call) before calling
//     this. If /repos ever returns the clone URL (or host+provider) directly,
//     drop this synthesis and that extra round-trip.
func mirrorCloneURL(host, owner, repo string) string {
	return fmt.Sprintf("%s%s/gh/%s/%s", entireCloneURLScheme, host, owner, repo)
}

// nativeCloneForge is the path token of Entire-native repos in entire:// clone
// URLs (`/et/<project>/<repo>`), mirroring the server's repourls.URLPathPrefix.
const nativeCloneForge = "et"

// nativeProjectRe / nativeRepoRe are the server's project- and repo-name
// charsets (entiredb core/resource/project_name.go: letters, digits, '-', plus
// '.' for repos; lookups fold case, so uppercase is accepted here). Length and
// edge-character rules are left to the server — this check only has to tell a
// native ref apart from things that are not one.
var (
	nativeProjectRe = regexp.MustCompile(`^[A-Za-z0-9-]+$`)
	nativeRepoRe    = regexp.MustCompile(`^[A-Za-z0-9.-]+$`)
)

// parseNativeCloneRef turns a native clone ref — `/et/<project>/<repo>` or the
// `<project>/<repo>` shorthand, leading slash optional — into its project and
// repo names. Not a match (ok == false) is not an error: the caller falls
// through to the mirror grammar. The `gh`/`et` forge tokens are never valid
// project names (3–32 chars server-side), so a two-segment ref starting with
// one is a malformed forge ref, not a shorthand, and is rejected here so the
// mirror parser can name the expected shape. Segments must match the server's
// name charsets: without that, any "x/y" — `git@github.com:foo/bar` included —
// parsed as a shorthand and produced a control-plane lookup error instead of a
// hint about the accepted shapes.
func parseNativeCloneRef(ref string) (project, repo string, ok bool) {
	segs := strings.Split(strings.TrimPrefix(strings.TrimSpace(ref), "/"), "/")
	if len(segs) == 3 && segs[0] == nativeCloneForge {
		segs = segs[1:]
	}
	if len(segs) != 2 || !nativeProjectRe.MatchString(segs[0]) || !nativeRepoRe.MatchString(segs[1]) {
		return "", "", false
	}
	if segs[0] == nativeCloneForge || segs[0] == "gh" {
		return "", "", false
	}
	return segs[0], segs[1], true
}

// resolveNativeCloneURL resolves an Entire-native repo (by project and repo
// name) to its entire:// clone URL: name → ULID via the project-scoped lookup,
// then GetRepo — the one call that returns both clusterHost and path. The URL
// is the server's own coordinates via repoRemoteURL, never synthesized from
// the user's ref (note: no .git suffix — the server strips it for /gh/ paths
// only, so a /et/ path with it would 404).
func resolveNativeCloneURL(ctx context.Context, c *coreapi.Client, project, repoName string) (string, error) {
	repoID, err := resolveRepoRef(ctx, c, repoName, project)
	if err != nil {
		return "", err
	}
	repo, err := c.GetRepo(ctx, coreapi.GetRepoParams{RepoId: repoID})
	if err != nil {
		return "", fmt.Errorf("get repo: %w", err)
	}
	cloneURL := repoRemoteURL(*repo)
	if cloneURL == "" {
		return "", fmt.Errorf("repo %s/%s has no clone URL yet (still provisioning?)", project, repoName)
	}
	// The host is server-provided but interpolated into the entire:// clone URL,
	// so apply the same anti-token-leak guard as the mirror path (see the
	// validateClusterHost call on the /gh/ branch).
	if err := validateClusterHost(repo.ClusterHost.Or("")); err != nil {
		return "", fmt.Errorf("repo has an invalid cluster host %q: %w", repo.ClusterHost.Or(""), err)
	}
	return cloneURL, nil
}

// mirrorCloneRefPrefixRe matches the `gh/` forge token (leading slash optional)
// that says the user meant a mirror ref, so a malformed one gets an error about
// its owner/repo rather than a list of every shape `repo clone` accepts.
var mirrorCloneRefPrefixRe = regexp.MustCompile(`^/?gh/`)

// cloneRefShapes lists every ref shape `repo clone` accepts, for error text.
const cloneRefShapes = "/et/<project>/<repo>, <project>/<repo>, /gh/<owner>/<repo>, or a full entire:// URL"

// invalidCloneRefError explains why ref matched none of the clone grammars.
// A GitHub URL gets pointed at the `/gh/` form it should have been; a `gh/`
// ref that the mirror parser rejected keeps the parser's reason (bad owner or
// repo charset, dot-only repo, missing segment), and anything else gets the
// list of accepted shapes.
func invalidCloneRefError(ref string, mirrorErr error) error {
	// Hosted forms only: bare `x/y` is what the native shorthand already
	// rejected, and a truncated `gh/foo` would otherwise read as owner "gh".
	// The parser's dot-only guard also keeps `foo/..` out of the hint.
	if owner, repo, err := parseHostedGitHubURL(ref); err == nil {
		return fmt.Errorf("invalid <repo> %q: pass GitHub mirrors as /gh/%s/%s", ref, owner, repo)
	}
	if mirrorCloneRefPrefixRe.MatchString(ref) {
		return fmt.Errorf("invalid <repo> %q: %w", ref, mirrorErr)
	}
	return fmt.Errorf("invalid <repo> %q: expected %s", ref, cloneRefShapes)
}

// parseMirrorCloneRef turns a clone ref like `/gh/entirehq/entire-api` into the
// API provider ("github") and the lowercased owner/repo. The `gh` token is the
// path provider used in entire:// clone URLs; it maps to the "github" upstream
// provider the control plane records.
func parseMirrorCloneRef(ref string) (provider, owner, repo string, err error) {
	m := mirrorCloneRefRe.FindStringSubmatch(strings.TrimSpace(ref))
	if m == nil {
		return "", "", "", fmt.Errorf("expected gh/<owner>/<repo> (leading slash optional; owner: letters, digits, '-'; repo: letters, digits, '.', '_', '-'), got %q", ref)
	}
	owner, repo = strings.ToLower(m[1]), strings.ToLower(m[2])
	if gitHubDotOnlyRe.MatchString(repo) {
		return "", "", "", fmt.Errorf("repo cannot be dot-only: %s", ref)
	}
	return mirrorCloneProviderGitHub, owner, repo, nil
}

func newRepoCloneCmd() *cobra.Command {
	var cluster string
	cmd := &cobra.Command{
		Use:   "clone <repo> [target-dir]",
		Short: "Clone an Entire repository",
		Long: "Clone an Entire-native repo by its `/et/<project>/<repo>` ref (or the " +
			"`<project>/<repo>` shorthand), a GitHub mirror by its `/gh/<owner>/<repo>` " +
			"ref, or a full `entire://` clone URL.\n\n" +
			"A native ref resolves the repo's home cluster and clones from there " +
			"(--cluster doesn't apply).\n\n" +
			"With a `/gh/<owner>/<repo>` ref, looks up where the repo is mirrored: if " +
			"it's on a single cluster, clones it directly; if it's mirrored on more " +
			"than one, prompts you to pick which to clone from (or pass --cluster to " +
			"choose non-interactively).\n\n" +
			"A full `entire://` URL already names the cluster, so it's passed straight " +
			"through to `git clone` with no lookup (and --cluster is ignored). The " +
			"optional [target-dir] is passed through to `git clone` either way.",
		Example: "  entire repo clone /et/project/example\n" +
			"  entire repo clone project/example\n" +
			"  entire repo clone /gh/entirehq/entire-api\n" +
			"  entire repo clone /gh/entirehq/entire-api ./entire-api\n" +
			"  entire repo clone /gh/entirehq/entire-api --cluster aws-us-east-2.entire.io\n" +
			"  entire repo clone entire://aws-us-east-2.entire.io/gh/entirehq/entire-api",
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			cmd.SilenceUsage = true
			// Trim once up front so the entire:// detection and the value forwarded
			// to git clone agree (the shorthand path trims inside parseMirrorCloneRef).
			ref := strings.TrimSpace(args[0])
			var targetDir string
			if len(args) > 1 {
				targetDir = args[1]
			}

			// A full entire:// clone URL already embeds the cluster host (it's what
			// --cluster would otherwise resolve to), so pass it verbatim to git clone
			// — no mirror lookup or cluster resolution. --cluster is irrelevant here.
			//
			// Deliberately NOT run through validateClusterHost: this is a raw URL the
			// user typed, forwarded to `git clone` exactly as given (the whole point
			// of this branch), so it's equivalent to running `git clone entire://…`
			// directly. The validateClusterHost guard applies on the shorthand path
			// where we *synthesize* the URL from a --cluster flag or an API-supplied
			// host — values that flow into the STS audience under our own construction.
			if isEntireCloneURL(ref) {
				return runGitClone(cmd.Context(), cmd, ref, targetDir)
			}

			// Native ref: resolve the repo's home cluster via the active-context
			// control plane and clone from there. A native repo lives on exactly
			// one home cluster, so --cluster has nothing to choose between.
			if project, repoName, ok := parseNativeCloneRef(ref); ok {
				if cluster != "" {
					return fmt.Errorf("--cluster applies to /gh/ mirror refs; %s/%s is cloned from its home cluster", project, repoName)
				}
				var cloneURL string
				if err := runCore(cmd, func(ctx context.Context, c *coreapi.Client) error {
					url, err := resolveNativeCloneURL(ctx, c, project, repoName)
					if err != nil {
						return err
					}
					cloneURL = url
					return nil
				}); err != nil {
					return err
				}
				return runGitClone(cmd.Context(), cmd, cloneURL, targetDir)
			}

			// provider is always "github" for the /gh/ shorthand; the pull-gated
			// resolver pins the provider itself, so it's not threaded through.
			_, owner, repo, err := parseMirrorCloneRef(ref)
			if err != nil {
				return invalidCloneRefError(ref, err)
			}

			var placements []coreapi.ResolvedPlacement
			lister := func(ctx context.Context, c *coreapi.Client) error {
				ps, err := resolvePullablePlacements(ctx, c, owner, repo)
				if err != nil {
					return err
				}
				placements = ps
				return nil
			}
			// An explicit --cluster may name a cluster in a different federation
			// than the active context, whose mirrors the active-context core can't
			// see (the original bug: cloning a royalcanin.partial.to mirror while a
			// different context is active failed with "not mirrored on ..."). Dial
			// the core fronting that cluster — discovered from its well-known and
			// authenticated with the matching local context, the same path
			// `mirror create <url> [cluster]` uses — so the lookup resolves against
			// the right federation. With no --cluster, list from the active context.
			runWithCore := runCore
			if cluster != "" {
				if err := validateClusterHost(cluster); err != nil {
					return fmt.Errorf("invalid --cluster: %w", err)
				}
				runWithCore = func(cmd *cobra.Command, fn func(context.Context, *coreapi.Client) error) error {
					return runCoreForCluster(cmd, cluster, fn)
				}
			}
			if err := runWithCore(cmd, lister); err != nil {
				return err
			}

			if len(placements) == 0 {
				return fmt.Errorf("no mirror found for /gh/%s/%s; run 'entire repo mirror create github.com/%s/%s' to onboard it", owner, repo, owner, repo)
			}

			chosen, err := selectCloneTarget(cmd, placements, cluster)
			if err != nil {
				return err
			}

			// chosen.ClusterHost is server-provided, but it's interpolated into the
			// entire:// clone URL just like the user-supplied --cluster, so apply the
			// same anti-token-leak guard (validateClusterHost) before building it —
			// defense-in-depth against a malformed host reaching git / the STS audience.
			if err := validateClusterHost(chosen.ClusterHost); err != nil {
				return fmt.Errorf("mirror has an invalid cluster host %q: %w", chosen.ClusterHost, err)
			}
			cloneURL := mirrorCloneURL(chosen.ClusterHost, owner, repo)
			return runGitClone(cmd.Context(), cmd, cloneURL, targetDir)
		},
	}
	cmd.Flags().StringVar(&cluster, "cluster", "", "Cluster host to clone from when the repo is mirrored on more than one (may belong to another auth context)")
	return cmd
}

// mirrorLister is the subset of the control-plane client listMirrorsForRepo
// needs. Narrowing to an interface lets callers (e.g. resolveCurrentRepoID in
// api_cmd.go, and entireapi_client.go's currentRepoRef) inject a fake control
// plane in tests; *coreapi.Client satisfies it.
type mirrorLister interface {
	ListMirrors(ctx context.Context, params coreapi.ListMirrorsParams) (*coreapi.ListMirrorsOutputBody, error)
}

// listMirrorsForRepo returns every mirror placement of one upstream repo across
// clusters. The list API filters by provider+owner server-side but has no repo
// filter, so the repo match is applied client-side (owner is already lowercased
// to match what the server persists).
func listMirrorsForRepo(ctx context.Context, c mirrorLister, provider, owner, repo string) ([]coreapi.Mirror, error) {
	all, err := fetchAllPages(ctx, func(ctx context.Context, cursor string) ([]coreapi.Mirror, string, error) {
		params := coreapi.ListMirrorsParams{
			Provider: coreapi.NewOptString(provider),
			Owner:    coreapi.NewOptString(owner),
		}
		if cursor != "" {
			params.PageToken = coreapi.NewOptString(cursor)
		}
		out, err := c.ListMirrors(ctx, params)
		if err != nil {
			return nil, "", fmt.Errorf("list mirrors: %w", err)
		}
		return out.Mirrors, out.NextPageToken.Or(""), nil
	})
	if err != nil {
		return nil, err
	}
	matched := make([]coreapi.Mirror, 0, len(all))
	for _, m := range all {
		if strings.EqualFold(m.Repo, repo) {
			matched = append(matched, m)
		}
	}
	return matched, nil
}

// resolvePullablePlacements returns every cluster placement of one GitHub
// upstream the caller may pull (clone). It backs `repo clone /gh/<owner>/<repo>`
// and deliberately differs from listMirrorsForRepo: that reads the
// affiliation-scoped mirror list (repo#list), which omits public mirrors the
// caller holds no grant on, so the shorthand used to fail on a public repo that
// clones fine by full entire:// URL. This hits the pull-gated /mirrors/placements
// endpoint instead — the same authority the clone's STS exchange enforces — so
// anything clonable resolves, public or private-with-grant.
//
// owner/repo arrive already lowercased from parseMirrorCloneRef; the server
// matches case-insensitively regardless. An empty result means not mirrored or
// not pullable, and the caller surfaces that.
func resolvePullablePlacements(ctx context.Context, c *coreapi.Client, owner, repo string) ([]coreapi.ResolvedPlacement, error) {
	out, err := c.ResolveMirrorPlacements(ctx, coreapi.ResolveMirrorPlacementsParams{
		Provider: coreapi.ResolveMirrorPlacementsProviderGithub,
		Owner:    owner,
		Repo:     repo,
	})
	if err != nil {
		return nil, fmt.Errorf("resolve mirror placements: %w", err)
	}
	return out.Placements, nil
}

// placementPicker adapts selectPlacement's messages to the calling verb. The
// picker logic is identical for every consumer (dedupe by host, honor an
// explicit selector, prompt only when there's a real choice); only the words
// differ, so they're passed in rather than duplicated per command.
type placementPicker struct {
	// selector names the non-interactive way to choose a cluster, as the user
	// would type it (e.g. `--cluster`). Interpolated into the no-terminal error
	// so the pointer names a flag the calling command actually accepts.
	selector string
	// title is the interactive single-select's prompt.
	title string
	// action names the operation in the cancellation message, capitalized
	// ("Clone", "Remote update") — handleFormCancellation prints
	// "<action> cancelled."
	action string
}

// selectCloneTarget resolves which mirror placement to clone from, with the
// clone verb's wording. See selectPlacement for the selection rules.
func selectCloneTarget(cmd *cobra.Command, placements []coreapi.ResolvedPlacement, clusterFlag string) (coreapi.ResolvedPlacement, error) {
	return selectPlacement(cmd, placements, clusterFlag, placementPicker{
		selector: "--cluster",
		title:    "This repo is mirrored on more than one cluster — pick one to clone from",
		action:   "Clone",
	})
}

// selectPlacement resolves which mirror placement a verb should act on. With one
// placement it returns it directly. With an explicit clusterSel it picks the
// matching one (or errors listing the available hosts). With more than one and no
// selector it prompts interactively, failing fast with a p.selector pointer when
// there's no terminal.
func selectPlacement(cmd *cobra.Command, placements []coreapi.ResolvedPlacement, clusterSel string, p placementPicker) (coreapi.ResolvedPlacement, error) {
	// Dedupe by cluster host: one placement per cluster is what a caller acts on,
	// and the same host appearing twice would only confuse the picker. Key on the
	// case-folded host — DNS is case-insensitive, so a selector value differing
	// only in case from the API's ClusterHost must still match (the alternative is
	// a misleading "not mirrored on ..." after a successful lookup + dial).
	byHost := make(map[string]coreapi.ResolvedPlacement, len(placements))
	hosts := make([]string, 0, len(placements))
	for _, p := range placements {
		key := strings.ToLower(p.ClusterHost)
		if _, seen := byHost[key]; seen {
			continue
		}
		byHost[key] = p
		hosts = append(hosts, key)
	}
	sort.Strings(hosts)

	if clusterSel != "" {
		match, ok := byHost[strings.ToLower(strings.TrimSpace(clusterSel))]
		if !ok {
			return coreapi.ResolvedPlacement{}, fmt.Errorf("repo is not mirrored on %q; available: %s", clusterSel, strings.Join(hosts, ", "))
		}
		return match, nil
	}

	if len(hosts) == 1 {
		return byHost[hosts[0]], nil
	}

	if !interactive.CanPromptInteractively() {
		return coreapi.ResolvedPlacement{}, fmt.Errorf("repo is mirrored on %d clusters; pass %s to choose one of: %s", len(hosts), p.selector, strings.Join(hosts, ", "))
	}

	options := make([]huh.Option[string], len(hosts))
	for i, h := range hosts {
		options[i] = huh.NewOption(mirrorCellLabel(byHost[h]), h)
	}
	var selected string
	form := NewAccessibleForm(
		huh.NewGroup(
			huh.NewSelect[string]().
				Title(p.title).
				Options(options...).
				Value(&selected),
		),
	)
	if err := form.RunWithContext(cmd.Context()); err != nil {
		// handleFormCancellation prints "<action> cancelled." and returns nil for a
		// Ctrl+C / cancelled-context abort. Surface that as a SilentError so the
		// caller stops instead of falling through to act on a zero-value target
		// (the `entire:///gh/...` empty-host bug) without main.go reprinting the
		// message handleFormCancellation already wrote; a real form error propagates.
		if cerr := handleFormCancellation(cmd.ErrOrStderr(), p.action, err); cerr != nil {
			return coreapi.ResolvedPlacement{}, cerr
		}
		return coreapi.ResolvedPlacement{}, NewSilentError(fmt.Errorf("%s cancelled", strings.ToLower(p.action)))
	}
	match, ok := byHost[selected]
	if !ok {
		// The form succeeded but handed back a host that is not on offer. Nothing
		// has been printed here, so this must NOT be a SilentError — main.go
		// suppresses those, and the command would exit non-zero with no message.
		return coreapi.ResolvedPlacement{}, fmt.Errorf("no cluster selected from the %d offered", len(hosts))
	}
	return match, nil
}

// mirrorCellLabel is the human label for a mirror placement in the clone picker:
// the physical cell and jurisdiction when known, always anchored by the cluster
// host that goes into the clone URL.
func mirrorCellLabel(p coreapi.ResolvedPlacement) string {
	cell := strings.TrimSpace(p.Cell.Or(""))
	jur := strings.TrimSpace(p.Jurisdiction.Or(""))
	switch {
	case cell != "" && jur != "":
		return fmt.Sprintf("%s (%s) — %s", cell, jur, p.ClusterHost)
	case cell != "":
		return fmt.Sprintf("%s — %s", cell, p.ClusterHost)
	default:
		return p.ClusterHost
	}
}

// runGitClone shells out to `git clone <cloneURL> [target-dir]`, wiring the
// child's stdio through so git-remote-entire's auth prompts and clone progress
// reach the user. A clone failure is wrapped as a SilentError: git already
// printed its own diagnostics, so main.go shouldn't reprint the wrapper.
func runGitClone(ctx context.Context, cmd *cobra.Command, cloneURL, targetDir string) error {
	args := []string{"clone", cloneURL}
	if targetDir != "" {
		args = append(args, targetDir)
	}
	fmt.Fprintf(cmd.ErrOrStderr(), "Cloning %s\n", cloneURL)
	gitCmd := exec.CommandContext(ctx, "git", args...)
	gitCmd.Stdin = cmd.InOrStdin()
	gitCmd.Stdout = cmd.OutOrStdout()
	gitCmd.Stderr = cmd.ErrOrStderr()
	if err := gitCmd.Run(); err != nil {
		return NewSilentError(fmt.Errorf("git clone failed: %w", err))
	}
	return nil
}
