package httpdebug

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"

	"github.com/entireio/cli/internal/remotehelper/debuglog"
)

// RoundTripper wraps another http.RoundTripper and logs each
// request/response when debuglog.Enabled returns true. Bodies and
// sensitive headers are redacted before logging.
//
// When debugging is off, the wrapper is a thin pass-through: no
// allocations, no body buffering.
type RoundTripper struct {
	Next http.RoundTripper
}

// RoundTrip implements http.RoundTripper. Response bodies are read in
// full (so the preview can render correctly) and re-wrapped before
// returning — the caller sees an io.ReadCloser as before. This is a
// debug-only cost; non-debug mode skips the read entirely.
func (d *RoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	if !debuglog.Enabled() {
		//nolint:wrapcheck // passthrough - wrapping would change error semantics
		return d.Next.RoundTrip(req)
	}

	reqForDump := req.Clone(req.Context())
	reqForDump.Header = RedactHeaders(req.Header)
	reqDump, err := httputil.DumpRequestOut(reqForDump, false)
	if err != nil {
		debuglog.Printf("failed to dump request: %v", err)
	} else {
		debuglog.Printf(">>> REQUEST >>>\n%s", RedactURLCredentials(reqDump))
	}

	resp, err := d.Next.RoundTrip(req)
	if err != nil {
		debuglog.Printf("<<< ERROR <<< %v", err)
		//nolint:wrapcheck // passthrough
		return nil, err
	}

	respForDump := *resp
	respForDump.Header = RedactHeaders(resp.Header)
	respDump, err := httputil.DumpResponse(&respForDump, false)
	if err != nil {
		debuglog.Printf("failed to dump response: %v", err)
	} else {
		debuglog.Printf("<<< RESPONSE <<<\n%s", RedactURLCredentials(respDump))
	}

	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		debuglog.Printf("failed to read response body: %v", err)
		return nil, fmt.Errorf("reading response body: %w", err)
	}

	preview := BodyPreview(body)
	debuglog.Printf("<<< BODY (%d bytes, showing %d) >>>\n%s", len(body), len(preview), preview)

	resp.Body = io.NopCloser(bytes.NewReader(body))
	return resp, nil
}
