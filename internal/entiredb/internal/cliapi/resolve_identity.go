package cliapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"

	"github.com/entireio/cli/internal/entiredb/core/api"
)

// TargetIdentity is the stable (provider, provider_user_id) tuple that
// identifies a user for membership/grant API calls. Always pass this
// shape on the wire — never raw handle.
type TargetIdentity struct {
	Provider       string
	ProviderUserID string
}

// ResolveTargetIdentity returns a TargetIdentity from CLI flags. If
// --provider-user-id was supplied, returns it directly (no API call).
// Otherwise it requires --handle and resolves via the API. provider
// defaults to "github".
func ResolveTargetIdentity(client *api.Client, provider, handle, providerUserID string) (*TargetIdentity, error) {
	if provider == "" {
		provider = "github"
	}
	if providerUserID != "" {
		return &TargetIdentity{Provider: provider, ProviderUserID: providerUserID}, nil
	}
	if handle == "" {
		return nil, errors.New("provide --handle or --provider-user-id")
	}

	data, err := client.GetJSON("/api/v1/identity/handles/" + url.PathEscape(provider) + "/" + url.PathEscape(handle))
	if err != nil {
		return nil, fmt.Errorf("resolve handle %s/%s: %w", provider, handle, err)
	}
	var resp struct {
		Provider       string `json:"provider"`
		Handle         string `json:"handle"`
		ProviderUserID string `json:"providerUserId"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("decode resolve response: %w", err)
	}
	if resp.ProviderUserID == "" {
		return nil, fmt.Errorf("resolve %s/%s returned empty providerUserId", provider, handle)
	}
	return &TargetIdentity{Provider: resp.Provider, ProviderUserID: resp.ProviderUserID}, nil
}

// ResolveAccountIDByHandle returns the account ULID for the given
// (provider, handle) pair via GET /api/identity/handles/{provider}/{handle}.
// Use this when a verb needs the account itself (e.g. `--owner` on
// entire-project create resolving to owner_id); use ResolveTargetIdentity
// when the verb needs the (provider, provider_user_id) tuple grants
// APIs take. provider defaults to "github".
func ResolveAccountIDByHandle(client *api.Client, provider, handle string) (string, error) {
	if provider == "" {
		provider = "github"
	}
	if handle == "" {
		return "", errors.New("empty handle")
	}
	data, err := client.GetJSON("/api/v1/identity/handles/" + url.PathEscape(provider) + "/" + url.PathEscape(handle))
	if err != nil {
		return "", fmt.Errorf("resolve handle %s/%s: %w", provider, handle, err)
	}
	var resp struct {
		AccountID string `json:"accountId"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return "", fmt.Errorf("decode resolve response: %w", err)
	}
	if resp.AccountID == "" {
		return "", fmt.Errorf("resolve %s/%s returned empty accountId", provider, handle)
	}
	return resp.AccountID, nil
}
