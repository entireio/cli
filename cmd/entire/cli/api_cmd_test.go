package cli

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestInferFieldValue(t *testing.T) {
	t.Parallel()
	cases := map[string]any{
		"true":  true,
		"false": false,
		"null":  nil,
		"42":    int64(42),
		"-7":    int64(-7),
		"3.14":  3.14,
		"demo":  "demo",
		"":      "",
		"v1.2":  "v1.2",
	}
	for in, want := range cases {
		if got := inferFieldValue(in); got != want {
			t.Errorf("inferFieldValue(%q) = %#v, want %#v", in, got, want)
		}
	}
}

func TestBuildAPIFields(t *testing.T) {
	t.Parallel()

	got, err := buildAPIFields([]string{"name=demo"}, []string{"count=3", "enabled=true"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got["name"] != "demo" || got["count"] != int64(3) || got["enabled"] != true {
		t.Fatalf("fields = %#v", got)
	}

	if f, err := buildAPIFields(nil, nil); len(f) != 0 || err != nil {
		t.Fatalf("no fields = (%v, %v), want (empty, nil)", f, err)
	}
	if _, err := buildAPIFields([]string{"bogus"}, nil); err == nil {
		t.Error("expected error for -f without '='")
	}
	if _, err := buildAPIFields(nil, []string{"=novalue"}); err == nil {
		t.Error("expected error for -F with empty key")
	}
}

func TestParseAPIHeaders(t *testing.T) {
	t.Parallel()

	h, err := parseAPIHeaders([]string{"Accept: application/json", "X-Foo:bar"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if h.Get("Accept") != "application/json" || h.Get("X-Foo") != "bar" {
		t.Fatalf("headers = %v", h)
	}
	if _, err := parseAPIHeaders([]string{"nocolon"}); err == nil {
		t.Error("expected error for header without ':'")
	}
}

func TestBuildAPIRequestBody_MethodInference(t *testing.T) {
	t.Parallel()

	// No fields, no method → GET, no body.
	r, err := buildAPIRequestBody("/p", &apiFlags{}, nil)
	if err != nil || r.method != http.MethodGet || r.body != nil {
		t.Fatalf("bare = %+v, err %v", r, err)
	}

	// Fields, no method → POST with JSON body.
	r, err = buildAPIRequestBody("/p", &apiFlags{}, map[string]any{"a": "b"})
	if err != nil || r.method != http.MethodPost || r.body == nil {
		t.Fatalf("fields = %+v, err %v", r, err)
	}

	// Explicit -X wins over inference.
	r, err = buildAPIRequestBody("/p", &apiFlags{method: "delete"}, nil)
	if err != nil || r.method != http.MethodDelete {
		t.Fatalf("explicit method = %+v, err %v", r, err)
	}

	// GET + fields → fields go on the query string, no body.
	r, err = buildAPIRequestBody("/p", &apiFlags{method: "GET"}, map[string]any{"limit": int64(5)})
	if err != nil || r.body != nil || !strings.Contains(r.path, "limit=5") {
		t.Fatalf("GET+fields = %+v, err %v", r, err)
	}

	// --input together with fields is rejected.
	if _, err := buildAPIRequestBody("/p", &apiFlags{input: "x"}, map[string]any{"a": "b"}); err == nil {
		t.Error("expected error combining --input and fields")
	}
}

func TestAppendQuery(t *testing.T) {
	t.Parallel()

	if got := appendQuery("/p", nil); got != "/p" {
		t.Errorf("empty query = %q", got)
	}
	q, err := fieldsToQuery(map[string]any{"a": "b"})
	if err != nil {
		t.Fatalf("fieldsToQuery: %v", err)
	}
	if got := appendQuery("/p", q); got != "/p?a=b" {
		t.Errorf("fresh query = %q, want /p?a=b", got)
	}
	if got := appendQuery("/p?x=1", q); got != "/p?x=1&a=b" {
		t.Errorf("existing query = %q, want /p?x=1&a=b", got)
	}
}

func TestValidateAPIPath(t *testing.T) {
	t.Parallel()

	// Origin-relative paths are allowed.
	for _, ok := range []string{"/api/v1/clusters", "/api/v1/me/recap?repo=01K", "api/v1/x", "/"} {
		if err := validateAPIPath(ok); err != nil {
			t.Errorf("validateAPIPath(%q) = %v, want nil", ok, err)
		}
	}
	// Absolute and scheme-relative URLs must be refused — they'd redirect the
	// bearer token to another host via url.ResolveReference.
	for _, bad := range []string{
		"https://evil.example/api/v1/x",
		"http://evil.example/x",
		"//evil.example/x",
		"https:/evil",
	} {
		if err := validateAPIPath(bad); err == nil {
			t.Errorf("validateAPIPath(%q) = nil, want rejection (token-leak vector)", bad)
		}
	}
}

func TestResolveAPIClient_UnknownTarget(t *testing.T) {
	t.Parallel()
	if _, err := resolveAPIClient(context.Background(), "banana", false); err == nil {
		t.Error("expected error for unknown --to")
	}
}

func TestWriteAPIResponse(t *testing.T) {
	t.Parallel()

	newResp := func(status int, body string) *http.Response {
		return &http.Response{
			StatusCode: status,
			Proto:      "HTTP/2.0",
			Status:     http.StatusText(status),
			Header:     http.Header{"Content-Type": {"application/json"}},
			Body:       io.NopCloser(strings.NewReader(body)),
		}
	}

	write := func(status int, body string, include bool) (out, errOut bytes.Buffer, err error) {
		resp := newResp(status, body)
		defer func() { _ = resp.Body.Close() }()
		err = writeAPIResponse(&out, &errOut, resp, include)
		return out, errOut, err
	}

	// 2xx JSON is pretty-printed (indentation added) and returns no error.
	out, _, err := write(200, `{"a":1}`, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out.String(), "\n  \"a\": 1") {
		t.Fatalf("expected indented JSON, got:\n%s", out.String())
	}

	// Non-2xx still prints the body but returns a (silent) error for a non-zero exit.
	out, _, err = write(404, `{"error":"nope"}`, false)
	if err == nil {
		t.Fatal("expected error on 404")
	}
	if !strings.Contains(out.String(), "nope") {
		t.Fatalf("expected body printed on 404, got:\n%s", out.String())
	}

	// --include writes the status line + headers to errOut.
	_, errOut, err := write(200, `{}`, true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(errOut.String(), "HTTP/2.0") || !strings.Contains(errOut.String(), "Content-Type: application/json") {
		t.Fatalf("expected status+headers on errOut, got:\n%s", errOut.String())
	}
}

func TestReadWithinLimit(t *testing.T) {
	t.Parallel()

	// Under and exactly at the limit: full content, no error.
	for _, tc := range []struct {
		in    string
		limit int64
	}{{"hello", 10}, {"hello", 5}, {"", 3}} {
		got, err := readWithinLimit(strings.NewReader(tc.in), tc.limit)
		if err != nil || string(got) != tc.in {
			t.Errorf("readWithinLimit(%q, %d) = (%q, %v), want (%q, nil)", tc.in, tc.limit, got, err, tc.in)
		}
	}
	// Over the limit: error rather than silent truncation.
	if _, err := readWithinLimit(strings.NewReader("hello world"), 5); err == nil {
		t.Error("readWithinLimit over limit = nil error, want limit error (must not truncate)")
	}
}
