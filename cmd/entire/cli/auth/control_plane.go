package auth

import (
	"context"
	"fmt"
	"strings"

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
// there is no /.well-known discovery here — the active context names the core,
// which is what makes `entire auth use <ctx>` retarget the control plane onto
// that login server. The bearer is a per-context refreshing provider (silent
// JWT re-mint from the stored refresh token).
//
// No active context means not logged in: the error wraps ErrNotLoggedIn so
// callers render the `entire login` hint. There is no fallback host — a
// control-plane command without a login has no identity to act as.
func ResolveControlPlaneTarget() (ControlPlaneTarget, error) {
	c, ok, err := activeContext()
	if err != nil {
		return ControlPlaneTarget{}, err
	}
	if !ok {
		return ControlPlaneTarget{}, &reauthError{
			msg:      "not logged in; run `entire login`",
			sentinel: ErrNotLoggedIn,
		}
	}

	src, err := NewRefreshingLoginProvider(c, nil, insecureHTTPEnabled() || isLoopbackHTTP(c.CoreURL))
	if err != nil {
		return ControlPlaneTarget{}, fmt.Errorf("build token source for context %q: %w", c.Name, err)
	}
	return ControlPlaneTarget{CoreURL: strings.TrimRight(c.CoreURL, "/"), TokenSource: src}, nil
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
