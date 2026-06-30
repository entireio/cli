package cli

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/entireio/cli/internal/coreapi"
)

// Control-plane commands reference orgs and projects by their parent ULID in
// many places (repo create --project, project create --owner, grant org/project
// <id>, …). ULIDs are unfriendly to type, so these refs also accept a human
// name: looksLikeULID decides which form was given, and the resolveXRef helpers
// turn a name into its ULID. A ULID is always passed straight through with no
// network call. A name is resolved by the control plane's O(1), case-insensitive
// by-name lookup (the server matches on lower(name) and returns the single match
// under the response's singular `org`/`project` field, or 404) — the CLI never
// lists everything and filters client-side.

// providerGitHub is the identity-provider slug for GitHub-backed accounts, the
// provider half of a qualified grantee handle like "github:alice". GitHub is the
// only provider with backing accounts today; other slugs resolve once they exist
// server-side. (Distinct from setup.go's checkpointProviderGitHub, which names
// the checkpoint hosting provider — same string, unrelated concern.)
const providerGitHub = "github"

// looksLikeULID reports whether s has the shape of a ULID: 26 characters drawn
// from Crockford base32 (digits plus uppercase letters, excluding I, L, O, U).
// The check is shape-only and case-insensitive on the alphabet; it never hits
// the network. A name that happened to be 26 valid base32 characters would be
// misread as an id, but real org/project names don't take that form, and the
// user can always fall back to the explicit ULID.
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
// (ListOrgs/ListProjects/ListOrgProjects with ?name=) return 404 when nothing
// matches; callers turn that into a friendly "no X named" message.
func isCoreNotFound(err error) bool {
	var se *coreapi.ErrorModelStatusCode
	return errors.As(err, &se) && se.StatusCode == http.StatusNotFound
}

// resolveOrgRef turns an org reference (ULID or name) into its ULID. A ULID is
// returned unchanged; a name is resolved via the server's case-insensitive
// by-name lookup.
func resolveOrgRef(ctx context.Context, c *coreapi.Client, ref string) (string, error) {
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
	org, ok := out.Org.Get()
	if !ok {
		return "", noOrgNamedErr(ref)
	}
	return org.ID, nil
}

// resolveAccountRef turns an account reference into its ULID. A ULID passes
// through unchanged; otherwise the ref is a provider-qualified handle (e.g.
// "github:alice") resolved via the control plane. We support github-backed
// user accounts today; other providers will resolve once they exist server-side.
func resolveAccountRef(ctx context.Context, c *coreapi.Client, ref string) (string, error) {
	if looksLikeULID(ref) {
		return ref, nil
	}
	provider, handle, err := parseQualifiedHandle(ref)
	if err != nil {
		return "", err
	}
	id, err := c.ResolveHandle(ctx, coreapi.ResolveHandleParams{Provider: provider, Handle: handle})
	if err != nil {
		return "", err
	}
	// ResolvedIdentity.AccountId is a plain string, so a handle that resolves to
	// an identity with no backing account would silently forward "" as the owner
	// ULID and fail later with an opaque server-side create error. Catch it here.
	if id.AccountId == "" {
		return "", fmt.Errorf("handle %q resolved to no account", ref)
	}
	return id.AccountId, nil
}

// resolveGranteeProvider turns a grantee reference into the (provider,
// providerUserId) pair the grant/membership "by provider" routes key on. The
// reference is a provider-qualified handle (e.g. "github:alice"); it is
// resolved through the control plane to the provider's stable numeric user id.
// The friendly handle alone is not what the grant routes accept — passing it as
// --provider-user-id was the COR-699 footgun ("provider identity not found") —
// so the CLI always resolves it first. A bare account ULID is rejected here:
// the by-provider routes can't be addressed by ULID, and there is no reverse
// account→provider-id lookup; callers that accept a ULID grantee (project/repo
// remove) handle it via the typed-id route before reaching this helper.
func resolveGranteeProvider(ctx context.Context, c *coreapi.Client, ref string) (provider, providerUserID string, err error) {
	// A ULID is a tempting paste from `grant … list` (which prints the grantee
	// ID), but the by-provider routes can't be addressed by ULID. Reject it with
	// a message that points at the form this command actually wants, rather than
	// letting parseQualifiedHandle dangle a "(or a ULID)" hint that doesn't apply.
	if looksLikeULID(ref) {
		return "", "", fmt.Errorf("grantee %q is an account ULID; this command needs a provider-qualified handle like \"github:alice\"", ref)
	}
	p, handle, err := parseQualifiedHandle(ref)
	if err != nil {
		return "", "", err
	}
	id, err := c.ResolveHandle(ctx, coreapi.ResolveHandleParams{Provider: p, Handle: handle})
	if err != nil {
		if isCoreNotFound(err) {
			return "", "", fmt.Errorf("no %s identity for handle %q", p, handle)
		}
		return "", "", err
	}
	if id.ProviderUserId == "" {
		return "", "", fmt.Errorf("handle %q resolved to no provider user id", ref)
	}
	// Prefer the server-normalized provider over the raw prefix, falling back to
	// the input when the response omits it.
	if id.Provider != "" {
		p = id.Provider
	}
	return p, id.ProviderUserId, nil
}

// parseQualifiedHandle splits a provider-qualified handle like "github:alice"
// into its provider ("github") and handle ("alice"). Accounts are addressed by
// this friendly form; a value with no "provider:" prefix is rejected so the
// user gets a clear hint rather than a confusing lookup miss.
func parseQualifiedHandle(ref string) (provider, handle string, err error) {
	provider, handle, ok := strings.Cut(ref, ":")
	if !ok || provider == "" || handle == "" {
		return "", "", fmt.Errorf("account %q must be a qualified handle like \"github:alice\" (or a ULID)", ref)
	}
	return provider, handle, nil
}

// resolveProjectRef turns a project reference (ULID or name) into its ULID. A
// ULID is returned unchanged; a name is resolved via the server's
// case-insensitive by-name lookup (the same call `entire project list --name`
// uses). Project names are globally unique, so a name maps to at most one project.
func resolveProjectRef(ctx context.Context, c *coreapi.Client, ref string) (string, error) {
	if looksLikeULID(ref) {
		return ref, nil
	}
	out, err := c.ListProjects(ctx, coreapi.ListProjectsParams{Name: coreapi.NewOptString(ref)})
	if err != nil {
		if isCoreNotFound(err) {
			return "", noProjectNamedErr(ref)
		}
		return "", err
	}
	project, ok := out.Project.Get()
	if !ok {
		return "", noProjectNamedErr(ref)
	}
	return project.ID, nil
}

// resolveRepoRef turns a repo reference into its ULID. A ULID passes through.
// A name requires a project scope (projectRef, itself a name or ULID) because
// repo names are unique only within a project: the repo is resolved via the
// server's case-insensitive by-name lookup, scoped to that project. Like the
// org/project endpoints, a name-filtered list returns the single match under the
// response's singular `repo` field (the plural `repos` is only populated for an
// unfiltered page) — reading `repos` here was the COR-699 bug.
func resolveRepoRef(ctx context.Context, c *coreapi.Client, ref, projectRef string) (string, error) {
	if looksLikeULID(ref) {
		return ref, nil
	}
	if projectRef == "" {
		return "", fmt.Errorf("repo %q is a name; pass --project <name|ULID> to resolve it, or use a repo ULID", ref)
	}
	projID, err := resolveProjectRef(ctx, c, projectRef)
	if err != nil {
		return "", err
	}
	out, err := c.ListProjectRepos(ctx, coreapi.ListProjectReposParams{ProjectId: projID, Name: coreapi.NewOptString(ref)})
	if err != nil {
		if isCoreNotFound(err) {
			return "", noRepoNamedErr(ref)
		}
		return "", err
	}
	repo, ok := out.Repo.Get()
	if !ok {
		return "", noRepoNamedErr(ref)
	}
	return repo.ID, nil
}

func noOrgNamedErr(name string) error {
	return fmt.Errorf("no org named %q (run `entire org list` to see names, or pass a ULID)", name)
}

func noProjectNamedErr(name string) error {
	return fmt.Errorf("no project named %q (run `entire project list` to see names, or pass a ULID)", name)
}

func noRepoNamedErr(name string) error {
	return fmt.Errorf("no repo named %q in that project (run `entire repo list <project>` to see names, or pass a ULID)", name)
}

// resolvedRefLabel formats a reference for a success message so it always
// names the resolved ULID. When the user passed a ULID (ref == id) it returns
// the id alone; when they passed a name it returns "name (id)" so the message
// is unambiguous in environments where names can be reused across orgs/projects.
func resolvedRefLabel(ref, id string) string {
	if ref == id {
		return id
	}
	return fmt.Sprintf("%s (%s)", ref, id)
}

// toProjectList adapts a name-filtered project response — which returns the
// single match under the response's singular `project` field — into a slice for
// list output (empty when the field is unset).
func toProjectList(p coreapi.OptProject) []coreapi.Project {
	if v, ok := p.Get(); ok {
		return []coreapi.Project{v}
	}
	return nil
}
