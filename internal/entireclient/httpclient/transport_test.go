package httpclient

import (
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
