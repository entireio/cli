package httpdebug

import (
	"bytes"
	"net/http"
	"strings"
	"testing"
)

func TestRedactHeaders(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   http.Header
		want http.Header
	}{
		{name: "nil header", in: nil, want: http.Header{}},
		{
			name: "no sensitive headers",
			in: http.Header{
				"Content-Type": []string{"application/json"},
				"User-Agent":   []string{"git-remote-entire/test"},
			},
			want: http.Header{
				"Content-Type": []string{"application/json"},
				"User-Agent":   []string{"git-remote-entire/test"},
			},
		},
		{
			name: "Authorization redacted",
			in: http.Header{
				"Authorization": []string{"Bearer eyJhbGciOi.secret.payload"},
				"Content-Type":  []string{"application/json"},
			},
			want: http.Header{
				"Authorization": []string{Placeholder},
				"Content-Type":  []string{"application/json"},
			},
		},
		{
			name: "Proxy-Authorization redacted",
			in:   http.Header{"Proxy-Authorization": []string{"Bearer leaky-token"}},
			want: http.Header{"Proxy-Authorization": []string{Placeholder}},
		},
		{
			name: "Cookie left alone (out of scope)",
			in:   http.Header{"Cookie": []string{"session=abc123"}},
			want: http.Header{"Cookie": []string{"session=abc123"}},
		},
		{
			name: "case-insensitive match via canonical key",
			in:   http.Header{"authorization": []string{"Bearer leaky"}},
			want: http.Header{"Authorization": []string{Placeholder}},
		},
		{
			name: "multi-value Authorization redacted per value",
			in:   http.Header{"Authorization": []string{"Bearer first", "Bearer second"}},
			want: http.Header{"Authorization": []string{Placeholder, Placeholder}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := RedactHeaders(tt.in)
			if len(got) != len(tt.want) {
				t.Fatalf("len = %d, want %d (got=%v want=%v)", len(got), len(tt.want), got, tt.want)
			}
			for k, wantVals := range tt.want {
				gotVals := got.Values(k)
				if len(gotVals) != len(wantVals) {
					t.Errorf("%q: len = %d, want %d", k, len(gotVals), len(wantVals))
					continue
				}
				for i := range wantVals {
					if gotVals[i] != wantVals[i] {
						t.Errorf("%q[%d] = %q, want %q", k, i, gotVals[i], wantVals[i])
					}
				}
			}
		})
	}
}

func TestRedactHeadersDoesNotMutateInput(t *testing.T) {
	t.Parallel()

	in := http.Header{
		"Authorization": []string{"Bearer real-token"},
		"Content-Type":  []string{"application/json"},
	}

	_ = RedactHeaders(in)

	if got := in.Get("Authorization"); got != "Bearer real-token" {
		t.Errorf("input mutated: Authorization = %q, want unchanged", got)
	}
}

func TestRedactURLCredentials(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		in         string
		mustNotHas []string
		mustHas    []string
	}{
		{
			name:       "Location header userinfo",
			in:         "Location: https://x-token:secret@host.example/path/repo\r\n",
			mustNotHas: []string{"secret", "x-token"},
			mustHas:    []string{"https://" + Placeholder + "@host.example/path/repo"},
		},
		{
			name:       "HTML anchor userinfo",
			in:         `<a href="https://x-token:abc123@host.example/path">redirect</a>`,
			mustNotHas: []string{"abc123", "x-token"},
			mustHas:    []string{"https://" + Placeholder + "@host.example/path"},
		},
		{
			name:       "username only (no password)",
			in:         "Location: http://user@host.example/path",
			mustNotHas: []string{"user@host"},
			mustHas:    []string{"http://" + Placeholder + "@host.example/path"},
		},
		{
			name:       "URL with no userinfo unchanged",
			in:         "Location: https://host.example/path?u=alice@example.com",
			mustNotHas: []string{Placeholder},
			mustHas:    []string{"https://host.example/path?u=alice@example.com"},
		},
		{
			name:       "plain text with @ but no scheme unchanged",
			in:         "contact alice@example.com for access",
			mustNotHas: []string{Placeholder},
			mustHas:    []string{"alice@example.com"},
		},
		{
			name:       "multiple URLs on same line",
			in:         "https://u1:p1@a.example/x https://u2:p2@b.example/y",
			mustNotHas: []string{"u1:p1", "u2:p2"},
			mustHas:    []string{"https://" + Placeholder + "@a.example/x", "https://" + Placeholder + "@b.example/y"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := string(RedactURLCredentials([]byte(tt.in)))
			for _, s := range tt.mustNotHas {
				if strings.Contains(got, s) {
					t.Errorf("output should not contain %q\ngot: %s", s, got)
				}
			}
			for _, s := range tt.mustHas {
				if !strings.Contains(got, s) {
					t.Errorf("output should contain %q\ngot: %s", s, got)
				}
			}
		})
	}
}

func TestBodyPreviewRedactsURLCredentials(t *testing.T) {
	t.Parallel()

	longPass := strings.Repeat("S", 600)
	body := []byte(`<a href="https://x-token:` + longPass + `@host.example/path">go</a>`)

	preview := BodyPreview(body)

	if strings.Contains(string(preview), longPass) {
		t.Fatalf("password leaked in preview:\n%s", preview)
	}
	if !strings.Contains(string(preview), "https://"+Placeholder+"@host.example/path") {
		t.Errorf("expected redacted URL in preview:\n%s", preview)
	}
}

func TestBodyPreviewTruncatesAfterRedaction(t *testing.T) {
	t.Parallel()

	// Long JWT whose closing quote falls past the preview boundary. If
	// we truncate first and redact second, the regex never sees the
	// closing quote and the token leaks. This test pins the
	// redact-then-truncate order.
	longJWT := strings.Repeat("A", 600)
	body := []byte(`{"access_token":"` + longJWT + `","token_type":"Bearer","expires_in":900}`)

	preview := BodyPreview(body)

	if strings.Contains(string(preview), longJWT) {
		t.Fatalf("token leaked in preview:\n%s", preview)
	}
	if !strings.Contains(string(preview), `"access_token":"`+Placeholder+`"`) {
		t.Errorf("expected redacted access_token in preview:\n%s", preview)
	}
}

func TestBodyPreviewBelowMaxUnchanged(t *testing.T) {
	t.Parallel()

	body := []byte(`{"name":"alice"}`)
	preview := BodyPreview(body)
	if string(preview) != `{"name":"alice"}` {
		t.Errorf("preview = %q, want unchanged", preview)
	}
}

func TestBodyPreviewCutsAtPackMagic(t *testing.T) {
	t.Parallel()

	// Smart-HTTP pack response: pktline header, then PACK signature,
	// then binary pack stream. Without the PACK cut, ~512 bytes of
	// binary garbage land in the debug log.
	binary := bytes.Repeat([]byte{0xff, 0x00, 0x42, 0x13}, 200)
	body := append([]byte("0008NAK\n0011PACK"), binary...)

	preview := BodyPreview(body)

	want := "0008NAK\n0011PACK [PACK_DATA]"
	if string(preview) != want {
		t.Errorf("preview = %q, want %q", preview, want)
	}
}

func TestRedactJSONTokens(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		in         string
		mustNotHas []string
		mustHas    []string
	}{
		{
			name:       "STS access_token response",
			in:         `{"access_token":"eyJhbGciOi.secret.payload","token_type":"Bearer","expires_in":900}`,
			mustNotHas: []string{"eyJhbGciOi.secret.payload"},
			mustHas:    []string{`"access_token":"` + Placeholder + `"`, `"token_type":"Bearer"`, `"expires_in":900`},
		},
		{
			name:       "id_token, refresh_token, subject_token all redacted",
			in:         `{"id_token":"id.leak","refresh_token":"refresh.leak","subject_token":"subject.leak"}`,
			mustNotHas: []string{"id.leak", "refresh.leak", "subject.leak"},
			mustHas:    []string{`"id_token":"` + Placeholder + `"`, `"refresh_token":"` + Placeholder + `"`, `"subject_token":"` + Placeholder + `"`},
		},
		{
			name:       "whitespace around colon",
			in:         `{"access_token" : "spaced.leak"}`,
			mustNotHas: []string{"spaced.leak"},
			mustHas:    []string{`"` + Placeholder + `"`},
		},
		{
			name:       "non-token JSON unchanged",
			in:         `{"name":"alice","role":"admin"}`,
			mustNotHas: []string{Placeholder},
			mustHas:    []string{`"name":"alice"`, `"role":"admin"`},
		},
		{
			name:       "nested token field",
			in:         `{"data":{"access_token":"nested.leak","other":"keep"}}`,
			mustNotHas: []string{"nested.leak"},
			mustHas:    []string{`"access_token":"` + Placeholder + `"`, `"other":"keep"`},
		},
		{
			name:       "non-JSON body unchanged",
			in:         "not json at all",
			mustNotHas: []string{Placeholder},
			mustHas:    []string{"not json at all"},
		},
		{
			name:       "lookalike key not redacted",
			in:         `{"my_token":"keep","token":"keep2"}`,
			mustNotHas: []string{Placeholder},
			mustHas:    []string{`"my_token":"keep"`, `"token":"keep2"`},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := string(RedactJSONTokens([]byte(tt.in)))
			for _, s := range tt.mustNotHas {
				if strings.Contains(got, s) {
					t.Errorf("output should not contain %q\ngot: %s", s, got)
				}
			}
			for _, s := range tt.mustHas {
				if !strings.Contains(got, s) {
					t.Errorf("output should contain %q\ngot: %s", s, got)
				}
			}
		})
	}
}
