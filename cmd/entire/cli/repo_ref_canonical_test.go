package cli

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestCanonicalizeRepoRef(t *testing.T) {
	t.Parallel()
	ulid := "01HZX3K9Q2M4N5P6R7S8T9V0WX"
	cases := []struct{ name, in, want string }{
		{"https", "https://entire.com/et/a/b", "/et/a/b"},
		{"no scheme", "entire.com/gh/o/r", "/gh/o/r"},
		{"upper", "HTTPS://ENTIRE.COM/et/a/b", "/et/a/b"},
		{"trailing slash", "https://entire.com/et/a/b/", "/et/a/b"},
		{"git suffix slash", "https://entire.com/et/p/r.git/", "/et/p/r.git"},
		{"whitespace", "  https://entire.com/et/a/b\n", "/et/a/b"},
		{"path case kept", "ENTIRE.COM/ET/p/r", "/ET/p/r"},
		{"root", "https://entire.com/", "https://entire.com/"},
		{"root double", "https://entire.com//", "https://entire.com//"},
		{"host only", "entire.com", "entire.com"},
		{"http", "http://entire.com/et/a/b", "http://entire.com/et/a/b"},
		{"www", "https://www.entire.com/et/a/b", "https://www.entire.com/et/a/b"},
		{"suffix host", "https://entire.com.evil.io/et/a/b", "https://entire.com.evil.io/et/a/b"},
		{"dot host", "entire.com./et/a/b", "entire.com./et/a/b"},
		{"port", "https://entire.com:443/et/a/b", "https://entire.com:443/et/a/b"},
		{"userinfo", "https://user@entire.com/et/a/b", "https://user@entire.com/et/a/b"},
		{"userinfo host", "https://entire.com@evil.io/et/a/b", "https://entire.com@evil.io/et/a/b"},
		{"one slash", "https:/entire.com/et/a/b", "https:/entire.com/et/a/b"},
		{"scheme relative", "//entire.com/et/a/b", "//entire.com/et/a/b"},
		{"encoded slash", "https://entire.com%2fet/a/b", "https://entire.com%2fet/a/b"},
		{"long s scheme", "httpſ://entire.com/et/a/b", "httpſ://entire.com/et/a/b"},
		{"dotted I host", "ENTİRE.com/et/a/b", "ENTİRE.com/et/a/b"},
		{"fullwidth host", "ｅｎｔｉｒｅ.com/et/a/b", "ｅｎｔｉｒｅ.com/et/a/b"},
		{"entire scheme", "entire://h/et/a/b", "entire://h/et/a/b"},
		{"path ref", "/et/a/b", "/et/a/b"},
		{"relative", "et/a/b", "et/a/b"},
		{"ulid", ulid, ulid},
		{"bare name", "myrepo", "myrepo"},
		{"query stripped", "https://entire.com/et/a/b?x", "/et/a/b?x"},
		{"fragment stripped", "entire.com/et/a/b#x", "/et/a/b#x"},
		{"no forge", "entire.com/foo/bar", "/foo/bar"},
		{"short", "entire.com/et/x", "/et/x"},
		{"double slash", "https://entire.com//et/a/b", "https://entire.com//et/a/b"},
		{"backslashes", `https://entire.com\et\a\b`, `https://entire.com\et\a\b`},
		{"backslash after host", `entire.com/et\a\b`, `/et\a\b`},
		{"dot-dot segment", "entire.com/et/a/b/../c", "/et/a/b/../c"},
		{"extra trailing slash", "entire.com/et/a/b//", "/et/a/b/"},
		{"NUL in segment", "entire.com/et/a/b\x00c", "/et/a/b\x00c"},
		{"newline in segment", "entire.com/et/a/b\nc", "/et/a/b\nc"},
		{"tree suffix", "https://entire.com/et/a/b/tree/main", "/et/a/b/tree/main"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := canonicalizeRepoRef(tc.in)
			if got != tc.want {
				t.Errorf("canonicalizeRepoRef(%q) = %q, want %q", tc.in, got, tc.want)
			}
			if strings.HasPrefix(got, "//") && !strings.HasPrefix(tc.in, "//") {
				t.Errorf("output %q starts with //", got)
			}
			if isEntireCloneURL(got) && !isEntireCloneURL(tc.in) {
				t.Errorf("output %q became an entire:// URL", got)
			}
		})
	}

	t.Run("embedded entire scheme", func(t *testing.T) {
		t.Parallel()
		got := canonicalizeRepoRef("https://entire.com/entire://h/et/a/b")
		if isEntireCloneURL(got) {
			t.Errorf("got %q satisfies isEntireCloneURL", got)
		}
	})
}

func TestRedactRefForError(t *testing.T) {
	t.Parallel()
	if got := redactRefForError("https://u:secret@entire.com/et/a/b"); strings.Contains(got, "secret") {
		t.Errorf("secret leaked: %q", got)
	}
	if got := redactRefForError("/et/a/b"); got != "/et/a/b" {
		t.Errorf("path ref changed: %q", got)
	}
	// Refs that are not URLs (no leading scheme) and URLs url.Parse refuses
	// echo verbatim: a mangled echo hides what the user typed.
	for _, ref := range []string{
		"/et/abc/b/https://x",
		"entire.com/entire://h/et/a/b",
		"entire://ho st/et/a/b",
		"https://%zz://bad",
		// Query, fragment, scheme case and percent-escapes stay as typed.
		"https://entire.com/et/acme/web?tab=x",
		"https://entire.com/gh/o/r#readme",
		"https://github.com/o/r?tab=readme",
		"https://github.com/o/%2E",
		"HTTPS://Entire.com/et/a/b",
	} {
		if got := redactRefForError(ref); got != ref {
			t.Errorf("redactRefForError(%q) = %q, want the ref verbatim", ref, got)
		}
	}
	if got := redactRefForError("https://u:secret@entire.com/et/a/b?x=1#y"); strings.Contains(got, "secret") || !strings.HasSuffix(got, "/et/a/b?x=1#y") {
		t.Errorf("want credentials gone and query/fragment kept, got %q", got)
	}
	if got := redactRefForError("entire://u:secret@ho st/et/a/b"); strings.Contains(got, "secret") {
		t.Errorf("secret leaked from an unparseable URL: %q", got)
	}
}

var errSentinelRef = errors.New("sentinel")

func TestTypedRefWrapErr(t *testing.T) {
	t.Parallel()
	t.Run("rewrites quoted canon", func(t *testing.T) {
		t.Parallel()
		tr := newTypedRef("https://entire.com/foo/bar")
		err := fmt.Errorf("bad ref %q: %w", tr.canon, errSentinelRef)
		got := tr.wrapErr(err)
		if want := `bad ref "https://entire.com/foo/bar": sentinel`; got.Error() != want {
			t.Errorf("got %q, want %q", got.Error(), want)
		}
		if !errors.Is(got, errSentinelRef) {
			t.Error("errors.Is lost through wrapper")
		}
	})
	t.Run("redaction alone still scrubs the error", func(t *testing.T) {
		t.Parallel()
		tr := newTypedRef("https://u:secret@entire.com/et/a/b")
		if tr.typed == tr.canon {
			t.Fatalf("typed == canon (%q); credentialed ref should differ", tr.typed)
		}
		err := fmt.Errorf("invalid <repo> %q: %w", tr.canon, errSentinelRef)
		got := tr.wrapErr(err)
		if strings.Contains(got.Error(), "secret") {
			t.Errorf("secret leaked: %q", got.Error())
		}
		if !errors.Is(got, errSentinelRef) {
			t.Error("errors.Is lost through wrapper")
		}
	})
	t.Run("nil", func(t *testing.T) {
		t.Parallel()
		if got := newTypedRef("https://entire.com/et/a/b").wrapErr(nil); got != nil {
			t.Errorf("got %v", got)
		}
	})
	t.Run("identical returns same error", func(t *testing.T) {
		t.Parallel()
		tr := newTypedRef("/et/a/b")
		if got := tr.wrapErr(errSentinelRef); got != errSentinelRef { //nolint:errorlint // identity is the assertion
			t.Errorf("got %v", got)
		}
	})
}
