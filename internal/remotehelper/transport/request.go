package transport

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// Do sends a request that is not a git smart-HTTP request to suffix under
// the repo path, for example "/bb/dedup". It uses the replica failover,
// sticky node, credential, 401 retry, redirect policy and User-Agent of the
// git requests. The credential function decides which suffixes may carry a
// credential, so a suffix that it does not admit fails before any send.
//
// Do returns every HTTP answer to the caller, 5xx included. A node that
// answers is up, so it stays in the replica set, and the caller decides
// whether to retry. The next request starts at the replica after the one
// that answered 5xx. Do fails over only on a connection error or a refused
// redirect. The git requests still fail over on a 5xx answer. The caller
// closes the response body.
//
// body may be nil. Do rewinds it before each attempt. A redirect resends the
// body only when net/http can rewind it itself (a *bytes.Reader, for
// example).
//
// Do is safe for concurrent use.
func (p *Proxy) Do(ctx context.Context, method, suffix string, body io.ReadSeeker, setHeaders func(*http.Request)) (*http.Response, error) {
	if !strings.HasPrefix(suffix, "/") {
		return nil, fmt.Errorf("request suffix %q does not start with /", suffix)
	}
	resp, err := p.failover(ctx, suffix, method, body, setHeaders, true)
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", method, suffix, err)
	}
	return resp, nil
}

// RepoLocation returns the cluster entry URL and the repo path. Unlike the
// node URLs, they stay the same across replica failover and between runs,
// so they key local per-repo state such as the xorb store.
func (p *Proxy) RepoLocation() (entryURL, repoPath string) {
	return p.entryURL, p.path
}

// HeaderBB asks entiredb to advertise entire-bb on receive-pack info/refs.
// The server advertises the capability only to a client that sends it,
// because some git clients refuse an unknown capability with a value.
const HeaderBB = "X-Entire-Bb"

// InfoRefsForPush is InfoRefs for git-receive-pack with HeaderBB, for a
// push that the helper may build itself. Every attempt of the failover,
// cold-path and redirect sequence carries the header.
func (p *Proxy) InfoRefsForPush(ctx context.Context) (io.ReadCloser, error) {
	return p.infoRefs(ctx, "info/refs", "/info/refs?service=git-receive-pack", func(req *http.Request) {
		req.Header.Set(HeaderBB, "1")
	})
}

// InfoRefsV2ForFetch is InfoRefsV2 with HeaderBB, for a fetch that may opt
// in to chunked recipes. The server then adds entire-bb-fetch to the fetch
// capability. Every attempt of the failover, cold-path and redirect sequence
// carries the header.
func (p *Proxy) InfoRefsV2ForFetch(ctx context.Context) (io.ReadCloser, error) {
	return p.infoRefs(ctx, "v2 info/refs", "/info/refs?service=git-upload-pack", func(req *http.Request) {
		req.Header.Set("Git-Protocol", "version=2")
		req.Header.Set(HeaderBB, "1")
	})
}

// UploadPackChunked is ServiceRPC for a git-upload-pack request that carries
// the entire-bb-fetch argument. A node that predates the argument refuses it
// with HTTP 500, so a 500 answer comes back at once as an *HTTPStatusError:
// the node stays in the replica set, and the caller can send the request
// again without the argument.
//
// Another 5xx answer does not go through ServiceRPC's failover, which would
// mark the node failed and purge the replica cache. The request goes to each
// other replica once instead, and a 500 from any of them comes back at once.
// The nodes stay in the replica set.
func (p *Proxy) UploadPackChunked(ctx context.Context, body io.ReadSeeker, extraHeaders ...func(*http.Request)) (io.ReadCloser, error) {
	const service = "git-upload-pack"
	setHeaders := serviceHeaders(service, extraHeaders)
	nodes, _ := p.failoverOrder()
	var lastErr error
	for range max(len(nodes), 1) {
		resp, err := p.failover(ctx, "/"+service, http.MethodPost, body, setHeaders, true)
		if err != nil {
			return nil, fmt.Errorf("calling %s: %w", service, err)
		}
		switch {
		case resp.StatusCode == http.StatusOK:
			return resp.Body, nil
		case resp.StatusCode > http.StatusInternalServerError:
			lastErr = p.httpError(resp)
		default:
			return nil, p.httpError(resp)
		}
	}
	return nil, lastErr
}
