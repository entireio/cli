package transport

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHTTPErrorMessage_ReturnsStatusError(t *testing.T) {
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
			err := HTTPErrorMessage(tt.statusCode, tt.serverMsg, "https://n/gh/o/r")
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
