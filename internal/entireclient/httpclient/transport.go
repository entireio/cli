// Package httpclient builds *http.Transport instances shared by Entire's
// client binaries (git-remote-entire, entiredb, entire-deploy). Centralizing
// transport construction means one place to honor cross-cutting knobs like
// ENTIRE_CONNECT_TIMEOUT_SECONDS — without forcing callers through a single
// *http.Client constructor, since they legitimately differ in CheckRedirect,
// per-client Timeout, and request-level wrapping.
package httpclient

import (
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"os"
	"strconv"
	"time"
)

// DefaultDialTimeout is the per-host TCP connect timeout. Short by default
// so failover paths skip dead nodes quickly, but long enough to absorb a
// slow initial connect (cold DNS, TLS-fronting LB, distant region) that a
// tighter budget would trip; override via ENTIRE_CONNECT_TIMEOUT_SECONDS on
// slow links where even this trips before the node can answer.
const DefaultDialTimeout = 2 * time.Second

// DialTimeout returns the configured dial timeout, honoring
// ENTIRE_CONNECT_TIMEOUT_SECONDS. Invalid values fall back to
// DefaultDialTimeout with a warning to stderr.
func DialTimeout() time.Duration {
	v := os.Getenv("ENTIRE_CONNECT_TIMEOUT_SECONDS")
	if v == "" {
		return DefaultDialTimeout
	}
	secs, err := strconv.Atoi(v)
	if err != nil || secs <= 0 {
		fmt.Fprintf(os.Stderr, "httpclient: ignoring invalid ENTIRE_CONNECT_TIMEOUT_SECONDS=%q, using default %s\n", v, DefaultDialTimeout)
		return DefaultDialTimeout
	}
	return time.Duration(secs) * time.Second
}

// NewTransport builds an *http.Transport with the configured dial timeout
// and a baseline TLS config. Callers wrap the returned transport with their
// own RoundTripper as needed (e.g. debug logging) and assemble their own
// *http.Client around it.
func NewTransport(skipTLSVerify bool) *http.Transport {
	return &http.Transport{
		DialContext: (&net.Dialer{Timeout: DialTimeout()}).DialContext,
		TLSClientConfig: &tls.Config{
			MinVersion:         tls.VersionTLS12,
			InsecureSkipVerify: skipTLSVerify, //nolint:gosec // intentional for local development
		},
	}
}
