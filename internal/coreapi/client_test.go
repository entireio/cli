package coreapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ogen-go/ogen/ogenerrors"
)

func TestAPIError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
		want string
	}{
		{
			name: "nil error",
			err:  nil,
			want: "",
		},
		{
			name: "non-API error returns empty",
			err:  errors.New("dial tcp: connection refused"),
			want: "",
		},
		{
			name: "prefers detail",
			err: &ErrorModelStatusCode{
				StatusCode: 409,
				Response: ErrorModel{
					Title:  NewOptString("Conflict"),
					Detail: NewOptString("organization name already taken"),
				},
			},
			want: "organization name already taken",
		},
		{
			name: "falls back to title when detail empty",
			err: &ErrorModelStatusCode{
				StatusCode: 403,
				Response:   ErrorModel{Title: NewOptString("Forbidden")},
			},
			want: "Forbidden",
		},
		{
			name: "falls back to status when title and detail empty",
			err:  &ErrorModelStatusCode{StatusCode: 500},
			want: "control-plane request failed with status 500",
		},
		{
			name: "unwraps a wrapped API error",
			err: fmt.Errorf("create org: %w", &ErrorModelStatusCode{
				StatusCode: 422,
				Response:   ErrorModel{Detail: NewOptString("name is required")},
			}),
			want: "name is required",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := APIError(tc.err); got != tc.want {
				t.Errorf("APIError() = %q, want %q", got, tc.want)
			}
		})
	}
}

// bearerOnlySource mirrors the CLI's bearerSource contract: a fixed
// bearer token, and ErrSkipClientSecurity for sessionAuth so the
// generated middleware does NOT add a `Cookie: entire_session=` header.
// Used by TestBearerOnlySource_NoCookieOnTheWire to nail down the
// "bearer-only, no cookie" contract at the HTTP layer.
type bearerOnlySource struct{}

func (bearerOnlySource) BearerAuth(context.Context, OperationName) (BearerAuth, error) {
	return BearerAuth{Token: "test-bearer"}, nil
}

func (bearerOnlySource) SessionAuth(context.Context, OperationName) (SessionAuth, error) {
	return SessionAuth{}, ogenerrors.ErrSkipClientSecurity
}

// TestBearerOnlySource_NoCookieOnTheWire documents the SessionAuth
// empty-value contract by checking the wire: any operation issued by a
// Client built with a SessionAuth-skipping source must NOT carry a
// Cookie header. (ogen's securitySessionAuth unconditionally calls
// req.AddCookie, so returning SessionAuth{} with a nil error would send
// an empty `entire_session=` cookie; only ErrSkipClientSecurity prevents
// the cookie from being added.)
func TestBearerOnlySource_NoCookieOnTheWire(t *testing.T) {
	t.Parallel()

	// The handler runs on httptest's goroutine and the assertion runs
	// on the test goroutine; HTTP completion isn't a happens-before
	// edge the race detector recognises. Pass the captured header
	// across through a buffered channel so -race stays happy.
	cookieCh := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookieCh <- r.Header.Get("Cookie")
		w.Header().Set("Content-Type", "application/json")
		// Minimal valid ListOrgMembersOutputBody payload so the response
		// decoder doesn't blow up; we only care about the inbound headers.
		w.WriteHeader(http.StatusOK)
		if _, err := w.Write([]byte(`{"members":[]}`)); err != nil {
			t.Errorf("writing test response: %v", err)
		}
	}))
	t.Cleanup(srv.Close)

	c, err := NewClient(srv.URL, bearerOnlySource{})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	// ListOrgMembers is a simple GET that exercises the security
	// middleware; the result itself is irrelevant to this test.
	if _, err := c.ListOrgMembers(context.Background(), ListOrgMembersParams{OrgId: "01H000000000000000000000A1"}); err != nil {
		t.Fatalf("ListOrgMembers: %v", err)
	}

	cookieHeader := <-cookieCh
	if cookieHeader != "" {
		t.Errorf("outbound Cookie header = %q, want empty (bearer-only contract)", cookieHeader)
	}
}
