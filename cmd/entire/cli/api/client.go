package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

const (
	maxResponseBytes = 1 << 20
	userAgent        = "entire-cli"
)

// Client is an authenticated HTTP client for the Entire API.
// It attaches the bearer token to all outgoing requests via the Authorization header.
type Client struct {
	httpClient *http.Client
	baseURL    string
}

// NewClient creates a new authenticated API client with an explicit bearer token.
func NewClient(token string) *Client {
	return &Client{
		httpClient: &http.Client{
			Transport: &bearerTransport{
				token: token,
				base:  http.DefaultTransport,
			},
		},
		baseURL: BaseURL(),
	}
}

// bearerTransport is an http.RoundTripper that injects the Authorization header.
type bearerTransport struct {
	token string
	base  http.RoundTripper
}

func (t *bearerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// Clone the request to avoid mutating the caller's request.
	r := req.Clone(req.Context())
	r.Header.Set("Authorization", "Bearer "+t.token)
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
	return c.do(ctx, http.MethodGet, path, nil)
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
	return c.do(ctx, http.MethodPost, path, reader)
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
	return c.do(ctx, http.MethodPut, path, reader)
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
	return c.do(ctx, http.MethodPatch, path, reader)
}

// Delete sends an authenticated DELETE request to the given API-relative path.
func (c *Client) Delete(ctx context.Context, path string) (*http.Response, error) {
	return c.do(ctx, http.MethodDelete, path, nil)
}

func (c *Client) do(ctx context.Context, method, path string, body io.Reader) (*http.Response, error) {
	endpoint, err := ResolveURLFromBase(c.baseURL, path)
	if err != nil {
		return nil, fmt.Errorf("resolve URL %s: %w", path, err)
	}

	req, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
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

// ErrorResponse represents a standard API error response.
type ErrorResponse struct {
	Error string `json:"error"`
}

// CheckResponse returns an error if the response status code indicates failure.
// For non-2xx responses, it reads and parses the error message from the body.
// The caller is responsible for closing resp.Body.
func CheckResponse(resp *http.Response) error {
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return fmt.Errorf("API error: status %d", resp.StatusCode)
	}

	var apiErr ErrorResponse
	if err := json.Unmarshal(body, &apiErr); err == nil && strings.TrimSpace(apiErr.Error) != "" {
		return fmt.Errorf("API error: %s (status %d)", apiErr.Error, resp.StatusCode)
	}

	text := strings.TrimSpace(string(body))
	if text != "" {
		return fmt.Errorf("API error: %s (status %d)", text, resp.StatusCode)
	}

	return fmt.Errorf("API error: status %d", resp.StatusCode)
}
