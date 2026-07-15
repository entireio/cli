package ci

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/entireio/cli/internal/coreapi"
)

// ResolveNativeRepo turns a native repo reference into its entiredb repo ULID,
// the identifier the `entire ci buildkite` verbs address a repo by. Accepted
// shapes:
//
//   - a raw 26-character ULID — round-tripped unchanged after a shape check,
//     never hitting the network;
//   - `<project>/<repo>` or `/et/<project>/<repo>` — a native (entire-git) repo,
//     resolved via the control plane: the project name → its ULID, then the repo
//     name within that project → its ULID.
//
// GitHub mirrors (`gh/<owner>/<repo>`, `github.com/<owner>/<repo>`) are rejected
// with an actionable error. The ci-webhooks backend enrolls native, org-owned
// `/et/` repos only — a mirror authorizes via GitHub permissions, not an Entire
// repo grant, so a CI pipeline could never clone it. We reject the shape loudly
// here rather than enrolling a pipeline that can never run.
//
// Both lookups use the control plane's O(1) case-insensitive by-name filter
// (?name=), the same one `entire project list --name` and `resolveRepoRef` use;
// the CLI never lists everything and matches client-side.
func ResolveNativeRepo(ctx context.Context, c *coreapi.Client, ref string) (repoULID string, err error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return "", errors.New("repo reference is empty")
	}
	if looksLikeULID(ref) {
		return ref, nil
	}
	if looksLikeMirrorRef(ref) {
		return "", fmt.Errorf("%q is a GitHub mirror path: Buildkite CI supports native /et/ repos only, not GitHub mirrors — pass a native repo as <project>/<repo>", ref)
	}
	projectName, repoName, ok := splitNativePath(ref)
	if !ok {
		return "", fmt.Errorf("invalid repo reference %q: expected a native path <project>/<repo> (or /et/<project>/<repo>), or a 26-character repo ULID", ref)
	}

	projectID, err := resolveProjectByName(ctx, c, projectName)
	if err != nil {
		return "", err
	}

	repos, err := c.ListProjectRepos(ctx, coreapi.ListProjectReposParams{
		ProjectId: projectID,
		Name:      coreapi.NewOptString(repoName),
	})
	if err != nil {
		if isCoreNotFound(err) {
			return "", noRepoNamedErr(repoName, projectName)
		}
		return "", err
	}
	// A name-filtered list returns the single match under the singular `repo`
	// field (the plural `repos` is only populated for an unfiltered page), so an
	// unset `repo` means no match — same contract as the project/org lookups.
	repo, ok := repos.Repo.Get()
	if !ok || repo.ID == "" {
		return "", noRepoNamedErr(repoName, projectName)
	}
	return repo.ID, nil
}

// ResolveOrg turns an org reference into its org ULID — the identifier the
// `entire ci buildkite org` verbs address an org by. A raw 26-character ULID is
// round-tripped unchanged after a shape check (never hitting the network); any
// other value is treated as an org name and resolved via the control plane's
// O(1) case-insensitive by-name filter (?name=), the same one resolveOrgRef and
// `entire org list --name` use. It mirrors ResolveNativeRepo's ULID-or-name
// contract and lives beside it so the two resolvers stay consistent.
func ResolveOrg(ctx context.Context, c *coreapi.Client, ref string) (orgULID string, err error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return "", errors.New("org reference is empty")
	}
	if looksLikeULID(ref) {
		return ref, nil
	}
	out, err := c.ListOrgs(ctx, coreapi.ListOrgsParams{Name: coreapi.NewOptString(ref)})
	if err != nil {
		if isCoreNotFound(err) {
			return "", noOrgNamedErr(ref)
		}
		return "", err
	}
	// A name-filtered list returns the single match under the singular `org`
	// field (the plural `orgs` is only populated for an unfiltered page), so an
	// unset `org` means no match — same contract as the project/repo lookups.
	org, ok := out.Response.Org.Get()
	if !ok || org.ID == "" {
		return "", noOrgNamedErr(ref)
	}
	return org.ID, nil
}

// resolveProjectByName resolves a project name to its ULID via the control
// plane's case-insensitive by-name filter, mapping "not found" (a 404 or an
// unset match) to a friendly error.
func resolveProjectByName(ctx context.Context, c *coreapi.Client, name string) (string, error) {
	projects, err := c.ListProjects(ctx, coreapi.ListProjectsParams{Name: coreapi.NewOptString(name)})
	if err != nil {
		if isCoreNotFound(err) {
			return "", noProjectNamedErr(name)
		}
		return "", err
	}
	project, ok := projects.Project.Get()
	if !ok || project.ID == "" {
		return "", noProjectNamedErr(name)
	}
	return project.ID, nil
}

// splitNativePath splits a native repo path into its project and repo segments,
// tolerating the `/et/` namespace prefix that mirrors the canonical
// `entire://<cluster>/et/<project>/<repo>` URL form. A bare leading slash is
// also tolerated. It reports ok=false for anything that isn't exactly
// `<project>/<repo>` after prefix stripping.
func splitNativePath(ref string) (project, repo string, ok bool) {
	path := ref
	switch {
	case strings.HasPrefix(path, "/et/"):
		path = strings.TrimPrefix(path, "/et/")
	case strings.HasPrefix(path, "/"):
		path = strings.TrimPrefix(path, "/")
	}
	project, repo, found := strings.Cut(path, "/")
	if !found || project == "" || repo == "" || strings.Contains(repo, "/") {
		return "", "", false
	}
	return project, repo, true
}

// looksLikeMirrorRef reports whether ref is an explicit GitHub-mirror path —
// `gh/<…>` or `github.com/<…>`, with or without a leading slash (the
// `entire repo clone /gh/…` form). A bare `<owner>/<repo>` is NOT a mirror ref:
// it is ambiguous with the native `<project>/<repo>` shape, so mirrors must be
// explicitly prefixed. Used only to reject the shape with a clear message; CI
// does not resolve mirrors.
func looksLikeMirrorRef(ref string) bool {
	for _, p := range []string{"gh/", "/gh/", "github.com/", "/github.com/"} {
		if strings.HasPrefix(ref, p) {
			return true
		}
	}
	return false
}

// looksLikeULID reports whether s has the shape of a ULID: 26 characters
// drawn from Crockford base32 (digits plus uppercase letters, excluding I, L,
// O, U). The check is shape-only and case-insensitive on the alphabet; it never
// hits the network. Entire ULIDs carry no type prefix, so this is the light
// validity check a raw repo or org ULID passes before being round-tripped
// unchanged.
//
// Replicated from cli.looksLikeULID rather than imported: the cli package
// imports this package (root.go calls ci.Register), so importing cli back would
// form an import cycle — the same reason the scaffold replicates
// addControlPlaneFlags.
func looksLikeULID(s string) bool {
	if len(s) != 26 {
		return false
	}
	for _, r := range strings.ToUpper(s) {
		switch {
		case r >= '0' && r <= '9':
		case r >= 'A' && r <= 'Z' && r != 'I' && r != 'L' && r != 'O' && r != 'U':
		default:
			return false
		}
	}
	return true
}

// isCoreNotFound reports whether err is a control-plane 404. The by-name lookups
// return 404 when nothing matches; callers turn that into a friendly "no X
// named" message. Replicated from cli.isCoreNotFound to avoid the import cycle.
func isCoreNotFound(err error) bool {
	var se *coreapi.ErrorModelStatusCode
	return errors.As(err, &se) && se.StatusCode == http.StatusNotFound
}

func noOrgNamedErr(name string) error {
	return fmt.Errorf("no org named %q (run `entire org list` to see names, or pass an org ULID)", name)
}

func noProjectNamedErr(name string) error {
	return fmt.Errorf("no project named %q (run `entire project list` to see names, or pass a repo ULID)", name)
}

func noRepoNamedErr(repo, project string) error {
	return fmt.Errorf("no repo named %q in project %q (run `entire repo list %s` to see names, or pass a repo ULID)", repo, project, project)
}
