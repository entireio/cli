package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/entireio/cli/internal/entiredb/httputil"
)

// defaultRetryBudget caps the total wall-clock time the client will
// spend honoring server Retry-After hints on a single call. Sized to
// cover the regional reconciler's worst-case window (30s grace + 30s
// loop).
const defaultRetryBudget = 60 * time.Second

// Client is a simple HTTP client for the entire-core API.
type Client struct {
	BaseURL string
	Token   string
	// RetryBudget overrides defaultRetryBudget; zero uses the default.
	// Exposed so tests can shrink the wait without touching package state.
	RetryBudget time.Duration
}

func (c *Client) retryBudget() time.Duration {
	if c.RetryBudget > 0 {
		return c.RetryBudget
	}
	return defaultRetryBudget
}

func (c *Client) do(method, path string, body any) ([]byte, error) {
	var bodyBytes []byte
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("marshal request: %w", err)
		}
		bodyBytes = b
	}

	deadline := time.Now().Add(c.retryBudget())
	for {
		var bodyReader io.Reader
		if bodyBytes != nil {
			bodyReader = bytes.NewReader(bodyBytes)
		}

		req, err := http.NewRequestWithContext(context.Background(), method, c.BaseURL+path, bodyReader)
		if err != nil {
			return nil, fmt.Errorf("create request: %w", err)
		}
		if c.Token != "" {
			req.Header.Set("Authorization", "Bearer "+c.Token)
		}
		if bodyBytes != nil {
			req.Header.Set("Content-Type", "application/json")
		}

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return nil, fmt.Errorf("request failed: %w", err)
		}
		data, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("read response: %w", err)
		}

		if cfErr := httputil.CloudflareChallengeError(resp, data); cfErr != nil {
			return nil, cfErr
		}

		if resp.StatusCode >= 400 {
			if wait, ok := parseRetryAfter(resp.Header.Get("Retry-After")); ok {
				remaining := time.Until(deadline)
				if remaining > 0 {
					if wait > remaining {
						wait = remaining
					}
					slog.Default().Info("retrying request after server Retry-After",
						slog.String("method", method),
						slog.String("path", path),
						slog.Int("status", resp.StatusCode),
						slog.Duration("wait", wait))
					time.Sleep(wait)
					continue
				}
			}
			return nil, errFromHTTPResponse(resp.StatusCode, data)
		}
		return data, nil
	}
}

// errFromHTTPResponse builds the error returned from a non-2xx
// response. If the body parses as RFC 7807 problem+json (the new
// /api/v1 envelope) the operator-facing message uses title/detail.
// The legacy `{"error":"..."}` shape entire-core emits on /api/*
// is also recognised. Wrapped one level deep for JSON arrays — some
// reverse proxies return arrays — and falls back to the raw body
// otherwise.
func errFromHTTPResponse(status int, body []byte) error {
	body = bytes.TrimSpace(body)
	if msg := ExtractAPIErrorMessage(body); msg != "" {
		return fmt.Errorf("HTTP %d: %s", status, msg)
	}
	return fmt.Errorf("HTTP %d: %s", status, string(body))
}

// ExtractAPIErrorMessage tries to pull a human message out of a JSON
// error body. It handles both the RFC 7807 problem+json shape
// ({"title":"...","detail":"..."}) the /api/v1 surface emits and the
// legacy {"error":"..."} envelope, preferring detail > title > error,
// and peels the first element of an array body (some reverse proxies
// wrap errors in one). Returns "" when no shape it recognises matches.
func ExtractAPIErrorMessage(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	switch body[0] {
	case '{':
		var p struct {
			Title  string `json:"title"`
			Detail string `json:"detail"`
			Error  string `json:"error"`
		}
		if err := json.Unmarshal(body, &p); err == nil {
			switch {
			case p.Detail != "":
				return p.Detail
			case p.Title != "":
				return p.Title
			case p.Error != "":
				return p.Error
			}
		}
	case '[':
		// Some reverse proxies and aggregator gateways return an
		// array of error objects. Peel the first element.
		var arr []json.RawMessage
		if err := json.Unmarshal(body, &arr); err == nil && len(arr) > 0 {
			return ExtractAPIErrorMessage(arr[0])
		}
	}
	return ""
}

// parseRetryAfter parses an RFC 9110 Retry-After header. Only the
// delta-seconds form is supported — entire-core never emits HTTP-date.
func parseRetryAfter(v string) (time.Duration, bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0, false
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return 0, false
	}
	return time.Duration(n) * time.Second, true
}

func (c *Client) GetJSON(path string) ([]byte, error) {
	return c.do("GET", path, nil)
}

func (c *Client) PostJSON(path string, body any) ([]byte, error) {
	return c.do("POST", path, body)
}

func (c *Client) Delete(path string) ([]byte, error) {
	return c.do("DELETE", path, nil)
}

// PostForm submits a form-encoded POST. The /api/authz/sts/token
// endpoint takes RFC 8693 form fields and rejects JSON bodies, so
// callers reaching that endpoint use this rather than PostJSON.
func (c *Client) PostForm(path string, form url.Values) ([]byte, error) {
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, c.BaseURL+path, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if cfErr := httputil.CloudflareChallengeError(resp, data); cfErr != nil {
		return nil, cfErr
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(data))
	}
	return data, nil
}
