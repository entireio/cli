package netcause

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"syscall"
	"testing"
)

// transportErr wraps cause the way http.Client and net.Dial do.
func transportErr(cause error) error {
	return &url.Error{Op: "Get", URL: "https://entire.io/api/v1/x", Err: &net.OpError{Op: "dial", Net: "tcp", Err: cause}}
}

func TestDeniedAndHint(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		err        error
		wantDenied bool
		wantHint   string
	}{
		{
			name:       "connect EPERM",
			err:        transportErr(os.NewSyscallError("connect", syscall.EPERM)),
			wantDenied: true,
			wantHint:   "not allowed to open network connections",
		},
		{
			name:       "connect EACCES",
			err:        transportErr(os.NewSyscallError("connect", syscall.EACCES)),
			wantDenied: true,
			wantHint:   "not allowed to open network connections",
		},
		{
			name:     "DNS lookup failure",
			err:      transportErr(&net.DNSError{Err: "no such host", Name: "entire.io", IsNotFound: true}),
			wantHint: "could not resolve entire.io",
		},
		{
			name: "connection refused is not named",
			err:  transportErr(os.NewSyscallError("connect", syscall.ECONNREFUSED)),
		},
		{
			name: "cancellation is not a network cause",
			err:  errors.Join(context.Canceled, os.NewSyscallError("connect", syscall.EPERM)),
		},
		{
			name: "non-network error",
			err:  errors.New("boom"),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := Denied(tt.err); got != tt.wantDenied {
				t.Errorf("Denied = %v, want %v", got, tt.wantDenied)
			}
			got := Hint(tt.err)
			if tt.wantHint == "" {
				if got != "" {
					t.Errorf("Hint = %q, want empty", got)
				}
				return
			}
			if !strings.Contains(got, tt.wantHint) {
				t.Errorf("Hint = %q, want it to contain %q", got, tt.wantHint)
			}
		})
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestTransport(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		cause    error
		wantHint string
	}{
		{name: "denied dial is explained", cause: os.NewSyscallError("connect", syscall.EPERM), wantHint: "not allowed to open network connections"},
		{name: "unnamed cause passes through", cause: os.NewSyscallError("connect", syscall.ECONNREFUSED)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			client := &http.Client{Transport: Transport(roundTripFunc(func(*http.Request) (*http.Response, error) {
				return nil, &net.OpError{Op: "dial", Net: "tcp", Err: tt.cause}
			}))}
			req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://entire.io/api/v1/x", nil)
			if err != nil {
				t.Fatal(err)
			}
			resp, err := client.Do(req)
			if err == nil {
				resp.Body.Close()
				t.Fatal("want an error")
			}
			if !errors.Is(err, tt.cause) {
				t.Errorf("error %v no longer unwraps to %v", err, tt.cause)
			}
			if got := err.Error(); tt.wantHint != "" && !strings.Contains(got, tt.wantHint) {
				t.Errorf("error %q missing %q", got, tt.wantHint)
			} else if tt.wantHint == "" && strings.Contains(got, ";") {
				t.Errorf("error %q gained a hint for an unnamed cause", got)
			}
			// A caller hinting the same error again must add nothing.
			if again := Hint(err); again != "" {
				t.Errorf("Hint on an already-hinted error = %q, want empty", again)
			}
		})
	}
}
