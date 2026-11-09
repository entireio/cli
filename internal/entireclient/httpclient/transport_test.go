package httpclient

import (
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
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

// TestNewTransportRoutesThroughHTTPSProxy re-runs itself in a child process
// because http.ProxyFromEnvironment reads the proxy variables once per process:
// t.Setenv here would be ignored if any earlier request had already cached them.
func TestNewTransportRoutesThroughHTTPSProxy(t *testing.T) {
	t.Parallel()
	const childEnv = "HTTPCLIENT_PROXY_TEST_CHILD"
	const target = "region.auth.entire.io"
	if os.Getenv(childEnv) == "1" {
		client := &http.Client{Transport: NewTransport(false)}
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://"+target+"/api/v1/repos/resolve", nil)
		if err != nil {
			t.Fatalf("NewRequest: %v", err)
		}
		// The proxy refuses the tunnel, so an error is expected; the parent
		// asserts on what the proxy received.
		if resp, err := client.Do(req); err == nil {
			_ = resp.Body.Close()
		}
		return
	}

	connects := make(chan string, 1)
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodConnect {
			select {
			case connects <- r.Host:
			default:
			}
		}
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer proxy.Close()

	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestNewTransportRoutesThroughHTTPSProxy$")
	cmd.Env = append(os.Environ(),
		childEnv+"=1",
		"HTTPS_PROXY="+proxy.URL, "https_proxy=",
		"NO_PROXY=", "no_proxy=",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("child: %v\n%s", err, out)
	}

	select {
	case host := <-connects:
		if host != target+":443" {
			t.Fatalf("CONNECT host = %q, want %s:443", host, target)
		}
	default:
		t.Fatal("request never reached the proxy")
	}
}
