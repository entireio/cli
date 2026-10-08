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

// EnvConnectTimeout is the env var that overrides every connect timeout.
const EnvConnectTimeout = "ENTIRE_CONNECT_TIMEOUT_SECONDS"

// DefaultDialTimeout is the per-host TCP connect timeout. Short by default
// so failover paths skip dead nodes quickly, but long enough to absorb a
// slow initial connect (cold DNS, TLS-fronting LB, distant region) that a
// tighter budget would trip; override via ENTIRE_CONNECT_TIMEOUT_SECONDS on
// slow links (e.g. satellite) where even this trips before the node answers.
const DefaultDialTimeout = 4 * time.Second

// DefaultDiscoveryDialTimeout is the per-host TCP connect budget for the
// initial replica-discovery request — the cold info/refs probe to the
// cluster entry domain that answers "which nodes serve this repo". It is
// deliberately longer than DefaultDialTimeout: this first contact often pays
// a cold DNS + TLS handshake to a possibly-distant entry LB, and unlike
// replica failover there is no second node to roll to, so tripping here fails
// the whole clone/fetch. Replica failover keeps the short DefaultDialTimeout
// so dead nodes are still skipped quickly.
//
// An explicit ENTIRE_CONNECT_TIMEOUT_SECONDS overrides this too: a user who
// sets it gets that one value for every connect, discovery included.
const DefaultDiscoveryDialTimeout = 10 * time.Second

// envConnectTimeout reads ENTIRE_CONNECT_TIMEOUT_SECONDS. It returns the
// parsed duration and true only when the var is set to a positive integer;
// an unset/blank var yields false, and an invalid value warns to stderr and
// yields false so callers fall back to their own default.
func envConnectTimeout() (time.Duration, bool) {
	v, ok := os.LookupEnv(EnvConnectTimeout)
	if !ok || v == "" {
		return 0, false
	}
	secs, err := strconv.Atoi(v)
	if err != nil || secs <= 0 {
		fmt.Fprintf(os.Stderr, "httpclient: ignoring invalid %s=%q, using defaults (%s failover, %s discovery)\n", EnvConnectTimeout, v, DefaultDialTimeout, DefaultDiscoveryDialTimeout)
		return 0, false
	}
	return time.Duration(secs) * time.Second, true
}

// DialTimeout returns the connect timeout for ordinary requests (and replica
// failover): the ENTIRE_CONNECT_TIMEOUT_SECONDS override if set, else
// DefaultDialTimeout.
func DialTimeout() time.Duration {
	if d, ok := envConnectTimeout(); ok {
		return d
	}
	return DefaultDialTimeout
}

// DiscoveryDialTimeout returns the connect budget for the discovery probe.
// When ENTIRE_CONNECT_TIMEOUT_SECONDS is set it wins outright (the user's
// chosen value applies to every connect); otherwise it falls back to the
// more patient DefaultDiscoveryDialTimeout rather than DefaultDialTimeout.
func DiscoveryDialTimeout() time.Duration {
	if d, ok := envConnectTimeout(); ok {
		return d
	}
	return DefaultDiscoveryDialTimeout
}

// NewTransport builds an *http.Transport with the configured dial timeout
// and a baseline TLS config. Callers wrap the returned transport with their
// own RoundTripper as needed (e.g. debug logging) and assemble their own
// *http.Client around it.
func NewTransport(skipTLSVerify bool) *http.Transport {
	return newTransport(skipTLSVerify, DialTimeout())
}

// NewDiscoveryTransport is NewTransport with the longer discovery connect
// budget (DiscoveryDialTimeout). Use it for the initial replica-discovery
// probe; see DefaultDiscoveryDialTimeout for why it is more patient.
func NewDiscoveryTransport(skipTLSVerify bool) *http.Transport {
	return newTransport(skipTLSVerify, DiscoveryDialTimeout())
}

func newTransport(skipTLSVerify bool, dialTimeout time.Duration) *http.Transport {
	return &http.Transport{
		DialContext: (&net.Dialer{Timeout: dialTimeout}).DialContext,
		TLSClientConfig: &tls.Config{
			MinVersion:         tls.VersionTLS12,
			InsecureSkipVerify: skipTLSVerify, //nolint:gosec // intentional for local development
		},
	}
}
