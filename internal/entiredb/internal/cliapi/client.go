// Package cliapi gives the entire-repo CLI a logged-in entire-core HTTP
// client. It looks up the active context's token + core URL using the
// same on-disk layout that entire-core's own CLI writes (so a login
// from either binary is visible to the other), then builds an
// *api.Client.
package cliapi

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/entireio/cli/internal/entiredb/client/contexts"
	"github.com/entireio/cli/internal/entiredb/core/api"
	"github.com/entireio/cli/internal/entiredb/internal/cliauth"
	"github.com/entireio/cli/internal/entiredb/tokenstore"
)

// Client builds an *api.Client wired with the active login JWT and the
// resolved entire-core base URL.
//
// coreURLOverride and contextOverride correspond to per-invocation
// --core-url and --context flags; either may be empty.
//
// Precedence for the base URL:
//  1. coreURLOverride
//  2. resolved context's CoreURL
//  3. cliauth.Config.EntireCoreBaseURL (ENTIRE_CORE_AUTH_BASE_URL)
//
// Precedence for the token:
//  1. ENTIRE_TOKEN env var — bypass the credential store
//  2. The resolved context's stored access token
func Client(cfg cliauth.Config, coreURLOverride, contextOverride string) (*api.Client, error) {
	c := &api.Client{BaseURL: resolveBaseURL(cfg, coreURLOverride, contextOverride)}
	if tok := os.Getenv("ENTIRE_TOKEN"); tok != "" {
		c.Token = tok
		return c, nil
	}
	tok, err := readToken(cfg, contextOverride)
	if err != nil {
		return nil, fmt.Errorf("no credentials for %s: %w (run 'entire-core auth login' or set ENTIRE_TOKEN)", c.BaseURL, err)
	}
	c.Token = tok
	return c, nil
}

func resolveBaseURL(cfg cliauth.Config, coreURLOverride, contextOverride string) string {
	if v := strings.TrimSpace(coreURLOverride); v != "" {
		return strings.TrimRight(v, "/")
	}
	if c, err := resolveContext(cfg, contextOverride); err == nil && c.CoreURL != "" {
		return strings.TrimRight(c.CoreURL, "/")
	}
	return strings.TrimRight(cfg.EntireCoreBaseURL, "/")
}

func readToken(cfg cliauth.Config, contextOverride string) (string, error) {
	c, err := resolveContext(cfg, contextOverride)
	if err != nil {
		return "", err
	}
	encoded, err := tokenstore.Get(c.KeychainService, c.Handle)
	if err != nil {
		if errors.Is(err, tokenstore.ErrNotFound) {
			return "", fmt.Errorf("context %q has no stored token", c.Name)
		}
		return "", fmt.Errorf("read token: %w", err)
	}
	tok, _ := tokenstore.DecodeTokenWithExpiration(encoded)
	if tok == "" {
		return "", fmt.Errorf("stored token for context %q is empty", c.Name)
	}
	return tok, nil
}

func resolveContext(cfg cliauth.Config, contextOverride string) (*contexts.Context, error) {
	f, err := contexts.Load(cfg.ConfigDir)
	if err != nil {
		return nil, fmt.Errorf("load contexts: %w", err)
	}
	if name := strings.TrimSpace(contextOverride); name != "" {
		c := f.Find(name)
		if c == nil {
			return nil, fmt.Errorf("context %q not found", name)
		}
		return c, nil
	}
	c := f.Resolve(cfg.CredentialIssuerKey())
	if c == nil {
		return nil, errors.New("no active session (run `entire-core auth login` or pass --context NAME)")
	}
	return c, nil
}
