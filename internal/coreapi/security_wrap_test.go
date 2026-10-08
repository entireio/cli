package coreapi

import (
	"errors"
	"fmt"
	"testing"
)

// The reported shape: an unreachable login server, wrapped by the generated
// client's two security segments, under the command's own context.
func TestStripSecurityWrapping_RealChain(t *testing.T) {
	t.Parallel()

	cause := errors.New("cannot reach the login server for \"local\" (http://localhost:8180): connect: connection refused")
	err := fmt.Errorf("resolve mirror placements: %w",
		fmt.Errorf("security %q: %w", "BearerAuth",
			fmt.Errorf("security source %q: %w", "BearerAuth", cause)))

	stripped := StripSecurityWrapping(err)
	want := "resolve mirror placements: " + cause.Error()
	if got := stripped.Error(); got != want {
		t.Errorf("stripped message:\n got %q\nwant %q", got, want)
	}
	// The original chain must survive for errors.Is/As callers.
	if !errors.Is(stripped, cause) {
		t.Error("stripped error no longer unwraps to the cause")
	}
}

func TestStripSecurityWrapping(t *testing.T) {
	t.Parallel()

	sentinel := errors.New("boom")

	tests := []struct {
		name string
		err  error
		want string
	}{
		{
			name: "nothing to strip is returned unchanged",
			err:  fmt.Errorf("list mirrors: %w", sentinel),
			want: "list mirrors: boom",
		},
		{
			name: "session scheme is stripped too",
			err:  fmt.Errorf("security %q: %w", "SessionAuth", sentinel),
			want: "boom",
		},
		{
			name: "the security source form is stripped too",
			err:  fmt.Errorf("security source %q: %w", "BearerAuth", sentinel),
			want: "boom",
		},
		{
			// Only a wrapper *prefix* is dropped. The same words inside a
			// message (an API problem detail, say) are the message, not
			// scaffolding, so they stay.
			name: "matching text inside a message is preserved",
			err:  fmt.Errorf("create org: %w", errors.New(`security "BearerAuth": rejected by policy`)),
			want: `create org: security "BearerAuth": rejected by policy`,
		},
		{
			// A message rewritten rather than prefixed (the per-context
			// re-login errors) stops the walk: outer segments already peeled
			// are still dropped, the rest renders verbatim.
			name: "walk stops at a rewritten message",
			err: fmt.Errorf("security %q: %w", "BearerAuth",
				&rewrittenError{msg: "login session expired; run `entire login`", err: sentinel}),
			want: "login session expired; run `entire login`",
		},
		{
			name: "empty scheme name still matches",
			err:  fmt.Errorf("security %q: %w", "", sentinel),
			want: "boom",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := StripSecurityWrapping(tt.err).Error(); got != tt.want {
				t.Errorf("stripped message:\n got %q\nwant %q", got, tt.want)
			}
		})
	}
}

func TestStripSecurityWrapping_Nil(t *testing.T) {
	t.Parallel()
	if err := StripSecurityWrapping(nil); err != nil {
		t.Errorf("StripSecurityWrapping(nil) = %v, want nil", err)
	}
}

// An error whose message replaces its cause's rather than prefixing it — the
// shape auth's per-context re-login errors use.
type rewrittenError struct {
	msg string
	err error
}

func (e *rewrittenError) Error() string { return e.msg }
func (e *rewrittenError) Unwrap() error { return e.err }
