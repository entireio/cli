package cli

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestOrderHostsByLatency(t *testing.T) {
	t.Parallel()

	t.Run("orders measured hosts nearest first", func(t *testing.T) {
		t.Parallel()
		// The case the feature exists for: alphabetical puts the far cluster
		// first purely because of the letter `a`.
		hosts := []string{"aws-ap-southeast-2.entire.io", "aws-us-east-2.entire.io"}
		got := orderHostsByLatency(hosts, map[string]probeResult{
			"aws-ap-southeast-2.entire.io": {rtt: 240 * time.Millisecond},
			"aws-us-east-2.entire.io":      {rtt: 18 * time.Millisecond},
		})
		require.Equal(t, []string{"aws-us-east-2.entire.io", "aws-ap-southeast-2.entire.io"}, got)
	})

	t.Run("unmeasured hosts sort after measured ones in input order", func(t *testing.T) {
		t.Parallel()
		hosts := []string{"a.entire.io", "b.entire.io", "c.entire.io"}
		got := orderHostsByLatency(hosts, map[string]probeResult{"c.entire.io": {rtt: 5 * time.Millisecond}})
		require.Equal(t, []string{"c.entire.io", "a.entire.io", "b.entire.io"}, got)
	})

	t.Run("a timed-out host sorts below every answer and above the unmeasurable", func(t *testing.T) {
		t.Parallel()
		// Three tiers in one assertion. The timed-out host is slower than any
		// host that answered, but it produced evidence the refused one did not,
		// so it outranks it.
		hosts := []string{"refused.entire.io", "slow.entire.io", "far.entire.io", "near.entire.io"}
		got := orderHostsByLatency(hosts, map[string]probeResult{
			"slow.entire.io": {timedOut: true},
			"far.entire.io":  {rtt: 240 * time.Millisecond},
			"near.entire.io": {rtt: 18 * time.Millisecond},
		})
		require.Equal(t, []string{"near.entire.io", "far.entire.io", "slow.entire.io", "refused.entire.io"}, got)
	})

	t.Run("timed-out hosts keep the input order among themselves", func(t *testing.T) {
		t.Parallel()
		// They are equally unknown past the budget, so inventing an order
		// between them would be a ranking the probe never measured.
		hosts := []string{"b.entire.io", "a.entire.io"}
		got := orderHostsByLatency(hosts, map[string]probeResult{
			"a.entire.io": {timedOut: true},
			"b.entire.io": {timedOut: true},
		})
		require.Equal(t, hosts, got)
	})

	t.Run("no measurements preserves the input order", func(t *testing.T) {
		t.Parallel()
		// Every probe failing must leave the picker exactly as it was before
		// this feature existed, not reshuffled by a partial signal.
		hosts := []string{"a.entire.io", "b.entire.io"}
		require.Equal(t, hosts, orderHostsByLatency(hosts, nil))
	})
}

func TestNearestHost(t *testing.T) {
	t.Parallel()

	hosts := []string{"near.entire.io", "far.entire.io"}

	t.Run("displaces a measured incumbent that lost", func(t *testing.T) {
		t.Parallel()
		got, ok := nearestHost(hosts, "far.entire.io", map[string]probeResult{
			"near.entire.io": {rtt: 12 * time.Millisecond},
			"far.entire.io":  {rtt: 230 * time.Millisecond},
		})
		require.True(t, ok)
		require.Equal(t, "near.entire.io", got)
	})

	t.Run("a small lead still wins", func(t *testing.T) {
		t.Parallel()
		// No margin: the caller asked for the nearest placement, so the nearest
		// placement is the answer even when the lead is small.
		got, ok := nearestHost(hosts, "far.entire.io", map[string]probeResult{
			"near.entire.io": {rtt: 14 * time.Millisecond},
			"far.entire.io":  {rtt: 20 * time.Millisecond},
		})
		require.True(t, ok)
		require.Equal(t, "near.entire.io", got)
	})

	t.Run("an unmeasured incumbent is never displaced", func(t *testing.T) {
		t.Parallel()
		// The regression: a primary that dropped one probe used to lose to a
		// mirror measured at 300ms, which was then announced as "nearest" and
		// written into .git/config. Silence is not a measurement, and the
		// challenger being slow in absolute terms is beside the point — there
		// is no second number to compare it against.
		_, ok := nearestHost(hosts, "far.entire.io", map[string]probeResult{
			"near.entire.io": {rtt: 300 * time.Millisecond},
		})
		require.False(t, ok)
	})

	t.Run("a timed-out incumbent is never displaced", func(t *testing.T) {
		t.Parallel()
		// ">400ms" is a lower bound, not a measurement: it ranks the host in the
		// picker but cannot lose a comparison on the caller's behalf.
		_, ok := nearestHost(hosts, "far.entire.io", map[string]probeResult{
			"near.entire.io": {rtt: 12 * time.Millisecond},
			"far.entire.io":  {timedOut: true},
		})
		require.False(t, ok)
	})

	t.Run("an incumbent that is already nearest stays", func(t *testing.T) {
		t.Parallel()
		_, ok := nearestHost(hosts, "near.entire.io", map[string]probeResult{
			"near.entire.io": {rtt: 12 * time.Millisecond},
			"far.entire.io":  {rtt: 230 * time.Millisecond},
		})
		require.False(t, ok)
	})

	t.Run("the incumbent matches case-insensitively", func(t *testing.T) {
		t.Parallel()
		// defaultHost comes from the API in whatever case it stores; the probe
		// map is keyed by the picker's folded hosts. A case difference must not
		// read as an unmeasured incumbent and silently disable the flag.
		got, ok := nearestHost(hosts, "FAR.Entire.IO", map[string]probeResult{
			"near.entire.io": {rtt: 12 * time.Millisecond},
			"far.entire.io":  {rtt: 230 * time.Millisecond},
		})
		require.True(t, ok)
		require.Equal(t, "near.entire.io", got)
	})

	t.Run("a timed-out challenger never wins", func(t *testing.T) {
		t.Parallel()
		_, ok := nearestHost(hosts, "far.entire.io", map[string]probeResult{
			"near.entire.io": {timedOut: true},
			"far.entire.io":  {rtt: 230 * time.Millisecond},
		})
		require.False(t, ok)
	})

	t.Run("no measurement means no nearest", func(t *testing.T) {
		t.Parallel()
		// Every probe failed, so there is nothing to choose on and the caller
		// must say so rather than fall back to first-measured-wins.
		_, ok := nearestHost(hosts, "far.entire.io", nil)
		require.False(t, ok)
	})

	t.Run("an incumbent outside the candidate set is not displaced", func(t *testing.T) {
		t.Parallel()
		// A caller that could not name a primary passes "". There is then no
		// incumbent to measure, so the selection falls through to the caller's
		// own default handling rather than picking on one number alone.
		_, ok := nearestHost(hosts, "", map[string]probeResult{
			"near.entire.io": {rtt: 12 * time.Millisecond},
		})
		require.False(t, ok)
	})
}

func TestFormatProbedRTT(t *testing.T) {
	t.Parallel()
	require.Equal(t, "18ms", formatProbedRTT(probeResult{rtt: 18 * time.Millisecond}, true))
	require.Equal(t, "2ms", formatProbedRTT(probeResult{rtt: 1600 * time.Microsecond}, true))
	// A host still connecting at the budget prints the bound, not a blank: the
	// reader needs to weigh it against the hosts that did answer. The figure
	// comes from placementProbeBudget so the label cannot drift from the cap.
	require.Equal(t, ">400ms", formatProbedRTT(probeResult{timedOut: true}, true))
	require.Equal(t, ">"+formatMillis(placementProbeBudget), formatProbedRTT(probeResult{timedOut: true}, true))
	// Unmeasurable stays blank: a refusal is immediate and says nothing about
	// distance, so ">400ms" would report a latency never observed.
	require.Empty(t, formatProbedRTT(probeResult{}, false))
}

func TestProbeAddress(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		host string
		want string
	}{
		{name: "bare host takes the default port", host: "aws-us-east-2.entire.io", want: "aws-us-east-2.entire.io:443"},
		// validateClusterHost admits a bare host[:port], so a dev or self-hosted
		// cluster on another port is a legal placement. Appending 443 to it
		// produced an address that never resolves, and the placement dropped out
		// of the ranking while `git clone` reached it fine.
		{name: "an explicit port is preserved", host: "localhost:8080", want: "localhost:8080"},
		{name: "bare IPv6 is bracketed once", host: "::1", want: "[::1]:443"},
		{name: "pre-bracketed IPv6 is not double-bracketed", host: "[::1]", want: "[::1]:443"},
		{name: "bracketed IPv6 with a port is preserved", host: "[::1]:8080", want: "[::1]:8080"},
		{name: "IPv4 takes the default port", host: "127.0.0.1", want: "127.0.0.1:443"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tt.want, probeAddress(tt.host))
		})
	}
}

// TestDialLatencies covers the real dial path rather than the stub the
// selection tests inject: address construction, the concurrent fan-out, and
// what happens to a host that does not answer. It stays hermetic by probing
// loopback listeners it owns, so it needs no network and no name resolution.
// listenLoopback opens a loopback listener on an arbitrary free port. It goes
// through net.ListenConfig rather than net.Listen so the listen is bound to the
// test's context (noctx), which also tears it down if the test is cancelled.
func listenLoopback(t *testing.T) net.Listener {
	t.Helper()
	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	return ln
}

func TestDialLatencies(t *testing.T) {
	t.Parallel()

	// listen returns a live loopback listener's host:port. The listener closes
	// with the test, so nothing outlives it. ListenConfig rather than
	// net.Listen because the latter takes no context (noctx).
	listen := func(t *testing.T) string {
		t.Helper()
		ln := listenLoopback(t)
		t.Cleanup(func() { _ = ln.Close() })
		return ln.Addr().String()
	}

	t.Run("measures every reachable host", func(t *testing.T) {
		t.Parallel()
		// Both carry an explicit port, which is also the regression guard for
		// probeAddress: with 443 appended neither would resolve and the map
		// would come back empty.
		a, b := listen(t), listen(t)
		got := dialLatencies(t.Context(), []string{a, b})
		require.Len(t, got, 2)
		require.Contains(t, got, a)
		require.Contains(t, got, b)
		require.Positive(t, got[a].rtt)
		require.False(t, got[a].timedOut)
	})

	t.Run("omits a host that refuses the connection", func(t *testing.T) {
		t.Parallel()
		// A listener closed before the probe leaves a port nothing answers on,
		// which is the reachable-then-gone case the ordering must survive.
		ln := listenLoopback(t)
		dead := ln.Addr().String()
		require.NoError(t, ln.Close())

		live := listen(t)
		got := dialLatencies(t.Context(), []string{live, dead})
		require.Contains(t, got, live)
		require.NotContains(t, got, dead, "an unreachable host must be absent, not present with a sentinel")
	})

	t.Run("an expired budget marks the host timed out", func(t *testing.T) {
		t.Parallel()
		// An already-passed deadline is the deterministic stand-in for the
		// budget running out mid-dial: DialContext returns immediately with
		// DeadlineExceeded, which is the classification under test. The host is
		// present and flagged, not absent, so the picker can print ">400ms".
		ctx, cancel := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
		defer cancel()
		host := listen(t)

		got := dialLatencies(ctx, []string{host})
		require.Contains(t, got, host)
		require.True(t, got[host].timedOut)
		require.Zero(t, got[host].rtt, "a timed-out host has no round trip to report")
	})

	t.Run("a cancelled context measures nothing", func(t *testing.T) {
		t.Parallel()
		// The budget's deadline reaches the dials through the context. Cancelling
		// up front is the deterministic stand-in for it expiring, and proves no
		// host is credited with a measurement it never produced.
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		require.Empty(t, dialLatencies(ctx, []string{listen(t)}))
	})

	t.Run("no hosts is not an error", func(t *testing.T) {
		t.Parallel()
		require.Empty(t, dialLatencies(t.Context(), nil))
	})
}
