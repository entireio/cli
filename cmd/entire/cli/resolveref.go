package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/entireio/cli/internal/coreapi"
)

// Control-plane commands reference orgs and projects by their parent ULID in
// many places (repo create --project, project create --owner, grant org/project
// <id>, …). ULIDs are unfriendly to type, so these refs also accept a human
// name: looksLikeULID decides which form was given, and the resolveXRef helpers
// turn a name into its ULID via a list lookup. A ULID is always passed straight
// through with no network call, preserving the original behavior exactly.

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

// resolveOrgRef turns an org reference (ULID or name) into its ULID. A ULID is
// returned unchanged; a name is looked up against the caller's visible orgs.
func resolveOrgRef(ctx context.Context, c *coreapi.Client, ref string) (string, error) {
	if looksLikeULID(ref) {
		return ref, nil
	}
	out, err := c.ListOrgs(ctx)
	if err != nil {
		return "", err
	}
	return pickOrg(out.Orgs, ref)
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
	return id.AccountId, nil
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
// ULID is returned unchanged; a name is looked up via the server's exact-name
// filter (the same call `entire project list --name` uses).
func resolveProjectRef(ctx context.Context, c *coreapi.Client, ref string) (string, error) {
	if looksLikeULID(ref) {
		return ref, nil
	}
	out, err := c.ListProjects(ctx, coreapi.ListProjectsParams{Name: coreapi.NewOptString(ref)})
	if err != nil {
		return "", err
	}
	return pickProject(out.Projects, ref)
}

// pickOrg selects the single org named name. Org names are unique, so a name
// matches at most one org; zero matches is an error pointing at `org list`, and
// (defensively) multiple matches list the colliding ids so the user can fall
// back to a ULID.
func pickOrg(orgs []coreapi.Org, name string) (string, error) {
	var matches []coreapi.Org
	for _, o := range orgs {
		if o.Name == name {
			matches = append(matches, o)
		}
	}
	switch len(matches) {
	case 1:
		return matches[0].ID, nil
	case 0:
		return "", fmt.Errorf("no org named %q (run `entire org list` to see names, or pass a ULID)", name)
	default:
		ids := make([]string, len(matches))
		for i, o := range matches {
			ids[i] = o.ID
		}
		return "", fmt.Errorf("org name %q is ambiguous (%s); pass a ULID instead", name, strings.Join(ids, ", "))
	}
}

// pickProject selects the single project named name. Project names are unique
// only within an owner, so a bare name can match several projects across
// different orgs/accounts; on ambiguity the candidates (id + owner) are listed
// so the user can pass the ULID or scope by org.
func pickProject(projects []coreapi.Project, name string) (string, error) {
	var matches []coreapi.Project
	for _, p := range projects {
		if p.Name == name {
			matches = append(matches, p)
		}
	}
	switch len(matches) {
	case 1:
		return matches[0].ID, nil
	case 0:
		return "", fmt.Errorf("no project named %q (run `entire project list` to see names, or pass a ULID)", name)
	default:
		parts := make([]string, len(matches))
		for i, p := range matches {
			parts[i] = fmt.Sprintf("%s (owner %s)", p.ID, p.OwnerId)
		}
		return "", fmt.Errorf("project name %q is ambiguous (%s); pass a ULID or scope with --org", name, strings.Join(parts, ", "))
	}
}

// filterProjectsByName narrows projects to exact name matches, returning all of
// them when name is empty. Used by `project list --org` to apply --name
// client-side, since the org-scoped list endpoint has no name parameter.
func filterProjectsByName(projects []coreapi.Project, name string) []coreapi.Project {
	if name == "" {
		return projects
	}
	var out []coreapi.Project
	for _, p := range projects {
		if p.Name == name {
			out = append(out, p)
		}
	}
	return out
}
