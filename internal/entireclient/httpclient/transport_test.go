package httpclient

import (
	"net/http"
	"testing"
	"time"
)

func TestDialTimeout(t *testing.T) {
	tests := []struct {
		name string
		env  string
		want time.Duration
	}{
		{"blank uses default", "", DefaultDialTimeout},
		{"valid seconds", "10", 10 * time.Second},
		{"single second", "1", 1 * time.Second},
		{"non-integer falls back", "abc", DefaultDialTimeout},
		{"zero falls back", "0", DefaultDialTimeout},
		{"negative falls back", "-5", DefaultDialTimeout},
		{"empty string falls back", "", DefaultDialTimeout},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("ENTIRE_CONNECT_TIMEOUT_SECONDS", tc.env)
			if got := DialTimeout(); got != tc.want {
				t.Fatalf("DialTimeout()=%s, want %s", got, tc.want)
			}
		})
	}
}

func TestDiscoveryDialTimeout(t *testing.T) {
	tests := []struct {
		name string
		env  string
		want time.Duration
	}{
		{"blank uses discovery default", "", DefaultDiscoveryDialTimeout},
		{"override below default still wins", "2", 2 * time.Second},
		{"override equal to default", "10", 10 * time.Second},
		{"larger override wins", "20", 20 * time.Second},
		{"invalid falls back to discovery default", "abc", DefaultDiscoveryDialTimeout},
		{"zero falls back to discovery default", "0", DefaultDiscoveryDialTimeout},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("ENTIRE_CONNECT_TIMEOUT_SECONDS", tc.env)
			if got := DiscoveryDialTimeout(); got != tc.want {
				t.Fatalf("DiscoveryDialTimeout()=%s, want %s", got, tc.want)
			}
		})
	}
}

func TestNewTransportTimeouts(t *testing.T) {
	t.Setenv(EnvConnectTimeout, "")
	tests := []struct {
		name string
		tr   *http.Transport
		want time.Duration
	}{
		{"failover", NewTransport(false), DefaultDialTimeout},
		{"discovery", NewDiscoveryTransport(false), DefaultDiscoveryDialTimeout},
	}
	for _, tc := range tests {
		if tc.tr.TLSHandshakeTimeout != tc.want {
			t.Errorf("%s: TLSHandshakeTimeout=%s, want %s", tc.name, tc.tr.TLSHandshakeTimeout, tc.want)
		}
		if tc.tr.ResponseHeaderTimeout != 0 {
			t.Errorf("%s: ResponseHeaderTimeout=%s, want 0 (push/fetch may send headers late)", tc.name, tc.tr.ResponseHeaderTimeout)
		}
	}
}

func TestNewTransportTLSHandshakeHonorsOverride(t *testing.T) {
	t.Setenv(EnvConnectTimeout, "30")
	for name, tr := range map[string]*http.Transport{
		"failover":  NewTransport(false),
		"discovery": NewDiscoveryTransport(false),
	} {
		if tr.TLSHandshakeTimeout != 30*time.Second {
			t.Errorf("%s: TLSHandshakeTimeout=%s, want 30s", name, tr.TLSHandshakeTimeout)
		}
	}
}
