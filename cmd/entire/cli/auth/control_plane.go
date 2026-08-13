package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/entireio/cli/internal/entireclient/clusterdiscovery"
	"github.com/entireio/cli/internal/entireclient/contexts"
	"github.com/entireio/cli/internal/entireclient/userdirs"
)

// controlPlaneClusterDiscoveryTimeout bounds the one
// /.well-known/entire-cluster.json GET a cluster-addressed control-plane
// command makes to learn which core fronts the cluster. Short so an absent or
// slow endpoint fails the command promptly.
const controlPlaneClusterDiscoveryTimeout = 8 * time.Second

// resolveContextForCluster is the discovery seam, swapped in tests so they
// don't reach the network. Mirrors clusterdiscovery.ResolveContextForCluster.
var resolveContextForCluster resolveContextFunc = clusterdiscovery.ResolveContextForCluster

// ControlPlaneTarget is the resolved login server a control-plane request
// (org/repo/project/grant) should dial, plus the bearer source for it.
//
// CoreURL is an origin (no /api/v1 suffix); the caller appends the API base
// path. TokenSource returns a bearer valid for CoreURL, re-minting silently
// from the stored refresh token when the current login drives resolution.
type ControlPlaneTarget struct {
	CoreURL     string
	TokenSource func(context.Context) (string, error)
}

// ResolveControlPlaneTarget chooses which core the control-plane commands talk
// to and how their bearer is obtained. The control-plane host *is* a core, so
// there is no /.well-known discovery here — the current login names the core.
// The bearer is a refreshing provider (silent
// JWT re-mint from the stored refresh token).
//
// No current login means not logged in: the error wraps ErrNotLoggedIn so
// callers render the `entire login` hint. There is no fallback host — a
// control-plane command without a login has no identity to act as.
func ResolveControlPlaneTarget() (ControlPlaneTarget, error) {
	c, ok, err := usableCurrentLogin()
	if err != nil {
		return ControlPlaneTarget{}, err
	}
	if !ok {
		return ControlPlaneTarget{}, &reauthError{
			msg:      "not logged in; run `entire login`",
			sentinel: ErrNotLoggedIn,
		}
	}

	return targetForLogin(c)
}

// ResolveControlPlaneTargetForCluster chooses which core a *resource-provider*
// control-plane command should dial — one whose subject is a mirror on a
// specific cluster (mirror create/remove, mirror collaborators list)
// rather than the caller's own account.
//
// Unlike ResolveControlPlaneTarget, the core is NOT taken from the active
// context: a cluster's mirror lives in the federation that fronts that cluster,
// which may differ from the active login (e.g. a partial.to context acting on a
// prod entire.io cluster). We discover the cluster's trusted cores from its
// /.well-known/entire-cluster.json and pick the local context eligible for one
// of them — active-wins-if-eligible, else the sole eligible context, else an
// explicit-choice / login hint — exactly as git and data-API resolution do
// (see ResolveDataAPIToken). The bearer is that context's
// refreshing login provider (silent JWT re-mint from its stored refresh token).
//
// With no eligible local context the discovery resolver returns its login hint
// naming the cluster's cores, so the user logs in to the right federation
// rather than seeing an opaque "unknown cluster_host" 400 from the active
// login's core.
func ResolveControlPlaneTargetForCluster(ctx context.Context, clusterHost string) (ControlPlaneTarget, error) {
	if clusterHost == "" {
		return ControlPlaneTarget{}, errors.New("cluster-addressed control-plane command requires a target cluster host")
	}
	httpClient := &http.Client{Timeout: controlPlaneClusterDiscoveryTimeout}
	c, err := resolveContextForCluster(ctx, userdirs.Config(), userdirs.Cache(), clusterHost, httpClient, nil)
	if err != nil {
		return ControlPlaneTarget{}, err
	}
	return targetForLogin(c)
}

// targetForLogin builds the ControlPlaneTarget for an already-chosen context:
// a refreshing login provider (silent JWT re-mint from the stored refresh
// token) bound to that login's core. Shared by the current-login and
// cluster-addressed resolvers, which differ only in how they pick c.
func targetForLogin(c *contexts.Context) (ControlPlaneTarget, error) {
	src, err := NewRefreshingLoginProvider(c, nil, insecureHTTPEnabled() || isLoopbackHTTP(c.CoreURL))
	if err != nil {
		return ControlPlaneTarget{}, fmt.Errorf("build token source for current login: %w", err)
	}
	return ControlPlaneTarget{CoreURL: strings.TrimRight(c.CoreURL, "/"), TokenSource: src}, nil
}

// usableCurrentLogin returns the current stored login and ok=true, or false
// when logged out or when its metadata has no login-server URL.
func usableCurrentLogin() (c *contexts.Context, ok bool, err error) {
	f, err := contexts.Load(userdirs.Config())
	if err != nil {
		return nil, false, fmt.Errorf("load current login: %w", err)
	}
	c = f.Find(f.CurrentContext)
	if c == nil || c.CoreURL == "" {
		return nil, false, nil
	}
	return c, true, nil
}
