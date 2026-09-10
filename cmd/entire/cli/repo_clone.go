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
var mirrorCloneRefRe = regexp.MustCompile(`^/?` + mirrorCloneForge + `/` + gitHubOwnerRepoPat + `$`)

// mirrorCloneForge is the path token of GitHub mirrors in entire:// clone URLs
// (`/gh/<owner>/<repo>`), the counterpart of nativeCloneForge. Scoped to the
// ref grammar: mirrorCloneURL still spells the token inline, because its doc
// comment documents that hardcoding as a reviewed assumption about the URL
// template rather than a token to swap.
const mirrorCloneForge = "gh"

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

// nativeProjectRe / nativeRepoRe are the server's project- and repo-name shape,
// as enforced by entiredb `core/resource/project_name.go` (normalizeName, behind
// both the repo-create path and the admin handlers):
//
//   - project: 3–32 characters of letters, digits and '-', no leading or
//     trailing '-'
//   - repo: 1–64 characters, additionally allowing an interior '.' (so
//     "entire-trails.el" is a name), no leading or trailing '.' or '-', and no
//     consecutive dots — that last rule is checked separately in
//     parseNativeCloneRef, since RE2 has no negative lookahead
//
// Uppercase is accepted even though the server rejects it when a name is
// CREATED, because both lookups the native path performs fold case (the project
// through foldProjectName, the repo on lower(name)), so `Paul/DogBark` resolves
// the stored lowercase names.
//
// Matching the server's whole shape rather than just its charset is what keeps
// a ref the server can never match from costing a control-plane round trip:
// `paul/..`, `paul/.foo`, `paul/foo.` and `paul/-x` are all refused locally.
//
// The bounds are server parity and nothing more. They used to carry a second
// job — the 3-character project minimum was what made the removed
// `<project>/<repo>` shorthand unambiguous against the two-character `gh`/`et`
// forge tokens — but a native ref now has to name its token, so the grammar no
// longer rests on them.
//
// Drift is one-directional, which is why the messages above phrase a failure as
// what the server accepts rather than as a rule of ours: if the server tightens
// we are merely lax, but if it loosens (2-char projects, underscores) we refuse
// a ref that would resolve, and only a ULID or a full entire:// URL gets past.
var (
	nativeProjectRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{1,30}[A-Za-z0-9]$`)
	nativeRepoRe    = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9.-]{0,62}[A-Za-z0-9])?$`)
)

// parseNativeCloneRef turns a native clone ref — `/et/<project>/<repo>`,
// leading slash optional — into its project and repo names. The `et/` token is
// required: the bare `<project>/<repo>` shorthand was removed because nothing
// in it says which forge was meant, and both take that shape (#2252).
//
// A non-nil error is not necessarily user-facing: for a ref that never named
// `et/` it only means "try the mirror grammar", and the caller discards it (see
// invalidCloneRefError, which decides when a parser's reason is shown).
func parseNativeCloneRef(ref string) (project, repo string, err error) {
	segs := strings.Split(trimRefPrefix(ref), "/")
	if segs[0] != nativeCloneForge {
		return "", "", fmt.Errorf("not a native ref: expected the %s/ forge token", nativeCloneForge)
	}
	// Counting names rather than segments is what makes `/et/paul` read as a
	// missing <repo> instead of a project named "et".
	names := segs[1:]
	if len(names) != 2 {
		return "", "", fmt.Errorf("expected /%s/<project>/<repo> (2 names after the %s token, got %d)", nativeCloneForge, nativeCloneForge, len(names))
	}
	project, repo = names[0], names[1]
	// Drop `.git` before the name is validated, not after: `.git` alone then
	// fails the shape check as an empty name rather than passing as a dotted
	// one. See gitDirSuffix for why the suffix is never part of a name.
	repo = strings.TrimSuffix(repo, gitDirSuffix)
	if !nativeProjectRe.MatchString(project) {
		return "", "", fmt.Errorf("project %q is not a name the server accepts: 3-32 characters of letters, digits and '-', not starting or ending with '-'", project)
	}
	if !nativeRepoRe.MatchString(repo) || strings.Contains(repo, "..") {
		return "", "", fmt.Errorf("repo %q is not a name the server accepts: 1-64 characters of letters, digits, '.' and '-', not starting or ending with '.' or '-', and with no consecutive dots", repo)
	}
	return project, repo, nil
}

type nativeRepoResolverClient interface {
	repoRefClient
	GetRepo(ctx context.Context, params coreapi.GetRepoParams) (*coreapi.Repo, error)
}

// resolveNativeRepo performs the canonical /et/<project>/<repo> identity
// lookup shared by clone and repo-scoped data commands.
func resolveNativeRepo(ctx context.Context, c nativeRepoResolverClient, project, repoName string) (*coreapi.Repo, error) {
	repoID, err := resolveRepoRef(ctx, c, repoName, project)
	if err != nil {
		return nil, err
	}
	repo, err := c.GetRepo(ctx, coreapi.GetRepoParams{RepoId: repoID})
	if err != nil {
		return nil, fmt.Errorf("get repo: %w", err)
	}
	return repo, nil
}

// resolveNativeCloneURL resolves an Entire-native repo (by project and repo
// name) to its entire:// clone URL: name → ULID via the project-scoped lookup,
// then GetRepo — the one call that returns both clusterHost and path. The URL
// is the server's own coordinates via repoRemoteURL, never synthesized from the
// user's ref. repoName arrives with any `.git` suffix already dropped by the
// parser (see gitDirSuffix).
func resolveNativeCloneURL(ctx context.Context, c *coreapi.Client, project, repoName string) (string, error) {
	repo, err := resolveNativeRepo(ctx, c, project, repoName)
	if err != nil {
		return "", err
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

// gitDirSuffix is the suffix git tools habitually append to a repo path, and
// Entire treats it as never part of a repo name — on either backend. Every ref
// parser in this package drops it before the name is used; `gitremote` trims
// the same suffix independently, because it cannot import this package, so a
// change here has to be mirrored at gitremote.splitOwnerRepo.
//
// It is unsupported rather than merely unusual. GitHub rejects a name ending in
// `.git` outright, so for a mirror the suffix can only ever be decoration. A
// native repo genuinely CAN be named "foo.git" server-side (an interior dot,
// which is also what makes `entire-trails.el` legal), but the CLI reads every
// remote back through gitremote.splitOwnerRepo, which trims the suffix
// unconditionally — so such a repo is unaddressable by name after cloning it
// anyway, in trails, `api`, experts, recap and explain alike. Rather than have
// `repo clone` be the one path that keeps the suffix, the whole CLI drops it,
// and `repo create` refuses to mint a name that ends in it.
//
// The escape hatches for a native "foo.git" that already exists are its ULID
// and the full `entire://` URL, which `repo clone` forwards to git verbatim.
const gitDirSuffix = ".git"

// trimRefPrefix normalizes a ref for segment work: surrounding space gone, one
// optional leading slash gone. Every place that reads a ref's leading token
// goes through it, so the parsers and invalidCloneRefError's dispatch cannot
// disagree about what the first segment is — three near-copies of this, one of
// them missing the TrimSpace, is what made "the token check IS the dispatch
// test" true only because RunE happened to trim first.
func trimRefPrefix(ref string) string {
	return strings.TrimPrefix(strings.TrimSpace(ref), "/")
}

// declaresForge reports whether ref opens with a forge token (leading slash
// optional). Declaring a token is what says the user meant that grammar, so a
// malformed ref gets that parser's reason — the bad owner, project or repo name
// — instead of a list of every shape `repo clone` accepts.
func declaresForge(ref, token string) bool {
	return strings.HasPrefix(trimRefPrefix(ref), token+"/")
}

// bareRefSuggestions returns the forge-qualified refs a bare `<a>/<b>` could
// have meant — one per grammar that would actually accept it, so a suggestion
// is never itself a ref that would fail next. Nil unless ref is exactly two
// slash-separated segments.
//
// This is the input the removed `<project>/<repo>` shorthand used to take
// (#2252), so it is what arrives from habit, and both forges take that shape.
// Naming both candidates is the honest answer: the ambiguity is precisely why
// the shorthand went, so guessing one would reintroduce it, and printing the
// whole grammar makes the reader do the substitution themselves. Re-running the
// parsers rather than re-deriving their charsets is what keeps the two in step.
func bareRefSuggestions(ref string) []string {
	trimmed := trimRefPrefix(ref)
	if strings.Count(trimmed, "/") != 1 {
		return nil
	}
	var out []string
	native := "/" + nativeCloneForge + "/" + trimmed
	if _, _, err := parseNativeCloneRef(native); err == nil {
		out = append(out, native)
	}
	mirror := "/" + mirrorCloneForge + "/" + trimmed
	if _, _, _, err := parseMirrorCloneRef(mirror); err == nil {
		out = append(out, mirror)
	}
	return out
}

// cloneRefShapes lists every ref shape `repo clone` accepts, for error text.
const cloneRefShapes = "/et/<project>/<repo>, /gh/<owner>/<repo>, or a full entire:// URL"

// invalidCloneRefError explains why ref matched none of the clone grammars, in
// four descending degrees of confidence about what the user meant: a ref that
// names github.com is pointed at the `/gh/` form it should have been; a ref
// that declared a forge token keeps the reason its own parser gave (a bad
// owner, project or repo name, a dot-only repo, a missing segment); a bare pair
// that either grammar would accept is offered both readings; and anything left
// gets the list of accepted shapes.
//
// Declaring a token is the whole test for showing a parser's reason: for every
// other ref both parsers failed only because it was not their grammar, so
// neither reason describes anything the user did.
func invalidCloneRefError(ref string, nativeErr, mirrorErr error) error {
	// Host-qualified forms only — an unqualified `x/y` names no forge, and the
	// suggestion branch below handles it without guessing one. A truncated
	// `gh/foo` would otherwise read as owner "gh", and the parser's dot-only
	// guard keeps `github.com/foo/..` out of the hint.
	if owner, repo, err := parseHostedGitHubURL(ref); err == nil {
		return fmt.Errorf("invalid <repo> %q: pass GitHub mirrors as /gh/%s/%s", ref, owner, repo)
	}
	if declaresForge(ref, mirrorCloneForge) {
		return fmt.Errorf("invalid <repo> %q: %w", ref, mirrorErr)
	}
	if declaresForge(ref, nativeCloneForge) {
		return fmt.Errorf("invalid <repo> %q: %w", ref, nativeErr)
	}
	if s := bareRefSuggestions(ref); len(s) > 0 {
		return fmt.Errorf("invalid <repo> %q: a ref must name its forge — did you mean %s?", ref, strings.Join(s, " or "))
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
	// Drop `.git` (see gitDirSuffix) BEFORE the dot-only guard, which is what
	// keeps `..git` — not dot-only as typed — from resolving to a "." repo.
	repo = strings.TrimSuffix(repo, gitDirSuffix)
	if repo == "" {
		return "", "", "", fmt.Errorf("repo name is empty once the %s suffix is dropped: %s", gitDirSuffix, ref)
	}
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
		Long: "Clone an Entire-native repo by its `/et/<project>/<repo>` ref, a " +
			"GitHub mirror by its `/gh/<owner>/<repo>` ref, or a full `entire://` " +
			"clone URL.\n\n" +
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
			project, repoName, nativeErr := parseNativeCloneRef(ref)
			if nativeErr == nil {
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
			_, owner, repo, mirrorErr := parseMirrorCloneRef(ref)
			if mirrorErr != nil {
				return invalidCloneRefError(ref, nativeErr, mirrorErr)
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
