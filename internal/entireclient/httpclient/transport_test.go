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

func TestNewTransportHonorsHTTPSProxy(t *testing.T) {
	const proxyURL = "http://proxy.example:8443"
	t.Setenv("HTTPS_PROXY", proxyURL)
	t.Setenv("https_proxy", "")
	t.Setenv("NO_PROXY", "")
	t.Setenv("no_proxy", "")

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://region.auth.entire.io/api/v1/repos/resolve", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	tr := NewTransport(false)
	if tr.Proxy == nil {
		t.Fatal("Proxy is nil")
	}
	got, err := tr.Proxy(req)
	if err != nil {
		t.Fatalf("Proxy: %v", err)
	}
	if got == nil || got.String() != proxyURL {
		t.Fatalf("Proxy()=%v, want %s", got, proxyURL)
	}
}
