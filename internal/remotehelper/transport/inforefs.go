package transport

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/entireio/cli/internal/entireclient/discovery"
	"github.com/entireio/cli/internal/remotehelper/debuglog"
)

// InfoRefs fetches the ref advertisement from the server. This is
// also the discovery entry point: the response's X-Entire-Replicas
// header refreshes the proxy's cached replica set, and every
// subsequent request in this invocation hits one of those replicas
// directly.
//
// Warm path: when the cache has replicas, they're tried with failover.
// If every cached replica fails we fall back to the cold path rather
// than surfacing the error — the cached set may be entirely stale
// (cluster resized, replicas moved) and the entry domain's LB will
// still route us to a live node.
//
// Cold path: probe the entry domain without auto-following 3xx. If it
// returns 307 + X-Entire-Replicas we adopt those replicas as the
// working set and run the warm-path failover instead of letting Go's
// HTTP client dial the redirect target — that matters when the
// redirect target's DNS or TCP is broken: failover rolls to the next
// replica instead of returning the dial error. When the redirect
// carries no replica header we fall back to following the redirect
// once. A direct 2xx is handled in place.
func (p *Proxy) InfoRefs(ctx context.Context, service string) (io.ReadCloser, error) {
	suffix := "/info/refs?service=" + service

	var warmErr error
	switch {
	case len(p.nodes) > 0:
		debuglog.Printf("info/refs warm path: trying %d cached replicas %v", len(p.nodes), p.nodes)
		resp, err := p.doWithFailover(ctx, suffix, http.MethodGet, nil, nil)
		if err == nil {
			return p.handleInfoRefsResponse(resp)
		}
		if p.entryURL == "" {
			return nil, fmt.Errorf("fetching info/refs: %w", err)
		}
		warmErr = err
		debuglog.Printf("all cached replicas failed for info/refs, falling back to entry URL %s: %v", p.entryURL, err)
	case p.entryURL == "":
		return nil, errors.New("no replicas available and no entry URL configured")
	default:
		debuglog.Printf("info/refs cold path: hitting entry URL %s (LB will route or 307 to a hosting replica)", p.entryURL)
	}

	body, err := p.coldInfoRefs(ctx, suffix, nil)
	if err != nil && warmErr != nil {
		return nil, errors.Join(fmt.Errorf("cached replicas: %w", warmErr), err)
	}
	return body, err
}

// InfoRefsV2 is InfoRefs with Git-Protocol: version=2 added — same
// replica-failover / cold-path / discovery logic, but the server's v2
// capability advertisement is what gets returned instead of the v0/v1
// ref list.
func (p *Proxy) InfoRefsV2(ctx context.Context) (io.ReadCloser, error) {
	suffix := "/info/refs?service=git-upload-pack"
	setHeaders := func(req *http.Request) {
		req.Header.Set("Git-Protocol", "version=2")
	}

	var warmErr error
	switch {
	case len(p.nodes) > 0:
		debuglog.Printf("v2 info/refs warm path: trying %d cached replicas %v", len(p.nodes), p.nodes)
		resp, err := p.doWithFailover(ctx, suffix, http.MethodGet, nil, setHeaders)
		if err == nil {
			return p.handleInfoRefsResponse(resp)
		}
		if p.entryURL == "" {
			return nil, fmt.Errorf("fetching v2 info/refs: %w", err)
		}
		warmErr = err
		debuglog.Printf("all cached replicas failed for v2 info/refs, falling back to entry URL %s: %v", p.entryURL, err)
	case p.entryURL == "":
		return nil, errors.New("no replicas available and no entry URL configured")
	default:
		debuglog.Printf("v2 info/refs cold path: hitting entry URL %s (LB will route or 307 to a hosting replica)", p.entryURL)
	}

	body, err := p.coldInfoRefs(ctx, suffix, setHeaders)
	if err != nil && warmErr != nil {
		return nil, errors.Join(fmt.Errorf("cached replicas: %w", warmErr), err)
	}
	return body, err
}

// coldInfoRefs fetches info/refs via the entry domain without
// following redirects automatically. See InfoRefs for the rationale.
//
// Order of attempts when the entry domain returns 307 + X-Entire-Replicas:
//  1. doWithFailover across the advertised replicas.
//  2. If every advertised replica fails, follow the original Location
//     target once. This salvages the request when the header was stale
//     but the LB-picked node is still healthy.
//
// Persisting the replica set is gated on a successful response so a
// stale header can't pin a bad set in the on-disk cache.
func (p *Proxy) coldInfoRefs(ctx context.Context, suffix string, setHeaders func(*http.Request)) (io.ReadCloser, error) {
	entryURL := strings.TrimSuffix(p.entryURL, "/") + p.path + suffix
	resp, err := p.doGet(ctx, entryURL, p.noRedirectClient(), setHeaders)
	if err != nil {
		return nil, fmt.Errorf("fetching info/refs from entry domain: %w", err)
	}

	if !isRedirect(resp.StatusCode) {
		return p.handleInfoRefsResponse(resp)
	}

	rs := p.filterReplicas(discovery.ParseReplicas(resp.Header.Get("X-Entire-Replicas")))
	location := resp.Header.Get("Location")
	_ = resp.Body.Close()

	if len(rs) == 0 {
		debuglog.Printf("entry domain redirect missing X-Entire-Replicas; falling back to redirect-following")
		followed, err := p.doGet(ctx, entryURL, p.client, setHeaders)
		if err != nil {
			return nil, fmt.Errorf("fetching info/refs from entry domain: %w", err)
		}
		return p.handleInfoRefsResponse(followed)
	}

	debuglog.Printf("entry domain redirected with %d replicas %v; failing over instead of following", len(rs), rs)
	p.nodes = rs

	viaReplicas, err := p.doWithFailover(ctx, suffix, http.MethodGet, nil, setHeaders)
	if err == nil {
		return p.handleInfoRefsResponse(viaReplicas)
	}

	if location == "" {
		return nil, fmt.Errorf("fetching info/refs after entry-domain redirect: %w", err)
	}
	// The Location target is server-controlled. Salvaging the request means
	// dialing it with the token attached, so it must be in-cluster — an
	// off-cluster Location is an exfiltration target, not a fallback node.
	if !p.replicaInCluster(location) {
		debuglog.Printf("all advertised replicas failed and redirect Location %s is out-of-cluster; refusing credentialed fallback", location)
		return nil, fmt.Errorf("fetching info/refs after entry-domain redirect: %w", err)
	}
	debuglog.Printf("all advertised replicas failed; trying redirect Location %s as last-ditch fallback: %v", location, err)
	locResp, locErr := p.doGet(ctx, location, p.client, setHeaders)
	if locErr != nil {
		return nil, fmt.Errorf("fetching info/refs after entry-domain redirect: %w", errors.Join(err, locErr))
	}
	return p.handleInfoRefsResponse(locResp)
}

// handleInfoRefsResponse refreshes the replica cache from the response
// and returns the body (or a typed HTTP error). The caller is
// responsible for closing the returned ReadCloser on success.
func (p *Proxy) handleInfoRefsResponse(resp *http.Response) (io.ReadCloser, error) {
	p.refreshReplicas(resp)

	if resp.StatusCode != http.StatusOK {
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, 1024))
		_ = resp.Body.Close()
		var msg string
		if readErr == nil {
			msg = strings.TrimSpace(string(body))
		}
		if strings.HasPrefix(msg, "<") {
			msg = ""
		}
		return nil, HTTPErrorMessage(resp.StatusCode, msg, p.ErrorBaseURL())
	}
	return resp.Body, nil
}

// ServiceRPC sends data to a Git service endpoint and returns the
// response. The extraHeaders callbacks are invoked on each request to
// set additional headers (e.g. Git-Protocol, X-Entire-Push-Size).
func (p *Proxy) ServiceRPC(ctx context.Context, service string, body io.ReadSeeker, extraHeaders ...func(*http.Request)) (io.ReadCloser, error) {
	suffix := "/" + service

	setHeaders := func(req *http.Request) {
		req.Header.Set("Content-Type", fmt.Sprintf("application/x-%s-request", service))
		req.Header.Set("Accept", fmt.Sprintf("application/x-%s-result", service))
		for _, fn := range extraHeaders {
			fn(req)
		}
	}

	resp, err := p.doWithFailover(ctx, suffix, http.MethodPost, body, setHeaders)
	if err != nil {
		return nil, fmt.Errorf("calling %s: %w", service, err)
	}

	if resp.StatusCode != http.StatusOK {
		respBody, readErr := io.ReadAll(io.LimitReader(resp.Body, 1024))
		_ = resp.Body.Close()
		var msg string
		if readErr == nil {
			msg = strings.TrimSpace(string(respBody))
		}
		if strings.HasPrefix(msg, "<") {
			msg = ""
		}
		return nil, HTTPErrorMessage(resp.StatusCode, msg, p.ErrorBaseURL())
	}

	return resp.Body, nil
}

// HTTPErrorMessage returns a user-friendly error for non-200 HTTP
// responses. Exposed so handlers outside the transport package can
// produce the same shape.
func HTTPErrorMessage(statusCode int, serverMsg, baseURL string) error {
	switch statusCode {
	case http.StatusUnauthorized:
		if serverMsg != "" {
			return fmt.Errorf("authentication failed: %s", serverMsg)
		}
		return errors.New("authentication failed - please run 'entire login'")
	case http.StatusNotFound:
		if serverMsg != "" {
			return errors.New(serverMsg)
		}
		return fmt.Errorf("repository not found: %s", baseURL)
	default:
		if serverMsg != "" {
			return fmt.Errorf("server error (HTTP %d): %s", statusCode, serverMsg)
		}
		return fmt.Errorf("server error (HTTP %d)", statusCode)
	}
}
