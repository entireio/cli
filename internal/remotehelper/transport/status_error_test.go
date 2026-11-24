package transport

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/entireio/cli/internal/entireclient/httpclient"
	"github.com/entireio/cli/internal/remotehelper/replicas"
)

func TestNewHTTPStatusError(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		statusCode int
		serverMsg  string
		wantText   string
	}{
		{"401 bare", http.StatusUnauthorized, "", "authentication failed even after refreshing credentials - run 'entire login' if this persists"},
		{"401 with message", http.StatusUnauthorized, "token expired", "authentication failed: token expired"},
		{"403", http.StatusForbidden, "no read access", "server error (HTTP 403): no read access"},
		{"404 bare", http.StatusNotFound, "", "repository not found: https://n/gh/o/r"},
		{"404 with message", http.StatusNotFound, "repo gone", "repo gone"},
		{"500 bare", http.StatusInternalServerError, "", "server error (HTTP 500)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := newHTTPStatusError(tt.statusCode, tt.serverMsg, "https://n/gh/o/r")
			var statusErr *HTTPStatusError
			if !errors.As(err, &statusErr) {
				t.Fatalf("want *HTTPStatusError, got %T: %v", err, err)
			}
			if statusErr.StatusCode != tt.statusCode || statusErr.ServerMsg != tt.serverMsg {
				t.Errorf("got status %d msg %q, want %d %q", statusErr.StatusCode, statusErr.ServerMsg, tt.statusCode, tt.serverMsg)
			}
			if err.Error() != tt.wantText {
				t.Errorf("Error() = %q, want %q", err.Error(), tt.wantText)
			}
		})
	}
}

func TestDoWithFailover_ExhaustedNodesAreTyped(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		fmt.Fprint(w, "<html><body>Bad Gateway</body></html>")
	}))
	defer server.Close()

	_, err := testProxy(server).ServiceRPC(context.Background(), "git-upload-pack", strings.NewReader("0000"))
	if !errors.Is(err, ErrNoNodeSucceeded) {
		t.Fatalf("want ErrNoNodeSucceeded, got %v", err)
	}
	var statusErr *HTTPStatusError
	if !errors.As(err, &statusErr) || statusErr.StatusCode != http.StatusBadGateway {
		t.Fatalf("want wrapped *HTTPStatusError with HTTP 502, got %v", err)
	}
	if strings.Contains(err.Error(), "<html>") {
		t.Errorf("HTML body leaked into error: %q", err.Error())
	}
}

func TestDoWithFailover_UnreachableNodesAreTyped(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.NotFoundHandler())
	p := testProxy(server)
	server.Close()

	_, err := p.ServiceRPC(context.Background(), "git-upload-pack", strings.NewReader("0000"))
	if !errors.Is(err, ErrNoNodeSucceeded) {
		t.Fatalf("want ErrNoNodeSucceeded, got %v", err)
	}
}

func TestInfoRefs_FallbackStatusWinsOverCachedReplicaStatus(t *testing.T) {
	t.Parallel()
	const entry = "https://cluster.example"
	const cached = "https://n.cluster.example"
	p := New(Config{Nodes: replicas.NodeConfig{EntryURL: entry, ClusterHost: "cluster.example", InitialNodes: []string{cached}}})
	p.client.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		code := http.StatusNotFound
		if req.URL.Host == "n.cluster.example" {
			code = http.StatusServiceUnavailable
		}
		return &http.Response{StatusCode: code, Header: make(http.Header), Body: http.NoBody, Request: req}, nil
	})
	p.discoveryTransport = p.client.Transport

	_, err := p.InfoRefs(t.Context(), "git-upload-pack")
	var statusErr *HTTPStatusError
	if !errors.As(err, &statusErr) {
		t.Fatalf("want *HTTPStatusError, got %T: %v", err, err)
	}
	if statusErr.StatusCode != http.StatusNotFound {
		t.Errorf("classified HTTP %d, want the fallback's 404: %v", statusErr.StatusCode, err)
	}
	if errors.Is(err, ErrNoNodeSucceeded) {
		t.Errorf("cached replica failure must not drive classification: %v", err)
	}
	if !strings.Contains(err.Error(), "cached replicas") || !strings.Contains(err.Error(), "HTTP 503") {
		t.Errorf("cached replica failure missing from debug text: %q", err.Error())
	}
}

func TestDoWithFailover_ExhaustedByPolicyMatchesRequestPolicy(t *testing.T) {
	t.Parallel()
	p := New(Config{Nodes: replicas.NodeConfig{EntryURL: "https://cluster.example", ClusterHost: "cluster.example", InitialNodes: []string{"https://a.cluster.example", "https://b.cluster.example"}}})
	p.client.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		h := make(http.Header)
		h.Set("Location", "http://"+req.URL.Host+"/repo")
		return &http.Response{StatusCode: http.StatusTemporaryRedirect, Header: h, Body: http.NoBody, Request: req}, nil
	})

	_, err := p.ServiceRPC(t.Context(), "git-upload-pack", strings.NewReader("0000"))
	if !errors.Is(err, ErrNoNodeSucceeded) || !errors.Is(err, httpclient.ErrRequestPolicy) {
		t.Fatalf("want ErrNoNodeSucceeded and ErrRequestPolicy, got %v", err)
	}
}

func TestInfoRefs_UnreachableEntryDomainIsTyped(t *testing.T) {
	t.Parallel()
	entry := deadServerURL(t)
	p := proxyWithClient(nil, "/gh/o/r", entry, "127.0.0.1", &http.Client{})

	_, err := p.InfoRefs(t.Context(), "git-upload-pack")
	if !errors.Is(err, ErrUnreachable) {
		t.Fatalf("want ErrUnreachable, got %v", err)
	}
}

func TestDoWithFailover_NoEligibleReplicaIsTyped(t *testing.T) {
	t.Parallel()
	p := New(Config{Nodes: replicas.NodeConfig{EntryURL: "https://cluster.example", ClusterHost: "cluster.example", InitialNodes: []string{"https://elsewhere.example"}}})

	_, err := p.ServiceRPC(t.Context(), "git-upload-pack", strings.NewReader("0000"))
	if !errors.Is(err, ErrUnreachable) {
		t.Fatalf("want ErrUnreachable, got %v", err)
	}
}

func TestInfoRefs_CredentialFailureIsNotUnreachable(t *testing.T) {
	t.Parallel()
	credErr := errors.New("cannot reach the login server")
	p := New(Config{
		Nodes:   replicas.NodeConfig{EntryURL: "https://cluster.example", ClusterHost: "cluster.example"},
		SetAuth: func(*http.Request) error { return credErr },
	})

	_, err := p.InfoRefs(t.Context(), "git-upload-pack")
	if !errors.Is(err, credErr) {
		t.Fatalf("want the credential error, got %v", err)
	}
	if errors.Is(err, ErrUnreachable) {
		t.Errorf("a credential failure must not read as a connectivity failure: %v", err)
	}
}
