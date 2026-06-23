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
// returned unchanged; a name is resolved by the server's exact-name lookup
// (GET /orgs?name=), an O(1) match that doesn't depend on the org landing on
// the first page of a now-paginated list.
func resolveOrgRef(ctx context.Context, c *coreapi.Client, ref string) (string, error) {
	if looksLikeULID(ref) {
		return ref, nil
	}
	out, err := c.ListOrgs(ctx, coreapi.ListOrgsParams{Name: coreapi.NewOptString(ref)})
	if err != nil {
		if isNotFound(err) {
			return "", fmt.Errorf("no org named %q (run `entire org list` to see names, or pass a ULID)", ref)
		}
		return "", err
	}
	if !out.Org.Set {
		return "", fmt.Errorf("no org named %q (run `entire org list` to see names, or pass a ULID)", ref)
	}
	return out.Org.Value.ID, nil
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
// ULID is returned unchanged; a name is resolved by the server's exact-name
// lookup (GET /projects?name=), which returns the single match under `project`.
// Project names are globally unique, so there's no client-side disambiguation.
func resolveProjectRef(ctx context.Context, c *coreapi.Client, ref string) (string, error) {
	if looksLikeULID(ref) {
		return ref, nil
	}
	out, err := c.ListProjects(ctx, coreapi.ListProjectsParams{Name: coreapi.NewOptString(ref)})
	if err != nil {
		if isNotFound(err) {
			return "", fmt.Errorf("no project named %q (run `entire project list` to see names, or pass a ULID)", ref)
		}
		return "", err
	}
	if !out.Project.Set {
		return "", fmt.Errorf("no project named %q (run `entire project list` to see names, or pass a ULID)", ref)
	}
	return out.Project.Value.ID, nil
}

// resolveRepoRef turns a repo reference into its ULID. A ULID passes through.
// A name requires a project scope (projectRef, itself a name or ULID) because
// repo names are unique only within a project: the server's exact-name lookup
// (GET /projects/{id}/repos?name=) returns the single match under `repo`.
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
		if isNotFound(err) {
			return "", fmt.Errorf("no repo named %q in that project (run `entire repo list <project>` to see names, or pass a ULID)", ref)
		}
		return "", err
	}
	if !out.Repo.Set {
		return "", fmt.Errorf("no repo named %q in that project (run `entire repo list <project>` to see names, or pass a ULID)", ref)
	}
	return out.Repo.Value.ID, nil
}

// isNotFound reports whether err is a control-plane 404. The exact-name
// lookups (resolveOrgRef/resolveProjectRef/resolveRepoRef) map it to a
// friendly "no X named …" hint rather than surfacing the raw problem detail.
func isNotFound(err error) bool {
	var se *coreapi.ErrorModelStatusCode
	return errors.As(err, &se) && se.StatusCode == http.StatusNotFound
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
