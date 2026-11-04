package httpdebug

import (
	"crypto/tls"
	"fmt"
	"net/http"
	"net/http/httptrace"
	"strings"
	"sync"
	"time"

	"github.com/entireio/cli/internal/remotehelper/debuglog"
)

// TimingRoundTripper logs one ENTIRE_DEBUG-gated line per request with the
// wall-clock time to response headers and a breakdown of where it went:
//
//	conn=new|reused  whether the request rode a pooled connection
//	dns_ms           name resolution (absent for IP literals and reused conns)
//	connect_ms       TCP connect (absent for reused conns)
//	tls_ms           TLS handshake (absent for reused conns and plain HTTP)
//	ttfb_ms          request fully written to first response byte, i.e. one
//	                 round trip plus server processing time
//
// dur_ms stays the last field so line-oriented tooling can read it with $NF.
// Bodies stream after RoundTrip returns, so transfer time is not included
// unless a wrapper below this one buffers the body (httpdebug.RoundTripper
// does under ENTIRE_DEBUG).
type TimingRoundTripper struct {
	Next http.RoundTripper
	// Label distinguishes the client the request rode on (e.g. "auth", "git").
	Label string
}

// RoundTrip implements http.RoundTripper.
func (t *TimingRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	if !debuglog.Enabled() {
		//nolint:wrapcheck // passthrough - wrapping would change error semantics
		return t.Next.RoundTrip(req)
	}
	var ph phases
	req = req.WithContext(httptrace.WithClientTrace(req.Context(), ph.trace()))
	start := time.Now()
	resp, err := t.Next.RoundTrip(req)
	dur := time.Since(start)
	if err != nil {
		debuglog.Printf("timing: %s %s %s error=%v %s dur_ms=%d", t.Label, req.Method, req.URL.Redacted(), err, ph.String(), dur.Milliseconds())
		//nolint:wrapcheck // passthrough - wrapping would change error semantics
		return resp, err
	}
	debuglog.Printf("timing: %s %s %s status=%d %s dur_ms=%d", t.Label, req.Method, req.URL.Redacted(), resp.StatusCode, ph.String(), dur.Milliseconds())
	return resp, nil
}

// phases collects httptrace timestamps for one request. The mutex is needed
// because dual-stack dialing reports connect events from parallel goroutines.
type phases struct {
	mu                       sync.Mutex
	reused                   bool
	dnsStart, dnsDone        time.Time
	connectStart, connectEnd time.Time
	tlsStart, tlsDone        time.Time
	wrote, firstByte         time.Time
}

func (p *phases) trace() *httptrace.ClientTrace {
	at := func(dst *time.Time) {
		p.mu.Lock()
		*dst = time.Now()
		p.mu.Unlock()
	}
	return &httptrace.ClientTrace{
		GotConn: func(info httptrace.GotConnInfo) {
			p.mu.Lock()
			p.reused = info.Reused
			p.mu.Unlock()
		},
		DNSStart: func(httptrace.DNSStartInfo) { at(&p.dnsStart) },
		DNSDone:  func(httptrace.DNSDoneInfo) { at(&p.dnsDone) },
		ConnectStart: func(string, string) {
			p.mu.Lock()
			if p.connectStart.IsZero() {
				p.connectStart = time.Now()
			}
			p.mu.Unlock()
		},
		ConnectDone:          func(string, string, error) { at(&p.connectEnd) },
		TLSHandshakeStart:    func() { at(&p.tlsStart) },
		TLSHandshakeDone:     func(tls.ConnectionState, error) { at(&p.tlsDone) },
		WroteRequest:         func(httptrace.WroteRequestInfo) { at(&p.wrote) },
		GotFirstResponseByte: func() { at(&p.firstByte) },
	}
}

// String renders the observed phases as key=value fields. A phase that did
// not happen is omitted rather than printed as zero.
func (p *phases) String() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	var b strings.Builder
	if p.reused {
		b.WriteString("conn=reused")
	} else {
		b.WriteString("conn=new")
	}
	span := func(name string, from, to time.Time) {
		if !from.IsZero() && !to.IsZero() {
			fmt.Fprintf(&b, " %s=%d", name, to.Sub(from).Milliseconds())
		}
	}
	span("dns_ms", p.dnsStart, p.dnsDone)
	span("connect_ms", p.connectStart, p.connectEnd)
	span("tls_ms", p.tlsStart, p.tlsDone)
	span("ttfb_ms", p.wrote, p.firstByte)
	return b.String()
}
