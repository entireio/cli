package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/versioninfo"
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
// from the stored refresh token when the active context drives resolution.
type ControlPlaneTarget struct {
	CoreURL     string
	TokenSource func(context.Context) (string, error)
}

// ResolveControlPlaneTarget chooses which core the control-plane commands talk
// to and how their bearer is obtained. The control-plane host *is* a core, so
// there is no /.well-known discovery here — the active context names the core,
// which is what makes `entire auth switch <ctx>` retarget the control plane onto
// that login server. The bearer is a per-context refreshing provider (silent
// JWT re-mint from the stored refresh token).
//
// No active context means not logged in: the error wraps ErrNotLoggedIn so
// callers render the `entire login` hint. There is no fallback host — a
// control-plane command without a login has no identity to act as.
func ResolveControlPlaneTarget() (ControlPlaneTarget, error) {
	c, ok, err := ActingContext()
	if err != nil {
		return ControlPlaneTarget{}, err
	}
	if !ok {
		return ControlPlaneTarget{}, errNoLogin()
	}

	return targetForContext(c)
}

// errNoLogin is the not-logged-in error with the login hint.
func errNoLogin() error {
	return &reauthError{msg: "not logged in; run `entire login`", sentinel: ErrNotLoggedIn}
}

// ResolveControlPlaneTargetForCluster chooses which core a *resource-provider*
// control-plane command should dial — one whose subject is a mirror on a
// specific cluster (mirror add/remove, reading a mirror's collaborators)
// rather than the caller's own account.
//
// Unlike ResolveControlPlaneTarget, the core is NOT taken from the active
// context: a cluster's mirror lives in the federation that fronts that cluster,
// which may differ from the active login (e.g. a partial.to context acting on a
// prod entire.io cluster). We discover the cluster's trusted cores from its
// /.well-known/entire-cluster.json and require the ACTIVE context to be issued
// by one of them — exactly as git and data-API resolution do (see
// ResolveDataAPIToken). The bearer is that context's refreshing login provider
// (silent JWT re-mint from its stored refresh token).
//
// When the active context isn't trusted by the cluster the discovery resolver
// says so and names the saved logins that are (or, when none is, the cluster's
// cores), so the user switches with `entire auth switch` or logs in to the right
// federation rather than seeing an opaque "unknown cluster_host" 400.
func ResolveControlPlaneTargetForCluster(ctx context.Context, clusterHost string) (ControlPlaneTarget, error) {
	if clusterHost == "" {
		return ControlPlaneTarget{}, errors.New("cluster-addressed control-plane command requires a target cluster host")
	}
	httpClient := &http.Client{Timeout: controlPlaneClusterDiscoveryTimeout, Transport: versioninfo.WrapTransport(nil)}
	c, err := resolveContextForCluster(ctx, userdirs.Config(), userdirs.Cache(), clusterHost, httpClient, nil)
	if err != nil {
		return ControlPlaneTarget{}, err
	}
	// Cluster discovery announces an auto-selected login itself.
	if f, selected, ok, serr := activeContextIn(); serr == nil && ok && selected.Name == c.Name {
		announceContext(len(f.Contexts), c)
	}
	return targetForContext(c)
}

// targetForContext builds the ControlPlaneTarget for an already-chosen context:
// a refreshing login provider (silent JWT re-mint from the stored refresh
// token) bound to that context's core. Shared by the active-context and
// cluster-addressed resolvers, which differ only in how they pick c.
func targetForContext(c *contexts.Context) (ControlPlaneTarget, error) {
	src, err := NewRefreshingLoginProvider(c, nil, insecureHTTPEnabled() || isLoopbackHTTP(c.CoreURL))
	if err != nil {
		return ControlPlaneTarget{}, fmt.Errorf("build token source for context %q: %w", c.Name, err)
	}
	return ControlPlaneTarget{CoreURL: strings.TrimRight(c.CoreURL, "/"), TokenSource: src}, nil
}
