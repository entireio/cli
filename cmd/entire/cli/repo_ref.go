package cli

import (
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/cmd/entire/cli/gitremote"
)

// entireWebHost is the host of the production web origin, read once from the
// api.DefaultWebBaseURL constant. canonicalizeRepoRef strips it from pasted
// URLs; no environment is consulted, so an override never widens the match.
var entireWebHost = func() string {
	u, err := url.Parse(api.DefaultWebBaseURL)
	if err != nil {
		return ""
	}
	return u.Hostname()
}()

// hasPrefixASCIIFold reports whether s starts with prefix, comparing bytes with
// ASCII-only case folding. strings.EqualFold is deliberately avoided: it folds
// non-ASCII look-alikes such as "ſ" (U+017F) to "s".
func hasPrefixASCIIFold(s, prefix string) bool {
	if prefix == "" || len(s) < len(prefix) {
		return false
	}
	for i := range len(prefix) {
		a, b := s[i], prefix[i]
		if 'A' <= a && a <= 'Z' {
			a += 'a' - 'A'
		}
		if 'A' <= b && b <= 'Z' {
			b += 'a' - 'A'
		}
		if a != b {
			return false
		}
	}
	return true
}

// canonicalizeRepoRef turns a pasted entire.com URL
// ("https://entire.com/et/a/b", "entire.com/gh/o/r") into the path form the
// ref grammars understand ("/et/a/b"). It only strips: the scheme (https
// only), the production web host and at most one trailing slash. Anything that
// is not exactly that shape, including a result that would be empty or start
// with "//", is returned trimmed but otherwise unchanged. Pure; no env, no I/O.
func canonicalizeRepoRef(ref string) string {
	s := strings.TrimSpace(ref)
	rest := s
	if hasPrefixASCIIFold(rest, "https://") {
		rest = rest[len("https://"):]
	}
	if !hasPrefixASCIIFold(rest, entireWebHost) {
		return s
	}
	rest = rest[len(entireWebHost):]
	if !strings.HasPrefix(rest, "/") {
		return s
	}
	rest = strings.TrimSuffix(rest, "/")
	if rest == "" || rest == "/" || strings.HasPrefix(rest, "//") {
		return s
	}
	return rest
}

// refURLScheme matches a ref that opens with a URL scheme.
var refURLScheme = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9+.-]*://`)

// redactRefForError hides credentials in a typed ref before it is echoed in an
// error. Only a ref that opens with a URL scheme ("https://u:p@host/...") is
// scrubbed: "://" elsewhere ("/et/a/b/https://x") is an ordinary character
// run, and a scheme-less "u:secret@entire.com/..." is not scrubbed at all (it
// is not a ref any grammar accepts, and the echo stays what was typed).
//
// The scrub is textual (gitremote.RedactCredentialsInText), not a url.Parse
// round-trip, so the echo keeps exactly what was typed apart from userinfo:
// query, fragment, scheme case and percent-escapes survive.
func redactRefForError(ref string) string {
	if !refURLScheme.MatchString(ref) {
		return ref
	}
	return gitremote.RedactCredentialsInText(ref)
}

// typedRef pairs what the user typed (redacted, for error text) with the
// canonical form used for parsing.
type typedRef struct{ typed, canon string }

// newTypedRef is the single point where a ref typed by the user becomes a
// typedRef: it keeps the redacted text for error echoes and runs
// canonicalizeRepoRef for parsing. Command code takes its ref through here (the
// canonicalize guard test and the cli-conventions doc name it as such) so every
// accepted spelling, path or entire.com URL, reaches the parsers in one form.
func newTypedRef(s string) typedRef {
	return typedRef{
		typed: redactRefForError(strings.TrimSpace(s)),
		canon: canonicalizeRepoRef(s),
	}
}

// typedRefError is the error wrapErr returns: its message echoes the typed ref
// while Unwrap keeps the original error, so errors.Is/As still match.
type typedRefError struct {
	msg string
	err error
}

func (e *typedRefError) Error() string { return e.msg }
func (e *typedRefError) Unwrap() error { return e.err }

// wrapErr rewrites the quoted canonical ref in err's message back to what the
// user typed. errors.Is/As keep working through Unwrap.
//
// It rewrites only %q echoes of the exact canonical ref. A message that echoes
// the ref with %s, or a derived fragment of it, keeps the canonical spelling:
// callers that print the ref unquoted must pass redactRefForError(tr.canon)
// themselves.
func (t typedRef) wrapErr(err error) error {
	if err == nil || t.typed == t.canon {
		return err
	}
	msg := strings.ReplaceAll(err.Error(), strconv.Quote(t.canon), strconv.Quote(t.typed))
	return &typedRefError{msg: msg, err: err}
}
