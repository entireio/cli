package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/entireio/cli/internal/entireclient/clusterdiscovery"
	"github.com/entireio/cli/internal/entireclient/contexts"
	"github.com/entireio/cli/internal/entireclient/tokenstore"
)

// These tests drive process-global state (ENTIRE_CONFIG_DIR, the
// token-store backend) so they cannot run in parallel.

// writeActiveContext writes a single-context contexts.json under configDir and
// marks it current.
func writeActiveContext(t *testing.T, configDir, name, coreURL, handle, svc string) {
	t.Helper()
	c := &contexts.Context{Name: name, CoreURL: coreURL, Handle: handle, KeychainService: svc}
	if err := contexts.Save(configDir, &contexts.File{CurrentContext: name, Contexts: []*contexts.Context{c}}); err != nil {
		t.Fatalf("write contexts.json: %v", err)
	}
}

// With no override and an active context, the target is that context's core and
// the bearer comes from the context's keyring slot (the refreshing provider).
func TestResolveControlPlaneTarget_ActiveContextWins(t *testing.T) {
	configDir := t.TempDir()
	t.Setenv("ENTIRE_CONFIG_DIR", configDir)

	restore := tokenstore.UseFileBackendForTesting(filepath.Join(t.TempDir(), "tokens.json"))
	t.Cleanup(restore)

	const coreURL = "https://ctx-core.example"
	svc := tokenstore.CoreKeyringService(coreURL)
	jwt := makeJWT(t, fmt.Sprintf(`{"iss":%q,"handle":"alice","exp":%d}`, coreURL, time.Now().Add(2*time.Hour).Unix()))
	if err := tokenstore.Set(svc, "alice", tokenstore.EncodeTokenWithExpiration(jwt, 7200)); err != nil {
		t.Fatalf("seed token: %v", err)
	}
	writeActiveContext(t, configDir, "alice@core", coreURL, "alice", svc)

	target, err := ResolveControlPlaneTarget()
	if err != nil {
		t.Fatalf("ResolveControlPlaneTarget: %v", err)
	}
	if target.CoreURL != coreURL {
		t.Fatalf("CoreURL = %q, want the active context's core %q", target.CoreURL, coreURL)
	}
	// The fresh token is returned with no network call, proving the source is
	// wired to the context's keyring slot.
	got, err := target.TokenSource(context.Background())
	if err != nil {
		t.Fatalf("TokenSource: %v", err)
	}
	if got != jwt {
		t.Fatalf("TokenSource returned %q, want the context's stored JWT", got)
	}
}

// A cluster-addressed control-plane command dials the core that fronts the
// cluster (discovered from /.well-known) using the matching local context —
// NOT the active context, which may belong to a different federation. This is
// the fix for `repo mirror collaborators list … <prod-cluster>` 400ing with
// "unknown cluster_host" while the active context is a staging login.
func TestResolveControlPlaneTargetForCluster_DialsClusterCoreNotActive(t *testing.T) {
	configDir := t.TempDir()
	t.Setenv("ENTIRE_CONFIG_DIR", configDir)

	restore := tokenstore.UseFileBackendForTesting(filepath.Join(t.TempDir(), "tokens.json"))
	t.Cleanup(restore)

	const (
		activeCore  = "https://eu.auth.partial.to"
		clusterCore = "https://eu.auth.entire.io"
		clusterHost = "aws-us-east-2.entire.io"
	)
	// Seed a fresh token only for the cluster's core context — the one we
	// expect to win. The active (partial) context deliberately has no token, so
	// a regression that dialed it would fail loudly rather than pass by luck.
	clusterSvc := tokenstore.CoreKeyringService(clusterCore)
	jwt := makeJWT(t, fmt.Sprintf(`{"iss":%q,"handle":"alice","exp":%d}`, clusterCore, time.Now().Add(2*time.Hour).Unix()))
	if err := tokenstore.Set(clusterSvc, "alice", tokenstore.EncodeTokenWithExpiration(jwt, 7200)); err != nil {
		t.Fatalf("seed token: %v", err)
	}

	activeCtx := &contexts.Context{Name: "alice@partial", CoreURL: activeCore, Handle: "alice", KeychainService: tokenstore.CoreKeyringService(activeCore)}
	clusterCtx := &contexts.Context{Name: "alice@prod", CoreURL: clusterCore, Handle: "alice", KeychainService: clusterSvc}
	if err := contexts.Save(configDir, &contexts.File{CurrentContext: activeCtx.Name, Contexts: []*contexts.Context{activeCtx, clusterCtx}}); err != nil {
		t.Fatalf("write contexts.json: %v", err)
	}

	// Stub cluster discovery so the test doesn't hit the network: the cluster
	// resolves to the prod context (what /.well-known + selectContext would
	// yield), NOT the active partial context.
	prev := resolveContextForCluster
	resolveContextForCluster = func(_ context.Context, _, _, host string, _ *http.Client, _ clusterdiscovery.DebugFunc) (*contexts.Context, error) {
		if host != clusterHost {
			t.Fatalf("discovery host = %q, want %q", host, clusterHost)
		}
		return clusterCtx, nil
	}
	t.Cleanup(func() { resolveContextForCluster = prev })

	target, err := ResolveControlPlaneTargetForCluster(context.Background(), clusterHost)
	if err != nil {
		t.Fatalf("ResolveControlPlaneTargetForCluster: %v", err)
	}
	if target.CoreURL != clusterCore {
		t.Fatalf("CoreURL = %q, want the cluster's core %q (not the active context's %q)", target.CoreURL, clusterCore, activeCore)
	}
	got, err := target.TokenSource(context.Background())
	if err != nil {
		t.Fatalf("TokenSource: %v", err)
	}
	if got != jwt {
		t.Fatalf("TokenSource returned %q, want the cluster context's stored JWT", got)
	}
}

// An empty cluster host is a caller bug — fail fast rather than discovering
// against "".
func TestResolveControlPlaneTargetForCluster_EmptyHost(t *testing.T) {
	if _, err := ResolveControlPlaneTargetForCluster(context.Background(), ""); err == nil {
		t.Fatal("want an error for empty cluster host, got nil")
	}
}

// A genuine contexts.json read/parse error must fail loud — not silently fall
// back to a stale legacy identity for a control-plane mutation.
func TestResolveControlPlaneTarget_CorruptContextsErrors(t *testing.T) {
	configDir := t.TempDir()
	t.Setenv("ENTIRE_CONFIG_DIR", configDir)
	if err := os.WriteFile(filepath.Join(configDir, "contexts.json"), []byte("{ not valid json"), 0o600); err != nil {
		t.Fatalf("write corrupt contexts.json: %v", err)
	}
	if _, err := ResolveControlPlaneTarget(); err == nil {
		t.Fatal("want an error when contexts.json is corrupt, got nil")
	}
}

// With no active context there is no identity to act as: the resolver errors
// with the ErrNotLoggedIn sentinel so callers render the `entire login` hint.
func TestResolveControlPlaneTarget_NoContextErrsNotLoggedIn(t *testing.T) {
	configDir := t.TempDir() // empty: no contexts.json
	t.Setenv("ENTIRE_CONFIG_DIR", configDir)

	_, err := ResolveControlPlaneTarget()
	if err == nil {
		t.Fatal("want not-logged-in error, got nil")
	}
	if !errors.Is(err, ErrNotLoggedIn) {
		t.Fatalf("err = %v, want it to wrap ErrNotLoggedIn", err)
	}
	if !strings.Contains(err.Error(), "entire login") {
		t.Fatalf("err = %q, want the `entire login` hint", err)
	}
}
