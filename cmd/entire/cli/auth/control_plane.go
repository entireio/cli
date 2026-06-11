package auth

import (
	"context"
	"fmt"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/internal/entireclient/contexts"
	"github.com/entireio/cli/internal/entireclient/userdirs"
)

// ControlPlaneTarget is the resolved login server a control-plane request
// (org/repo/project/grant) should dial, plus the bearer source for it.
//
// CoreURL is an origin (no /api/v1 suffix); the caller appends the API base
// path. TokenSource returns a bearer valid for CoreURL, re-minting silently
// from the stored refresh token when the active context drives resolution.
type ControlPlaneTarget struct {
	CoreURL     string
	TokenSource func(context.Context) (string, error)
}

// ResolveControlPlaneTarget chooses which core the control-plane commands talk
// to and how their bearer is obtained. The control-plane host *is* a core, so
// there is no /.well-known discovery here — the active context already names
// the core. Precedence (matching `auth status`):
//
//  1. the active contexts.json login -> its CoreURL, with a per-context
//     refreshing bearer (silent JWT re-mint). This is what makes
//     `entire auth use <ctx>` retarget the control plane onto that core.
//  2. no active context -> the configured auth origin (ENTIRE_AUTH_BASE_URL or
//     the default) + TokenForResource, the pre-contexts fallback.
//
// ENTIRE_AUTH_BASE_URL is the fallback host, not an override: a token minted
// by the active context's core can't authenticate against a different host, so
// "use the override host but the context's identity" can't succeed. The active
// context always wins when present.
func ResolveControlPlaneTarget() (ControlPlaneTarget, error) {
	c, ok, err := activeContext()
	if err != nil {
		return ControlPlaneTarget{}, err
	}
	if !ok {
		return staticControlPlaneTarget(), nil
	}

	// The refreshing provider keys its own token manager on c.CoreURL as the
	// issuer, so its store reads and STS/refresh both target the right core —
	// the bug the singleton manager (pinned to AuthBaseURL) has when the
	// active context lives on a different core.
	src, err := NewRefreshingLoginProvider(c, nil, insecureHTTPEnabled() || isLoopbackHTTP(c.CoreURL))
	if err != nil {
		return ControlPlaneTarget{}, fmt.Errorf("build token source for context %q: %w", c.Name, err)
	}
	return ControlPlaneTarget{CoreURL: strings.TrimRight(c.CoreURL, "/"), TokenSource: src}, nil
}

// staticControlPlaneTarget is the no-active-context fallback: dial the
// configured auth origin (ENTIRE_AUTH_BASE_URL or the default) and resolve the
// bearer through the singleton manager, which performs an RFC 8693 exchange
// when the stored token's audience doesn't cover the core.
func staticControlPlaneTarget() ControlPlaneTarget {
	base := strings.TrimRight(api.AuthBaseURL(), "/")
	// The exchange's resource must be the bare origin; OriginOnly strips any
	// path/query so the audience the manager keys on matches the server's.
	resource := api.OriginOnly(base)
	return ControlPlaneTarget{
		CoreURL: base,
		TokenSource: func(ctx context.Context) (string, error) {
			return TokenForResource(ctx, resource)
		},
	}
}

// activeContext returns the active contexts.json login and ok=true, or
// ok=false when there is no current context or it carries no CoreURL (an
// unusable pointer we treat as "no active context" rather than dialing an
// empty host).
func activeContext() (c *contexts.Context, ok bool, err error) {
	f, err := contexts.Load(userdirs.Config())
	if err != nil {
		return nil, false, fmt.Errorf("load contexts: %w", err)
	}
	c = f.Find(f.CurrentContext)
	if c == nil || c.CoreURL == "" {
		return nil, false, nil
	}
	return c, true, nil
}
