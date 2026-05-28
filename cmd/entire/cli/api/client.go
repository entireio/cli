package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

const (
	maxResponseBytes = 16 << 20
	userAgent        = "entire-cli"
)

// Client is an authenticated HTTP client for the Entire API.
// It attaches the bearer token to all outgoing requests via the Authorization header.
type Client struct {
	httpClient *http.Client
	baseURL    string

	// authTokensPath is the base path for the auth-tokens management
	// endpoints (list / revoke). Set via WithAuthTokensPath when the
	// client targets the auth host. Empty for data-API-only clients;
	// auth-tokens methods error out if called against an empty path.
	authTokensPath string
}

// WithAuthTokensPath sets the base path used by ListTokens,
// RevokeCurrentToken, and RevokeToken. The path is supplied by the
// auth shim from auth.CurrentProvider().AuthTokensPath, which is the
// single source of truth for provider-version routing — the api
// package no longer reads ENTIRE_AUTH_PROVIDER_VERSION itself.
//
// Returns the receiver for chaining at construction:
//
//	c := api.NewClientWithBaseURL(token, base).WithAuthTokensPath(p)
func (c *Client) WithAuthTokensPath(path string) *Client {
	c.authTokensPath = path
	return c
}

// NewClient creates a new authenticated API client with an explicit bearer
// token, targeting the data API base URL (BaseURL()).
func NewClient(token string) *Client {
	return NewClientWithBaseURL(token, BaseURL())
}

// NewClientWithBaseURL creates a new authenticated API client targeting an
// explicit base URL. Use this for endpoints that live on the auth host (e.g.
// auth-token management) when ENTIRE_AUTH_BASE_URL splits the auth origin
// from the data API origin.
func NewClientWithBaseURL(token, baseURL string) *Client {
	return &Client{
		httpClient: &http.Client{
			Transport: &bearerTransport{
				token: token,
				base:  http.DefaultTransport,
			},
		},
		baseURL: baseURL,
	}
}

// bearerTransport is an http.RoundTripper that injects the Authorization header.
//
// When token is empty, the Authorization header is omitted (rather than sent
// as a malformed "Authorization: Bearer "). This supports endpoints like
// recap that deliberately want the unauthenticated request to reach the
// server so it can return a typed 401 — callers that want a local fast-fail
// for missing auth should check ErrNotLoggedIn at construction time, not
// rely on the transport.
type bearerTransport struct {
	token string
	base  http.RoundTripper
}

func (t *bearerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// Clone the request to avoid mutating the caller's request.
	r := req.Clone(req.Context())
	if t.token != "" {
		r.Header.Set("Authorization", "Bearer "+t.token)
	}
	r.Header.Set("User-Agent", userAgent)
	if r.Header.Get("Accept") == "" {
		r.Header.Set("Accept", "application/json")
	}
	resp, err := t.base.RoundTrip(r)
	if err != nil {
		return nil, fmt.Errorf("round trip: %w", err)
	}
	return resp, nil
}

// Get sends an authenticated GET request to the given API-relative path.
func (c *Client) Get(ctx context.Context, path string) (*http.Response, error) {
	return c.do(ctx, http.MethodGet, path, nil, nil)
}

// GetStream sends an authenticated GET request with optional extra request
// headers (e.g. Accept: text/event-stream, Last-Event-ID) and returns the
// response with the body still open. Callers are responsible for reading and
// closing resp.Body. Intended for streaming endpoints such as Server-Sent
// Events; for normal JSON requests use Get.
func (c *Client) GetStream(ctx context.Context, path string, headers http.Header) (*http.Response, error) {
	return c.do(ctx, http.MethodGet, path, nil, headers)
}

// Post sends an authenticated POST request with a JSON body to the given API-relative path.
func (c *Client) Post(ctx context.Context, path string, body any) (*http.Response, error) {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("marshal request body: %w", err)
		}
		reader = bytes.NewReader(data)
	}
	return c.do(ctx, http.MethodPost, path, reader, nil)
}

// Put sends an authenticated PUT request with a JSON body to the given API-relative path.
func (c *Client) Put(ctx context.Context, path string, body any) (*http.Response, error) {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("marshal request body: %w", err)
		}
		reader = bytes.NewReader(data)
	}
	return c.do(ctx, http.MethodPut, path, reader, nil)
}

// Patch sends an authenticated PATCH request with a JSON body to the given API-relative path.
func (c *Client) Patch(ctx context.Context, path string, body any) (*http.Response, error) {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("marshal request body: %w", err)
		}
		reader = bytes.NewReader(data)
	}
	return c.do(ctx, http.MethodPatch, path, reader, nil)
}

// Delete sends an authenticated DELETE request to the given API-relative path.
func (c *Client) Delete(ctx context.Context, path string) (*http.Response, error) {
	return c.do(ctx, http.MethodDelete, path, nil, nil)
}

func (c *Client) do(ctx context.Context, method, path string, body io.Reader, headers http.Header) (*http.Response, error) {
	endpoint, err := ResolveURLFromBase(c.baseURL, path)
	if err != nil {
		return nil, fmt.Errorf("resolve URL %s: %w", path, err)
	}

	req, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	for k, vs := range headers {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}

	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", method, path, err)
	}
	return resp, nil
}

// DecodeJSON reads the response body and decodes it into dest.
// It limits the body size to protect against unbounded reads.
// The caller is responsible for closing resp.Body.
func DecodeJSON(resp *http.Response, dest any) error {
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return fmt.Errorf("read response body: %w", err)
	}

	if err := json.Unmarshal(body, dest); err != nil {
		return fmt.Errorf("decode JSON response: %w", err)
	}

	return nil
}

// ErrorResponse represents a standard API error response. Older endpoints
// return {"error":"message"}; newer endpoints return
// {"error":{"code":"...","message":"...",...}}.
type ErrorResponse struct {
	Error any `json:"error"`
}

// Message extracts the human-readable error message from either envelope shape.
func (e ErrorResponse) Message() string {
	switch v := e.Error.(type) {
	case string:
		return strings.TrimSpace(v)
	case map[string]any:
		if message, ok := v["message"].(string); ok && strings.TrimSpace(message) != "" {
			return strings.TrimSpace(message)
		}
		if code, ok := v["code"].(string); ok && strings.TrimSpace(code) != "" {
			return strings.TrimSpace(code)
		}
	}
	return ""
}

// HTTPError is returned by CheckResponse for non-2xx responses. Callers can use
// errors.As to inspect the HTTP status, or IsHTTPErrorStatus for a quick check.
type HTTPError struct {
	StatusCode int
	Message    string
}

func (e *HTTPError) Error() string {
	if e.Message != "" {
		return fmt.Sprintf("API error: %s (status %d)", e.Message, e.StatusCode)
	}
	return fmt.Sprintf("API error: status %d", e.StatusCode)
}

// IsHTTPErrorStatus reports whether err wraps an *HTTPError with the given HTTP status.
func IsHTTPErrorStatus(err error, status int) bool {
	var ae *HTTPError
	return errors.As(err, &ae) && ae.StatusCode == status
}

// CheckResponse returns an error if the response status code indicates failure.
// For non-2xx responses, it reads and parses the error message from the body
// and returns it as an *HTTPError. The caller is responsible for closing resp.Body.
func CheckResponse(resp *http.Response) error {
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}

	apiError := &HTTPError{StatusCode: resp.StatusCode}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return apiError
	}

	var parsed ErrorResponse
	if err := json.Unmarshal(body, &parsed); err == nil {
		if message := parsed.Message(); message != "" {
			apiError.Message = message
			return apiError
		}
	}

	if text := strings.TrimSpace(string(body)); text != "" {
		apiError.Message = text
	}
	return apiError
}
