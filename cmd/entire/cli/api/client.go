package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/versioninfo"
)

const (
	maxResponseBytes = 16 << 20
)

// Client is an authenticated HTTP client for the Entire API.
// It attaches the bearer token to all outgoing requests via the Authorization header.
type Client struct {
	httpClient *http.Client
	baseURL    string

	trailRoutes map[string]string

	// authSessionsPath is the base path for entire-core's login-session
	// endpoints (list / revoke / current). Set via WithAuthSessionsPath when the
	// client targets the auth host; empty otherwise, and the session methods
	// error out if called against an empty path.
	authSessionsPath string
}

// WithAuthSessionsPath sets the base path used by ListAuthSessions,
// RevokeCurrentAuthSession, and RevokeAuthSession. Returns the receiver for chaining
// at construction:
//
//	c := api.NewClientWithBaseURL(token, base).WithAuthSessionsPath(p)
func (c *Client) WithAuthSessionsPath(path string) *Client {
	c.authSessionsPath = path
	return c
}

// NewClientWithBaseURL creates a new authenticated API client targeting an
// explicit base URL.
//
// This is the only constructor on purpose. Its predecessor, NewClient(token),
// defaulted the host to BaseURL() — ENTIRE_API_BASE_URL or the production apex —
// which is exactly the ambient default the data plane no longer has: the host
// belongs to the acting login and comes from auth.ResolveDataAPI /
// auth.DataBaseURL. Keeping a one-argument constructor around would let the next
// caller reintroduce that silently, sending a staging login's bearer to
// entire.io, so the base URL is a required argument and every caller has to say
// where it got it.
func NewClientWithBaseURL(token, baseURL string) *Client {
	return &Client{
		httpClient: &http.Client{
			Transport: &bearerTransport{
				token: token,
				base:  http.DefaultTransport,
			},
			// A cross-host redirect must never carry the Entire bearer to
			// another origin; refuse it rather than follow it. Same-origin
			// requests are guaranteed by the base-host check in do().
			CheckRedirect: rejectCrossHostRedirect,
		},
		baseURL: baseURL,
	}
}

// rejectCrossHostRedirect stops a redirect chain from leaving the origin the
// client was built for. Same-host redirects (e.g. a trailing-slash normalize)
// still follow, up to Go's usual 10-hop cap.
func rejectCrossHostRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 10 {
		return errors.New("stopped after 10 redirects")
	}
	if len(via) > 0 && !strings.EqualFold(req.URL.Host, via[0].URL.Host) {
		return fmt.Errorf("refusing redirect to a different host (%s → %s): the Entire bearer must not leave its origin", via[0].URL.Host, req.URL.Host)
	}
	return nil
}

// requireSameHost rejects an endpoint whose host differs from the base URL's.
// It guards the direct case (a path that resolved to another host); redirects
// are handled by rejectCrossHostRedirect.
func requireSameHost(baseURL, endpoint string) error {
	b, err := url.Parse(baseURL)
	if err != nil {
		return fmt.Errorf("parse base URL: %w", err)
	}
	e, err := url.Parse(endpoint)
	if err != nil {
		return fmt.Errorf("parse endpoint URL: %w", err)
	}
	if !strings.EqualFold(b.Host, e.Host) {
		return fmt.Errorf("refusing to send an authenticated request to %q, which is not the API host %q", e.Host, b.Host)
	}
	return nil
}

// bearerTransport is an http.RoundTripper that injects the Authorization header.
//
// The token is only ever sent to the client's base host: do() rejects a request
// URL whose host differs from the base, and CheckRedirect refuses a cross-host
// redirect, so every request this transport sees is same-origin as the base.
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
	r.Header.Set("User-Agent", versioninfo.UserAgent())
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

// doJSON sends an authenticated request with an optional JSON-marshaled body.
func (c *Client) doJSON(ctx context.Context, method, path string, body any) (*http.Response, error) {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("marshal request body: %w", err)
		}
		reader = bytes.NewReader(data)
	}
	return c.do(ctx, method, path, reader, nil)
}

// Post sends an authenticated POST request with a JSON body to the given API-relative path.
func (c *Client) Post(ctx context.Context, path string, body any) (*http.Response, error) {
	return c.doJSON(ctx, http.MethodPost, path, body)
}

// Put sends an authenticated PUT request with a JSON body to the given API-relative path.
func (c *Client) Put(ctx context.Context, path string, body any) (*http.Response, error) {
	return c.doJSON(ctx, http.MethodPut, path, body)
}

// Patch sends an authenticated PATCH request with a JSON body to the given API-relative path.
func (c *Client) Patch(ctx context.Context, path string, body any) (*http.Response, error) {
	return c.doJSON(ctx, http.MethodPatch, path, body)
}

// Delete sends an authenticated DELETE request to the given API-relative path.
func (c *Client) Delete(ctx context.Context, path string) (*http.Response, error) {
	return c.do(ctx, http.MethodDelete, path, nil, nil)
}

// Request sends an authenticated request with an explicit method, optional
// extra headers, and an optional raw body. It's the general-purpose escape
// hatch behind `entire api`; prefer the typed verbs (Get/Post/…) for normal
// use. The bearer, User-Agent, and default Accept are still attached by the
// transport; a body defaults to Content-Type: application/json unless the
// caller supplies its own via headers.
func (c *Client) Request(ctx context.Context, method, path string, headers http.Header, body io.Reader) (*http.Response, error) {
	return c.do(ctx, method, path, body, headers)
}

// SetTrailRoute registers the entire-api base path for a resolved trail, so
// later requests spelled with the trail's ID are rewritten onto its
// forge/owner/repo/number route (see rewriteTrailRoute).
func (c *Client) SetTrailRoute(trailID, path string) {
	trailID = strings.TrimSpace(trailID)
	path = strings.TrimRight(strings.TrimSpace(path), "/")
	if trailID == "" || path == "" {
		return
	}
	if c.trailRoutes == nil {
		c.trailRoutes = make(map[string]string)
	}
	c.trailRoutes[url.PathEscape(trailID)] = path
}

func (c *Client) rewriteTrailRoute(path string) string {
	const prefix = "/api/v1/trails/"
	if !strings.HasPrefix(path, prefix) || len(c.trailRoutes) == 0 {
		return path
	}
	rest := strings.TrimPrefix(path, prefix)
	id, suffix, _ := strings.Cut(rest, "/")
	base, ok := c.trailRoutes[id]
	if !ok {
		return path
	}
	if suffix == "" {
		return base
	}
	return base + "/" + suffix
}

func (c *Client) do(ctx context.Context, method, path string, body io.Reader, headers http.Header) (*http.Response, error) {
	path = c.rewriteTrailRoute(path)
	endpoint, err := ResolveURLFromBase(c.baseURL, path)
	if err != nil {
		return nil, fmt.Errorf("resolve URL %s: %w", path, err)
	}
	// The bearer is only ever sent to the API's own host. A path that resolves
	// to another host (absolute or scheme-relative URL) would otherwise redirect
	// the Authorization header off-origin.
	if err := requireSameHost(c.baseURL, endpoint); err != nil {
		return nil, err
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

	// Default a body's Content-Type to JSON, but don't clobber a caller-supplied
	// one — the `entire api -H 'Content-Type: …'` escape hatch must be able to
	// send non-JSON bodies.
	if body != nil && req.Header.Get("Content-Type") == "" {
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
// {"error":{"code":"...","message":"...",...}}; entire-api cells return RFC
// 9457 problem details (application/problem+json), whose human-readable text
// is detail with title as the coarser fallback. RequestID is a problem-details
// extension entire-api sets on every error; it is what support needs to find
// the server-side trace, so it is carried through to HTTPError.
type ErrorResponse struct {
	Error     any    `json:"error"`
	Detail    string `json:"detail"`
	Title     string `json:"title"`
	Code      string `json:"code"`
	RequestID string `json:"request_id"`
}

// ErrorCode extracts the stable machine-readable code, if the server sent one.
// entire-api's contract (docs/api-errors.md there) is to branch on code, never
// on message text: problem details and compact cell errors carry it top-level;
// the legacy nested envelope carries it as error.code.
func (e ErrorResponse) ErrorCode() string {
	if code := strings.TrimSpace(e.Code); code != "" {
		return code
	}
	if v, ok := e.Error.(map[string]any); ok {
		if code, ok := v["code"].(string); ok {
			return strings.TrimSpace(code)
		}
	}
	return ""
}

// Message extracts the human-readable error message from any envelope shape.
func (e ErrorResponse) Message() string {
	switch v := e.Error.(type) {
	case string:
		if message := strings.TrimSpace(v); message != "" {
			return message
		}
	case map[string]any:
		if message, ok := v["message"].(string); ok && strings.TrimSpace(message) != "" {
			return strings.TrimSpace(message)
		}
		if code, ok := v["code"].(string); ok && strings.TrimSpace(code) != "" {
			return strings.TrimSpace(code)
		}
	}
	if detail := strings.TrimSpace(e.Detail); detail != "" {
		return detail
	}
	return strings.TrimSpace(e.Title)
}

// HTTPError is returned by CheckResponse for non-2xx responses. Callers can use
// errors.As to inspect the HTTP status, or IsHTTPErrorStatus for a quick check.
type HTTPError struct {
	StatusCode int
	Message    string
	// Code is the server's stable error code (e.g. rate_limited, conflict,
	// wrong_cell) when it sent one. Branch on it, not on Message, which is
	// human-readable prose the server may reword.
	Code string
	// RequestID is the RFC 9457 request_id extension when the server sent one.
	RequestID string
}

func (e *HTTPError) Error() string {
	var out string
	if e.Message != "" {
		out = fmt.Sprintf("API error: %s (status %d)", e.Message, e.StatusCode)
	} else {
		out = fmt.Sprintf("API error: status %d", e.StatusCode)
	}
	if e.RequestID != "" {
		out += fmt.Sprintf(" [request %s]", e.RequestID)
	}
	return out
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
		apiError.RequestID = strings.TrimSpace(parsed.RequestID)
		apiError.Code = parsed.ErrorCode()
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
