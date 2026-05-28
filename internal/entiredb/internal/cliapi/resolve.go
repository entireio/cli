package cliapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/entireio/cli/internal/entiredb/api/corev1"
	"github.com/entireio/cli/internal/entiredb/core/api"
	"github.com/entireio/cli/internal/entiredb/core/model"
)

// LooksLikeULID reports whether s parses as a 26-char Crockford-base32
// ULID, optionally with the `svc_` service-account prefix. Delegates to
// model.ParseAccountID, the canonical parser that handles both shapes.
func LooksLikeULID(s string) bool {
	_, err := model.ParseAccountID(s)
	return err == nil
}

// ResolveOrgID accepts either an org name or an org ULID and returns the
// canonical ULID. Name resolution lists every accessible org and matches
// on Name. Multiple matches error with a disambiguation list.
func ResolveOrgID(c *api.Client, arg string) (string, error) {
	arg = strings.TrimSpace(arg)
	if arg == "" {
		return "", errors.New("empty org reference")
	}
	if LooksLikeULID(arg) {
		return arg, nil
	}
	data, err := c.GetJSON("/api/v1/orgs")
	if err != nil {
		return "", fmt.Errorf("list orgs: %w", err)
	}
	var resp struct {
		Orgs []corev1.Org `json:"orgs"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return "", fmt.Errorf("decode orgs: %w", err)
	}
	var matches []corev1.Org
	for _, o := range resp.Orgs {
		if o.Name == arg {
			matches = append(matches, o)
		}
	}
	switch len(matches) {
	case 0:
		return "", fmt.Errorf("org %q not found among %d accessible orgs", arg, len(resp.Orgs))
	case 1:
		return matches[0].ID, nil
	default:
		var b strings.Builder
		for _, m := range matches {
			fmt.Fprintf(&b, "\n  %s\t%s", m.ID, m.Name)
		}
		return "", fmt.Errorf("org name %q is ambiguous (%d matches); pass a ULID:%s", arg, len(matches), b.String())
	}
}

// ResolveProjectID accepts either a globally unique project name or a project
// ULID and returns the canonical ULID. Names resolve through GET /api/projects
// so the server applies the same validation and project#view authorization as
// every other name lookup.
func ResolveProjectID(c *api.Client, ref string) (string, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return "", errors.New("empty project reference")
	}
	if model.LooksLikeRawULID(ref) {
		return ref, nil
	}
	data, err := c.GetJSON("/api/v1/projects?name=" + url.QueryEscape(ref))
	if err != nil {
		return "", fmt.Errorf("resolve project %q: %w", ref, err)
	}
	var resp struct {
		Project *corev1.Project `json:"project"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return "", fmt.Errorf("decode project lookup: %w", err)
	}
	if resp.Project == nil || resp.Project.ID == "" {
		return "", fmt.Errorf("resolve project %q returned empty ID", ref)
	}
	return resp.Project.ID, nil
}

// CurrentAccount returns the logged-in account ID from GET /api/v1/me.
func CurrentAccount(c *api.Client) (accountID string, err error) {
	data, err := c.GetJSON("/api/v1/me")
	if err != nil {
		return "", fmt.Errorf("get /api/v1/me: %w", err)
	}
	// /api/v1/me returns a {global, regional} envelope; accountId lives
	// on global. Decoding the whole struct would couple to the regional
	// PII shape — pull just the field we need.
	var resp struct {
		Global struct {
			AccountID string `json:"accountId"`
		} `json:"global"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return "", fmt.Errorf("decode /api/v1/me: %w", err)
	}
	if resp.Global.AccountID == "" {
		return "", errors.New("/api/v1/me returned empty accountId")
	}
	return resp.Global.AccountID, nil
}
