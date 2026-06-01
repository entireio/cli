// Package transport implements the HTTP layer the helper protocol
// speaks against: a Proxy that talks to one or more Entire data-plane
// replicas, applies failover on connection errors and 5xx responses,
// and bridges the warm/cold paths driven by X-Entire-Replicas.
//
// The Proxy is decoupled from authentication via a SetAuthFunc — the
// caller (cmd/entire's runRemoteHelper) wires the scoped-token mint in
// there. checkRedirect enforces the same-cluster Authorization carry
// rule so credentials never leak across hosts.
package transport

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/entireio/cli/internal/entireclient/discovery"
	"github.com/entireio/cli/internal/entireclient/httpclient"
	"github.com/entireio/cli/internal/remotehelper/debuglog"
	"github.com/entireio/cli/internal/remotehelper/httpdebug"
	"github.com/entireio/cli/internal/remotehelper/replicas"
)

// SetAuthFunc attaches authentication headers to an outbound request.
// Errors are surfaced verbatim — they short-circuit failover because
// they originate at the auth provider, not the data-plane node.
type SetAuthFunc func(*http.Request) error

// Config bundles the inputs needed to build a Proxy. Path is the URL
// path on each node (e.g. "/et/project/repo") and is appended to the
// node base for every request.
type Config struct {
	Nodes        replicas.NodeConfig
	Path         string
	SkipTLS      bool
	SetAuth      SetAuthFunc
	OnNodeFailed func(failedNode string)
}

// Proxy is the HTTP transport the helper protocol uses to talk to the
// data plane. Methods are not safe for concurrent use — Git's helper
// protocol is strictly serial.
type Proxy struct {
	// nodes is the current replica set. Populated from
	// X-Entire-Replicas headers and the on-disk cache; empty on a
	// cold invocation, in which case InfoRefs uses entryURL instead.
	nodes []string
	// entryURL is the cluster entry domain (e.g.
	// https://aws-us-east-2.entire.io). Used by InfoRefs when the
	// cached replica set is empty or exhausted.
	entryURL string
	// clusterHost is the cluster's entry hostname (no scheme, port
	// allowed). Used to decide which redirects are safe to carry
	// Authorization across — same-cluster only.
	clusterHost string
	// cacheHost is the key used when persisting replica sets. Empty
	// disables cache writes.
	cacheHost string
	// repoPath mirrors cacheHost — empty disables cache writes.
	repoPath string
	// path is the URL path on each node.
	path string

	client       *http.Client
	setAuth      SetAuthFunc
	onNodeFailed func(failedNode string)

	// stickyNode is the URL (matching one of nodes) of the replica
	// that served the most recent successful request, post-redirects.
	// Used as the starting offset for the next doWithFailover so
	// subsequent requests in the same fetch session reuse Go's
	// transport-pool TCP + TLS connection instead of paying a fresh
	// handshake per round.
	stickyNode string
}

// New builds a Proxy from the given configuration. The HTTP client is
// constructed here so that its CheckRedirect can reference the proxy
// (otherwise the cross-host Authorization-stripping policy would need
// duplicate state).
func New(cfg Config) *Proxy {
	debuglog.Printf("nodes=%v entry=%s path=%s skipTLS=%v", cfg.Nodes.InitialNodes, cfg.Nodes.EntryURL, cfg.Path, cfg.SkipTLS)

	p := &Proxy{
		entryURL:     cfg.Nodes.EntryURL,
		clusterHost:  cfg.Nodes.ClusterHost,
		path:         cfg.Path,
		setAuth:      cfg.SetAuth,
		onNodeFailed: cfg.OnNodeFailed,
	}
	// The cached node set (nodes.json) is attacker-influenceable: anything
	// running as the user can amend it. Scope it to the cluster trust domain
	// before any of these become credential-bearing dial targets.
	p.nodes = p.filterReplicas(cfg.Nodes.InitialNodes)
	if cfg.Nodes.Caching() {
		p.cacheHost = cfg.Nodes.ClusterHost
		p.repoPath = cfg.Nodes.RepoPath
	}

	transport := httpclient.NewTransport(cfg.SkipTLS)
	p.client = &http.Client{
		Transport:     &httpdebug.RoundTripper{Next: transport},
		CheckRedirect: p.checkRedirect,
	}
	return p
}

// ErrorBaseURL returns a URL string suitable for embedding in error
// messages, guarding against empty node lists.
func (p *Proxy) ErrorBaseURL() string {
	if len(p.nodes) > 0 {
		return p.nodes[0] + p.path
	}
	return p.path
}

// checkRedirect is the http.Client CheckRedirect hook. It caps
// redirect chains and enforces an explicit same-cluster policy for the
// Authorization header: in-cluster hops re-carry it (Go's default
// strips on cross-host redirect, which would otherwise drop the
// credential when info/refs 307s from the entry domain to a hosting
// replica), out-of-cluster hops strip it.
//
// The explicit strip matters: Go's default shouldCopyHeaderOnRedirect
// keys on hostname only, so same-host-different-port hops (common with
// local test servers, and possible with a misconfigured LB) would
// otherwise carry Authorization through. We'd rather drop the header
// than leak it.
func (p *Proxy) checkRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 10 {
		return errors.New("stopped after 10 redirects")
	}
	if len(via) == 0 {
		return nil
	}
	if p.clusterHost != "" && !discovery.HostInCluster(req.URL.Hostname(), p.clusterHost) {
		debuglog.Printf("redirect to %s is out-of-cluster (cluster=%s); stripping Authorization", req.URL.Host, p.clusterHost)
		req.Header.Del("Authorization")
		return nil
	}
	if auth := via[0].Header.Get("Authorization"); auth != "" {
		debuglog.Printf("redirect to %s is in-cluster (cluster=%s); preserving Authorization", req.URL.Host, p.clusterHost)
		req.Header.Set("Authorization", auth) //nolint:gosec // intentional: preserve auth for in-cluster hops (out-of-cluster is stripped above)
	}
	return nil
}

// hostInCluster reports whether host is the cluster's entry domain or a
// subdomain of it. With no clusterHost configured (un-pinned callers and
// some tests) it returns true — there is no trust domain to enforce, which
// matches checkRedirect's behaviour.
func (p *Proxy) hostInCluster(host string) bool {
	if p.clusterHost == "" {
		return true
	}
	return discovery.HostInCluster(host, p.clusterHost)
}

// replicaInCluster reports whether a replica base URL is safe to dial with
// the repo-scoped token attached: it must parse and resolve to a host
// inside the cluster trust domain. Replica sets arrive from
// attacker-influenceable sources — the X-Entire-Replicas response header, a
// redirect Location, and the on-disk node cache — so a malicious or poisoned
// entry pointing off-cluster would otherwise receive a core-minted bearer
// token. Dropping it here keeps credentials scoped to the cluster the user
// actually addressed.
func (p *Proxy) replicaInCluster(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" {
		return false
	}
	return p.hostInCluster(u.Hostname())
}

// filterReplicas drops replica URLs that fall outside the cluster trust
// domain, logging each rejection. A no-op when clusterHost is unset. See
// replicaInCluster.
func (p *Proxy) filterReplicas(urls []string) []string {
	if p.clusterHost == "" {
		return urls
	}
	kept := make([]string, 0, len(urls))
	for _, u := range urls {
		if p.replicaInCluster(u) {
			kept = append(kept, u)
			continue
		}
		debuglog.Printf("dropping out-of-cluster replica %q (cluster=%s): refusing to carry credentials off-cluster", u, p.clusterHost)
	}
	return kept
}

func (p *Proxy) nodeURL(nodeBase, suffix string) string {
	return strings.TrimSuffix(nodeBase, "/") + p.path + suffix
}

// shouldFailover returns true for HTTP status codes that indicate a
// node health issue (5xx). Client errors (4xx) are not retried since
// they would produce the same result on any node.
func shouldFailover(statusCode int) bool { return statusCode >= 500 }

// markNodeFailed removes a node from the in-memory list and notifies
// the OnNodeFailed callback. Subsequent calls (in this invocation)
// skip the removed node.
func (p *Proxy) markNodeFailed(node string) {
	filtered := make([]string, 0, len(p.nodes))
	for _, n := range p.nodes {
		if n != node {
			filtered = append(filtered, n)
		}
	}
	p.nodes = filtered

	if p.onNodeFailed != nil {
		p.onNodeFailed(node)
	}
}

// setAuthOrError applies the configured SetAuthFunc (if any) to the
// request. Errors come from the auth provider (core), not the data
// plane, so they short-circuit failover.
func (p *Proxy) setAuthOrError(req *http.Request) error {
	if p.setAuth == nil {
		return nil
	}
	// Hard chokepoint: every credential-bearing request flows through here.
	// Even if a node URL slipped past the ingress filters, refuse to stamp a
	// token onto a request bound for a host outside the cluster trust domain.
	if !p.hostInCluster(req.URL.Hostname()) {
		return fmt.Errorf("refusing to send credentials to out-of-cluster host %q (cluster %q)", req.URL.Hostname(), p.clusterHost)
	}
	return p.setAuth(req)
}

// doGet issues an authed GET to a fully qualified URL using the given
// client. Used for one-shot cold-path fetches that don't carry a
// request body — the cold-path probe (no-redirect client), the
// missing-replicas fallback, and the Location salvage all share this
// shape.
func (p *Proxy) doGet(ctx context.Context, urlStr string, client *http.Client, setHeaders func(*http.Request)) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, urlStr, nil)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	if err := p.setAuthOrError(req); err != nil {
		return nil, err
	}
	if setHeaders != nil {
		setHeaders(req)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("doing request: %w", err)
	}
	return resp, nil
}

// doWithFailover tries an HTTP request against each node, starting
// from a random offset (or the stickyNode if one is set and still in
// the replica list). Connection errors and 5xx responses trigger
// failover to the next node. The failed node is removed from the list
// and OnNodeFailed is called.
func (p *Proxy) doWithFailover(ctx context.Context, makeSuffix string, method string, body io.ReadSeeker, setHeaders func(*http.Request)) (*http.Response, error) {
	nodes := slices.Clone(p.nodes)
	if len(nodes) == 0 {
		return nil, errors.New("no healthy nodes available")
	}
	start := rand.IntN(len(nodes)) //nolint:gosec // load-spreading, not security
	if p.stickyNode != "" {
		if idx := slices.Index(nodes, p.stickyNode); idx >= 0 {
			start = idx
		}
	}
	var lastErr error

	for i := range nodes {
		node := nodes[(start+i)%len(nodes)]
		reqURL := p.nodeURL(node, makeSuffix)

		var bodyReader io.Reader
		if body != nil {
			if _, err := body.Seek(0, io.SeekStart); err != nil {
				return nil, fmt.Errorf("resetting request body: %w", err)
			}
			bodyReader = body
		}

		req, err := http.NewRequestWithContext(ctx, method, reqURL, bodyReader)
		if err != nil {
			return nil, fmt.Errorf("creating request: %w", err)
		}
		if err := p.setAuthOrError(req); err != nil {
			return nil, err
		}
		if setHeaders != nil {
			setHeaders(req)
		}

		resp, err := p.client.Do(req)
		if err != nil {
			debuglog.Printf("node %s unreachable: %v", node, err)
			lastErr = err
			p.markNodeFailed(node)
			continue
		}

		if shouldFailover(resp.StatusCode) {
			respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1024)) //nolint:errcheck // best-effort body read for error message
			_ = resp.Body.Close()
			msg := strings.TrimSpace(string(respBody))
			debuglog.Printf("node %s returned HTTP %d: %s", node, resp.StatusCode, msg)
			lastErr = fmt.Errorf("node %s: HTTP %d: %s", node, resp.StatusCode, msg)
			p.markNodeFailed(node)
			continue
		}

		// Record the actual responder for connection reuse on the next
		// request. Use the post-redirect URL so a 307 to a different
		// replica makes us stick to the redirect target, not the
		// bouncer.
		if resp.Request != nil && resp.Request.URL != nil {
			resolved := resp.Request.URL.Scheme + "://" + resp.Request.URL.Host
			if slices.Contains(nodes, resolved) {
				p.stickyNode = resolved
			}
		}
		return resp, nil
	}

	return nil, fmt.Errorf("all %d nodes failed, last error: %w", len(nodes), lastErr)
}

// noRedirectClient returns a one-shot client that shares this proxy's
// transport (so connection pooling still applies) but never follows
// redirects, surfacing 3xx responses to the caller.
func (p *Proxy) noRedirectClient() *http.Client {
	var transport http.RoundTripper
	if p.client != nil {
		transport = p.client.Transport
	}
	return &http.Client{
		Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func isRedirect(statusCode int) bool { return statusCode >= 300 && statusCode < 400 }

// responseHost returns the host of the URL that ultimately answered
// the request (after redirects), for log messages. Empty for a
// malformed response.
func responseHost(resp *http.Response) string {
	if resp == nil || resp.Request == nil || resp.Request.URL == nil {
		return ""
	}
	return resp.Request.URL.Host
}

// refreshReplicas updates p.nodes from the X-Entire-Replicas header on
// an info/refs response.
//
// When the header is present we treat it as the authoritative current
// replica set: in-memory state and on-disk cache are both updated.
// When it's absent we deliberately do *not* persist, because doing so
// would mask a server running without the discovery middleware — every
// op would shrink the cache to a single entry. Behaviour in that case:
//
//   - If a cached set already populated p.nodes, keep it untouched and
//     skip the cache write. Subsequent ops retry the cached set; if
//     those entries go bad the warm-path failover and entry-domain
//     fallback in InfoRefs take over.
//   - Otherwise (cold start with no header), use the URL that answered
//     as a single in-memory entry so the subsequent POST has somewhere
//     to go, but still don't persist — the next invocation retries
//     cold so the fix (server side) eventually shows up.
func (p *Proxy) refreshReplicas(resp *http.Response) {
	if rs := p.filterReplicas(discovery.ParseReplicas(resp.Header.Get("X-Entire-Replicas"))); len(rs) > 0 {
		debuglog.Printf("X-Entire-Replicas from %s: refreshed replica set to %d nodes %v", responseHost(resp), len(rs), rs)
		p.nodes = rs
		if p.cacheHost != "" && p.repoPath != "" {
			replicas.Persist(p.cacheHost, p.repoPath, rs)
		}
		return
	}

	debuglog.Printf("info/refs response from %s missing X-Entire-Replicas; not refreshing cache", responseHost(resp))

	if len(p.nodes) > 0 {
		return
	}
	// Cold start with no header: adopt the host that answered as a single
	// in-memory entry so the subsequent POST has a target — but only if it's
	// in-cluster, so a stripped-auth redirect to an off-cluster host can't
	// pin an attacker URL as the node set.
	if resp.Request != nil && resp.Request.URL != nil && p.hostInCluster(resp.Request.URL.Hostname()) {
		u := &url.URL{Scheme: resp.Request.URL.Scheme, Host: resp.Request.URL.Host}
		p.nodes = []string{u.String()}
	}
}
