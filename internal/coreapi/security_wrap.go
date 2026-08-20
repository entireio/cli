package coreapi

import (
	"errors"
	"regexp"
	"strings"
)

// securityWrapPrefix matches the prefixes ogen's generated client adds to any
// error a SecuritySource returns: `security "BearerAuth"` around the operation's
// security resolution (oas_client_gen.go) and `security source "BearerAuth"`
// around the source call itself (oas_security_gen.go). Both are generated, so
// neither can be reworded at the source.
var securityWrapPrefix = regexp.MustCompile(`^security (source )?"[^"]*"$`)

// strippedError renders a message with the generated wrappers removed while
// still unwrapping to the original chain, so errors.Is/As keep matching
// whatever the caller was matching before.
type strippedError struct {
	msg string
	err error
}

func (e *strippedError) Error() string { return e.msg }
func (e *strippedError) Unwrap() error { return e.err }

// StripSecurityWrapping returns err with ogen's generated security wrappers
// removed from the rendered message, and err unchanged when it carries none.
//
// It exists because those wrappers turn an actionable auth failure into
// something a user has to read past: an unreachable login server surfaces as
//
//	resolve mirror placements: security "BearerAuth": security source "BearerAuth": cannot reach the login server for …
//
// where the two middle segments say nothing a user can act on — they name the
// OpenAPI security scheme that failed to resolve, which is an implementation
// detail of how the client asks for a token.
//
// The segments are peeled structurally rather than by search-and-replace: each
// link in the chain is decomposed into the prefix its wrapper added plus the
// message it wrapped, and only a prefix that is itself a generated wrapper is
// dropped. Identical text appearing inside a message (an API error detail, say)
// is left alone, because it is not a wrapper prefix. Walking stops at the first
// link whose message doesn't contain its cause verbatim (a message that was
// rewritten rather than wrapped, e.g. the per-context re-login errors) — there
// is nothing further to decompose past that point.
func StripSecurityWrapping(err error) error {
	if err == nil {
		return nil
	}
	var kept []string
	stripped := false
	cur := err
	for {
		inner := errors.Unwrap(cur)
		if inner == nil {
			break
		}
		// Only a `<segment>: <cause>` wrapper can be recomposed by joining on
		// ": ". Anything else (an interpolated %w, a bare "%w", a message
		// rewritten rather than prefixed) isn't decomposable, so stop and render
		// the rest verbatim.
		segment, ok := strings.CutSuffix(cur.Error(), ": "+inner.Error())
		if !ok {
			break
		}
		if securityWrapPrefix.MatchString(segment) {
			stripped = true
		} else {
			kept = append(kept, segment)
		}
		cur = inner
	}
	if !stripped {
		return err
	}
	return &strippedError{msg: strings.Join(append(kept, cur.Error()), ": "), err: err}
}
