package cli

import (
	"context"
	"strings"
	"testing"
)

// authorizeURLWithSeparators is shaped like a real authorization URL:
// url.Values sorts its keys, so client_id comes first and redirect_uri sits
// behind three `&` separators, and the redirect_uri itself is percent-encoded.
// Shared with the Windows launcher test.
const authorizeURLWithSeparators = "https://us.auth.entire.io/authorize?" +
	"client_id=entire-cli&code_challenge=Zt1c&code_challenge_method=S256&" +
	"redirect_uri=http%3A%2F%2F127.0.0.1%3A54123%2Fcallback&response_type=code&" +
	"scope=cli+offline_access&state=Yg8q"

// TestOpenBrowser_ValidatesURLBeforeLaunching covers both halves of
// openBrowser's guard: non-HTTP URLs are refused outright, and http(s) URLs
// clear validation and reach the platform launcher — which is stubbed out under
// test, so the sentinel below is what a URL that passed validation looks like
// and no browser is spawned on a dev or CI host.
func TestOpenBrowser_ValidatesURLBeforeLaunching(t *testing.T) {
	t.Parallel()

	const (
		refused    = "refusing to open non-HTTP URL"
		reachedRun = "browser unavailable under test"
	)

	for _, tc := range []struct {
		url     string
		wantErr string
	}{
		{"file:///etc/passwd", refused},
		{"javascript:alert(1)", refused},
		{"ftp://example.test/x", refused},
		{"not a url at all", refused},
		{"", refused},
		{authorizeURLWithSeparators, reachedRun},
		{"http://127.0.0.1:8080/callback?code=abc&state=x", reachedRun},
	} {
		err := openBrowser(context.Background(), tc.url)
		if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
			t.Errorf("openBrowser(%q) error = %v, want %q", tc.url, err, tc.wantErr)
		}
	}
}
