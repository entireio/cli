package coreapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeLister is a ClusterLister that answers from a fixed host list, or fails.
type fakeLister struct {
	hosts []string
	err   error
}

func (f fakeLister) ListClusters(context.Context) (*ListClustersOutputBody, error) {
	if f.err != nil {
		return nil, f.err
	}
	clusters := make([]Cluster, 0, len(f.hosts))
	for _, h := range f.hosts {
		clusters = append(clusters, Cluster{PublicUrl: "https://" + h, Slug: h, Jurisdiction: "us"})
	}
	return &ListClustersOutputBody{Clusters: clusters}, nil
}

func TestVerifyClusterRegistered(t *testing.T) {
	t.Parallel()
	const core = "https://core.us.entire.io"
	hosts := []string{"aws-us-east-2.entire.io", "aws-eu-west-1.entire.io"}

	t.Run("registered host passes", func(t *testing.T) {
		t.Parallel()
		if err := VerifyClusterRegistered(t.Context(), fakeLister{hosts: hosts}, "", core, "aws-us-east-2.entire.io"); err != nil {
			t.Fatalf("registered host must pass: %v", err)
		}
	})

	// DNS is case-insensitive, so a host typed in mixed case names the same
	// cluster and must not be refused.
	t.Run("host match folds case", func(t *testing.T) {
		t.Parallel()
		if err := VerifyClusterRegistered(t.Context(), fakeLister{hosts: hosts}, "", core, "AWS-US-East-2.entire.io"); err != nil {
			t.Fatalf("case-folded host must pass: %v", err)
		}
	})

	t.Run("unregistered host fails actionably", func(t *testing.T) {
		t.Parallel()
		err := VerifyClusterRegistered(t.Context(), fakeLister{hosts: hosts}, "", core, "evil.example.com")
		if !errors.Is(err, ErrClusterNotRegistered) {
			t.Fatalf("err = %v, want ErrClusterNotRegistered", err)
		}
		// The message must name the core consulted (so the user can tell which
		// login answered) and the way out.
		for _, want := range []string{core, "evil.example.com", "aws-us-east-2.entire.io", "entire auth use"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("err = %q, missing %q", err.Error(), want)
			}
		}
	})

	// An unreachable registry is a failure, never a pass: the whole point is
	// that nothing else gets to vouch for the host.
	t.Run("registry failure fails closed", func(t *testing.T) {
		t.Parallel()
		err := VerifyClusterRegistered(t.Context(), fakeLister{err: errors.New("connection refused")}, "", core, "aws-us-east-2.entire.io")
		if err == nil {
			t.Fatal("an unreachable registry must fail")
		}
		if errors.Is(err, ErrClusterNotRegistered) {
			t.Fatal("an unreachable registry must not be reported as 'not registered'")
		}
		if !strings.Contains(err.Error(), "connection refused") {
			t.Errorf("err = %q, want the underlying failure", err.Error())
		}
	})

	t.Run("empty host is a caller bug", func(t *testing.T) {
		t.Parallel()
		if err := VerifyClusterRegistered(t.Context(), fakeLister{hosts: hosts}, "", core, "  "); err == nil {
			t.Fatal("an empty cluster host must fail")
		}
	})
}

// countingLister records how many registry round trips a caller actually made,
// which is the whole point of the cache.
type countingLister struct {
	hosts []string
	err   error
	calls int
}

func (c *countingLister) ListClusters(ctx context.Context) (*ListClustersOutputBody, error) {
	c.calls++
	return fakeLister{hosts: c.hosts, err: c.err}.ListClusters(ctx)
}

// A confirmed cluster is cached, so routine git operations stop paying a
// control-plane round trip — and keep working while the core is unreachable.
func TestVerifyClusterRegistered_CachesPositiveAnswers(t *testing.T) {
	t.Parallel()
	const core = "https://core.us.entire.io"
	const host = "aws-us-east-2.entire.io"
	cacheDir := t.TempDir()

	lister := &countingLister{hosts: []string{host}}
	for i := range 3 {
		if err := VerifyClusterRegistered(t.Context(), lister, cacheDir, core, host); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	if lister.calls != 1 {
		t.Fatalf("registry consulted %d times, want 1 (later calls must hit the cache)", lister.calls)
	}

	// DNS is case-insensitive and a core URL's trailing slash is
	// insignificant, so the same pairing typed differently must still hit.
	if err := VerifyClusterRegistered(t.Context(), lister, cacheDir, core+"/", "AWS-US-East-2.entire.io"); err != nil {
		t.Fatalf("case/slash variant: %v", err)
	}
	if lister.calls != 1 {
		t.Fatalf("registry consulted %d times, want 1 (normalised key must hit the cache)", lister.calls)
	}

	// The cached pairing keeps working when the core is down — the outage
	// resilience the cache exists for.
	down := &countingLister{err: errors.New("connection refused")}
	if err := VerifyClusterRegistered(t.Context(), down, cacheDir, core, host); err != nil {
		t.Fatalf("a cached cluster must survive a core outage: %v", err)
	}
}

// The cache is scoped to the core that answered: switching logins must not
// reuse another core'"'"'s verdict.
func TestVerifyClusterRegistered_CacheIsPerCore(t *testing.T) {
	t.Parallel()
	const host = "aws-us-east-2.entire.io"
	cacheDir := t.TempDir()

	if err := VerifyClusterRegistered(t.Context(), fakeLister{hosts: []string{host}}, cacheDir, "https://core.us.entire.io", host); err != nil {
		t.Fatalf("seed: %v", err)
	}
	other := &countingLister{hosts: []string{"somewhere-else.entire.io"}}
	if err := VerifyClusterRegistered(t.Context(), other, cacheDir, "https://core.partial.to", host); !errors.Is(err, ErrClusterNotRegistered) {
		t.Fatalf("err = %v, want a fresh check against the other core to fail", err)
	}
	if other.calls != 1 {
		t.Fatalf("other core consulted %d times, want 1", other.calls)
	}
}

// Misses and errors are re-checked every time: caching a refusal would let a
// stale "no" outlive an onboarding, and caching an outage would turn a blip
// into a sticky failure.
func TestVerifyClusterRegistered_DoesNotCacheFailures(t *testing.T) {
	t.Parallel()
	const core = "https://core.us.entire.io"
	cacheDir := t.TempDir()

	miss := &countingLister{hosts: []string{"aws-us-east-2.entire.io"}}
	for i := range 2 {
		if err := VerifyClusterRegistered(t.Context(), miss, cacheDir, core, "not-yet.entire.io"); !errors.Is(err, ErrClusterNotRegistered) {
			t.Fatalf("call %d: err = %v, want ErrClusterNotRegistered", i, err)
		}
	}
	if miss.calls != 2 {
		t.Fatalf("registry consulted %d times, want 2 (a miss must never be cached)", miss.calls)
	}
	// ... and once the cluster IS onboarded, the very next call sees it.
	miss.hosts = append(miss.hosts, "not-yet.entire.io")
	if err := VerifyClusterRegistered(t.Context(), miss, cacheDir, core, "not-yet.entire.io"); err != nil {
		t.Fatalf("a newly onboarded cluster must resolve immediately: %v", err)
	}

	failing := &countingLister{err: errors.New("connection refused")}
	for i := range 2 {
		if err := VerifyClusterRegistered(t.Context(), failing, cacheDir, core, "aws-eu-west-1.entire.io"); err == nil {
			t.Fatalf("call %d: an unreachable registry must fail", i)
		}
	}
	if failing.calls != 2 {
		t.Fatalf("registry consulted %d times, want 2 (an error must never be cached)", failing.calls)
	}
}

// An empty cacheDir disables the cache outright — every call asks the core.
func TestVerifyClusterRegistered_NoCacheDirAlwaysAsks(t *testing.T) {
	t.Parallel()
	const host = "aws-us-east-2.entire.io"
	lister := &countingLister{hosts: []string{host}}
	for i := range 2 {
		if err := VerifyClusterRegistered(t.Context(), lister, "", "https://core.us.entire.io", host); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	if lister.calls != 2 {
		t.Fatalf("registry consulted %d times, want 2 (caching must be off)", lister.calls)
	}
}

// clusterRegistryServer serves GET /api/v1/clusters with the given hosts (or a
// 500 when hosts is nil), and returns a *Client dialing it.
func clusterRegistryServer(t *testing.T, hosts []string) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != apiBasePath+"/clusters" {
			http.NotFound(w, r)
			return
		}
		if hosts == nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		clusters := make([]map[string]any, 0, len(hosts))
		for _, h := range hosts {
			clusters = append(clusters, map[string]any{
				"apiUrl": "https://api." + h, "isDefault": false,
				"jurisdiction": "us", "publicUrl": "https://" + h, "slug": h,
			})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"clusters": clusters}) //nolint:errcheck // best-effort in test stub
	}))
	t.Cleanup(srv.Close)
	c, err := NewWithBearer(srv.URL, "test-token")
	if err != nil {
		t.Fatalf("NewWithBearer: %v", err)
	}
	return c
}

// NewForCluster (the `entire repo mirror …` path) now dials the acting
// identity's core and gates on that core's registry, instead of discovering a
// core from the target host's own /.well-known document.
func TestNewForCluster(t *testing.T) {
	const clusterHost = "aws-us-east-2.entire.io"

	t.Run("registered host returns the active client", func(t *testing.T) {
		client := clusterRegistryServer(t, []string{clusterHost})
		withActiveClient(t, client)
		got, err := NewForCluster(t.Context(), clusterHost)
		if err != nil {
			t.Fatalf("NewForCluster: %v", err)
		}
		if got != client {
			t.Fatal("NewForCluster must return the active-context client, not a re-derived one")
		}
	})

	t.Run("unregistered host fails", func(t *testing.T) {
		client := clusterRegistryServer(t, []string{clusterHost})
		withActiveClient(t, client)
		got, err := NewForCluster(t.Context(), "evil.example.com")
		if !errors.Is(err, ErrClusterNotRegistered) {
			t.Fatalf("err = %v, want ErrClusterNotRegistered", err)
		}
		if got != nil {
			t.Fatal("no client may be returned for an unregistered cluster host")
		}
	})

	t.Run("registry failure fails closed", func(t *testing.T) {
		client := clusterRegistryServer(t, nil)
		withActiveClient(t, client)
		if _, err := NewForCluster(t.Context(), clusterHost); err == nil {
			t.Fatal("a registry that cannot be consulted must fail the command")
		}
	})
}

// withActiveClient points NewForCluster's client seam at c for one test. Not
// parallel-safe (a package-level var), which is why these subtests aren't.
func withActiveClient(t *testing.T, c *Client) {
	t.Helper()
	prev := newActiveClient
	newActiveClient = func() (*Client, error) { return c, nil }
	t.Cleanup(func() { newActiveClient = prev })
}
