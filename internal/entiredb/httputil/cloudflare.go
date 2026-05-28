// Package httputil contains small HTTP helpers shared across the CLI and
// server-side packages.
package httputil

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// CloudflareChallengeError detects when an HTTP response is an HTML challenge
// page (typically Cloudflare's WARP/Access gate sitting in front of our
// partial.to domains) rather than a JSON API response, and returns a
// user-facing error explaining the likely fix. Returns nil if the response
// looks like a normal API reply.
//
// Detection: Content-Type is text/html, or the body starts with `<` after
// trimming whitespace. Both heuristics are needed because Cloudflare
// challenge responses sometimes ship without a Content-Type header, and we
// don't want a stray JSON body that happens to start with `<` to trigger a
// false positive (it can't — JSON values never start with `<`).
//
// The returned error includes the request URL so the user knows which
// hostname to whitelist or connect to via WARP.
func CloudflareChallengeError(resp *http.Response, body []byte) error {
	if resp == nil {
		return nil
	}
	if !looksLikeHTML(resp, body) {
		return nil
	}
	host := ""
	if resp.Request != nil && resp.Request.URL != nil {
		host = resp.Request.URL.Host
	}
	if host != "" {
		return fmt.Errorf("got an HTML response from %s instead of JSON — check your Cloudflare WARP connection", host)
	}
	return errors.New("got an HTML response instead of JSON — check your Cloudflare WARP connection")
}

func looksLikeHTML(resp *http.Response, body []byte) bool {
	ct := resp.Header.Get("Content-Type")
	if ct != "" {
		mediaType, _, _ := strings.Cut(ct, ";")
		if strings.EqualFold(strings.TrimSpace(mediaType), "text/html") {
			return true
		}
	}
	trimmed := bytes.TrimLeft(body, " \t\r\n")
	return len(trimmed) > 0 && trimmed[0] == '<'
}
