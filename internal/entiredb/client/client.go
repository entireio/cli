package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/entireio/cli/internal/entiredb/api/repov1"
	"github.com/entireio/cli/internal/entiredb/client/repocreds"
	"github.com/entireio/cli/internal/entiredb/tokenstore"
)

const callTimeout = 300 * time.Second

var ErrDiffTruncated = errors.New("diff truncated")

// Client is not safe for concurrent use. doHTTP mutates c.token in place when
// it proactively refreshes or after a 401 retry, and authTransport reads the
// same field on every RoundTrip. CLI callers issue one call at a time; share
// across goroutines requires external synchronization.
type Client struct {
	repoHTTP    *http.Client
	repoBaseURL string
	authCfg     *AuthRefreshConfig

	token string

	// scopedCoreURL / scopedClusterURL configure the scoped-JWT exchange when
	// set via WithScopedExchange. scopedClusterURL is the canonical cluster
	// URL used as the JWT audience, distinct from repoBaseURL (the dial
	// target, which may be a per-replica node host).
	scopedCoreURL    string
	scopedClusterURL string

	// scopedCreds caches per-(repo, action) scoped JWTs. Lazily initialized
	// on the first /api/v1/repos/{repoId}/... call; nil for Client instances
	// that only drive the admin gRPC surface or the Resolve route.
	scopedCreds *repocreds.Cache
}

type ConnFunc func(c *Client) error

type RepoOption func(*repoOptions)

type repoOptions struct {
	httpClient *http.Client
	// scopedCoreURL / scopedClusterURL configure where per-repo scoped JWTs
	// are minted and which cluster they're addressed to. clusterURL is the
	// canonical cluster URL (the audience host the server trusts), which is
	// deliberately distinct from the dial target — a client may dial a
	// specific replica node whose host is not in the trusted-audience set.
	scopedCoreURL    string
	scopedClusterURL string
}

func WithRepoHTTPClient(httpClient *http.Client) RepoOption {
	return func(o *repoOptions) { o.httpClient = httpClient }
}

// WithScopedExchange configures the client to mint repo-scoped JWTs for
// /api/v1/repos/{repoId}/... calls by exchanging the bearer (login) token at
// coreURL's /oauth/token endpoint, with the audience addressed to clusterURL.
// clusterURL must be the canonical cluster URL, NOT a per-replica dial target,
// since the server verifies the JWT audience host against its trusted set.
// When this option is absent, per-repo calls fall back to sending the bearer
// token directly (the pre-scoped-JWT behavior).
func WithScopedExchange(coreURL, clusterURL string) RepoOption {
	return func(o *repoOptions) {
		o.scopedCoreURL = coreURL
		o.scopedClusterURL = clusterURL
	}
}

func ConnectRepoWithAuth(baseURL, token string, clientConnFunc ConnFunc, opts ...RepoOption) error {
	client := newRepoClient(baseURL, token, nil, opts...)
	return clientConnFunc(client)
}

func ConnectRepoWithRefresh(baseURL string, cfg AuthRefreshConfig, clientConnFunc ConnFunc, opts ...RepoOption) error {
	token, err := getTokenFromKeyring(cfg.KeyringService, cfg.Username)
	if err != nil {
		return fmt.Errorf("getting token: %w", err)
	}
	client := newRepoClient(baseURL, token, &cfg, opts...)
	return clientConnFunc(client)
}

func newRepoClient(baseURL, token string, cfg *AuthRefreshConfig, opts ...RepoOption) *Client {
	var o repoOptions
	for _, opt := range opts {
		opt(&o)
	}
	httpClient := cloneHTTPClient(o.httpClient)
	if httpClient == nil {
		httpClient = cloneHTTPClient(nil)
	}
	if httpClient.Transport == nil {
		httpClient.Transport = http.DefaultTransport
	}
	client := &Client{
		repoHTTP:         httpClient,
		repoBaseURL:      strings.TrimRight(baseURL, "/"),
		authCfg:          cfg,
		token:            token,
		scopedCoreURL:    strings.TrimRight(o.scopedCoreURL, "/"),
		scopedClusterURL: strings.TrimRight(o.scopedClusterURL, "/"),
	}
	httpClient.Transport = AuthTransport(&client.token, httpClient.Transport)
	return client
}

func cloneHTTPClient(c *http.Client) *http.Client {
	if c == nil {
		return &http.Client{}
	}
	clone := *c
	return &clone
}

func getTokenFromKeyring(keyringService, username string) (string, error) {
	encodedToken, err := tokenstore.Get(keyringService, username)
	if err != nil {
		return "", fmt.Errorf("getting token from keyring: %w", err)
	}
	// Decode the token (may include expiration timestamp)
	token, _ := tokenstore.DecodeTokenWithExpiration(encodedToken)
	return token, nil
}

type HTTPError struct {
	StatusCode int
	Title      string
	Detail     string
	Body       []byte
}

func (e *HTTPError) Error() string {
	if e.Detail != "" {
		return fmt.Sprintf("HTTP %d: %s", e.StatusCode, e.Detail)
	}
	if e.Title != "" {
		return fmt.Sprintf("HTTP %d: %s", e.StatusCode, e.Title)
	}
	return fmt.Sprintf("HTTP %d", e.StatusCode)
}

func (c *Client) repoOrigin() error {
	if c.repoHTTP == nil || c.repoBaseURL == "" {
		return errors.New("repo HTTP client is not configured")
	}
	return nil
}

func (c *Client) repoURL(path string, q url.Values) (string, error) {
	if err := c.repoOrigin(); err != nil {
		return "", err
	}
	u, err := url.Parse(c.repoBaseURL + "/api/v1" + path)
	if err != nil {
		return "", fmt.Errorf("parse repo URL: %w", err)
	}
	u.RawQuery = q.Encode()
	return u.String(), nil
}

func (c *Client) doHTTP(ctx context.Context, method, path string, q url.Values, body []byte, contentType, accept string) (*http.Response, error) {
	if c.authCfg != nil && c.token != "" {
		c.token = proactiveRefresh(ctx, *c.authCfg, &c.token, c.token)
	}
	resp, err := c.doHTTPOnce(ctx, method, path, q, body, contentType, accept)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return resp, nil
	}
	if resp.StatusCode == http.StatusUnauthorized && c.authCfg != nil {
		_, _ = io.Copy(io.Discard, resp.Body) //nolint:errcheck // best-effort drain before retry
		_ = resp.Body.Close()
		newToken, refreshErr := RefreshAccessToken(ctx, *c.authCfg)
		if refreshErr != nil {
			return nil, fmt.Errorf("token refresh failed after 401: %w", refreshErr)
		}
		c.token = newToken
		resp, err = c.doHTTPOnce(ctx, method, path, q, body, contentType, accept)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return resp, nil
		}
	}
	defer resp.Body.Close()
	return nil, parseHTTPError(resp)
}

// doRepoJSON is doJSON for /repos/{repoId}/... routes. It mints (or reuses) a
// repo-scoped JWT for (repoID, op) via repocreds and pre-sets the Authorization
// header so the login-JWT-based AuthTransport leaves it alone. op is "pull"
// for reads and "push" for endpoints that mutate refs. On 401 the scoped JWT
// is invalidated and re-minted once before giving up.
func (c *Client) doRepoJSON(ctx context.Context, method, repoID, suffix, op string, q url.Values, body, out any) error {
	var bodyBytes []byte
	contentType := ""
	if body != nil {
		var err error
		bodyBytes, err = json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encode request: %w", err)
		}
		contentType = "application/json"
	}
	resp, err := c.doRepoHTTP(ctx, method, repoPath(repoID, suffix), "/git/repo/"+repoID, op, q, bodyBytes, contentType, "application/json")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body) //nolint:errcheck // best-effort drain
		return nil
	}
	dec := json.NewDecoder(resp.Body)
	if err := dec.Decode(out); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}

// doRepoStream is doStream for /repos/{repoId}/... routes.
func (c *Client) doRepoStream(ctx context.Context, method, repoID, suffix, op string, q url.Values, body any, accept string) (*http.Response, error) {
	var bodyBytes []byte
	contentType := ""
	if body != nil {
		var err error
		bodyBytes, err = json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("encode request: %w", err)
		}
		contentType = "application/json"
	}
	return c.doRepoHTTP(ctx, method, repoPath(repoID, suffix), "/git/repo/"+repoID, op, q, bodyBytes, contentType, accept)
}

// doResolveJSON issues GET /repos/resolve authenticated with a repo-scoped
// "pull" JWT minted for the repo-path audience (audienceSuffix = "/"+repoPath,
// e.g. "/et/project/repo"). Resolve is the bootstrap call, but it is still
// gated on a scoped token: core resolves the path audience to a ULID and runs
// the SpiceDB check at mint time, and the server re-checks PermitsGit. When no
// scoped exchange is configured (ENTIRE_TOKEN), the bearer token is sent
// directly, mirroring doRepoHTTP.
func (c *Client) doResolveJSON(ctx context.Context, repoPathValue string, q url.Values, out any) error {
	audienceSuffix := "/" + strings.TrimPrefix(repoPathValue, "/")
	resp, err := c.doRepoHTTP(ctx, http.MethodGet, "/repos/resolve", audienceSuffix, "pull", q, nil, "", "application/json")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	dec := json.NewDecoder(resp.Body)
	if err := dec.Decode(out); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}

// doRepoHTTP issues a request authenticated with a repo-scoped JWT minted
// for audienceSuffix (the path portion of the JWT audience core resolves to
// a repo ULID) and action op. path is the request URL path on the data
// plane; it is distinct from audienceSuffix because the per-repo routes hit
// /api/v1/repos/{ulid}/... while the token's audience is "/git/repo/{ulid}",
// and Resolve hits /repos/resolve while the audience is the repo path.
func (c *Client) doRepoHTTP(ctx context.Context, method, path, audienceSuffix, op string, q url.Values, body []byte, contentType, accept string) (*http.Response, error) {
	creds := c.repocredsCache()
	if creds == nil {
		// No scoped exchange configured (e.g. ConnectRepoWithAuth /
		// ENTIRE_TOKEN). Send the bearer token directly via the login-JWT
		// path, preserving pre-scoped-JWT behavior. The caller is
		// responsible for the token being valid for this route.
		return c.doHTTP(ctx, method, path, q, body, contentType, accept)
	}

	token, err := creds.Token(ctx, audienceSuffix, op)
	if err != nil {
		return nil, fmt.Errorf("mint scoped JWT: %w", err)
	}
	resp, err := c.doRepoHTTPOnce(ctx, method, path, q, body, contentType, accept, token)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return resp, nil
	}
	if resp.StatusCode == http.StatusUnauthorized {
		_, _ = io.Copy(io.Discard, resp.Body) //nolint:errcheck // best-effort drain before retry
		_ = resp.Body.Close()
		// A 401 can mean the scoped JWT is stale OR the underlying login JWT
		// is expired. Invalidate the scoped entry AND refresh the login JWT
		// (mirroring doHTTP) so the re-mint exchanges with a fresh subject
		// token rather than replaying the rejected one.
		creds.Invalidate(audienceSuffix, op)
		if c.authCfg != nil {
			if newToken, refreshErr := RefreshAccessToken(ctx, *c.authCfg); refreshErr == nil {
				c.token = newToken
			}
		}
		token, err = creds.Token(ctx, audienceSuffix, op)
		if err != nil {
			return nil, fmt.Errorf("re-mint scoped JWT after 401: %w", err)
		}
		resp, err = c.doRepoHTTPOnce(ctx, method, path, q, body, contentType, accept, token)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return resp, nil
		}
	}
	defer resp.Body.Close()
	return nil, parseHTTPError(resp)
}

func (c *Client) doRepoHTTPOnce(ctx context.Context, method, path string, q url.Values, body []byte, contentType, accept, scopedToken string) (*http.Response, error) {
	u, err := c.repoURL(path, q)
	if err != nil {
		return nil, err
	}
	var r io.Reader
	if body != nil {
		r = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, r)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	// Pre-set Authorization so AuthTransport leaves it alone (RoundTrip
	// only fills in the login JWT when the header is empty).
	req.Header.Set("Authorization", "Bearer "+scopedToken)
	resp, err := c.repoHTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("send request: %w", err)
	}
	return resp, nil
}

// repocredsCache lazily builds c.scopedCreds with a loginJWTProvider that
// pulls the live login JWT (post proactive refresh) on every exchange.
//
// The exchange endpoint (coreURL) comes from WithScopedExchange, falling back
// to authCfg.CoreBaseURL. The audience cluster URL comes from
// WithScopedExchange, falling back to repoBaseURL. When no coreURL is
// available, returns nil: the caller then sends the bearer token directly
// (the pre-scoped-JWT path) instead of hard-failing — this keeps
// ConnectRepoWithAuth / ENTIRE_TOKEN working. A misconfigured repoBaseURL
// surfaces downstream when repoURL builds the request.
func (c *Client) repocredsCache() *repocreds.Cache {
	if c.scopedCreds != nil {
		return c.scopedCreds
	}
	coreURL := c.scopedCoreURL
	if coreURL == "" && c.authCfg != nil {
		coreURL = c.authCfg.CoreBaseURL
	}
	if coreURL == "" {
		return nil
	}
	clusterURL := c.scopedClusterURL
	if clusterURL == "" {
		clusterURL = c.repoBaseURL
	}
	provider := func(ctx context.Context) (string, error) {
		if c.token == "" {
			return "", errors.New("client: no login JWT available for scoped exchange")
		}
		if c.authCfg != nil {
			c.token = proactiveRefresh(ctx, *c.authCfg, &c.token, c.token)
		}
		return c.token, nil
	}
	c.scopedCreds = repocreds.New(coreURL, clusterURL, provider, c.repoHTTP)
	return c.scopedCreds
}

func (c *Client) doHTTPOnce(ctx context.Context, method, path string, q url.Values, body []byte, contentType, accept string) (*http.Response, error) {
	u, err := c.repoURL(path, q)
	if err != nil {
		return nil, err
	}
	var r io.Reader
	if body != nil {
		r = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, r)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	resp, err := c.repoHTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("send request: %w", err)
	}
	return resp, nil
}

func parseHTTPError(resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20)) //nolint:errcheck // best-effort diagnostics
	herr := &HTTPError{StatusCode: resp.StatusCode, Body: body}
	var problem struct {
		Title  string `json:"title"`
		Detail string `json:"detail"`
		Status int    `json:"status"`
	}
	if strings.Contains(resp.Header.Get("Content-Type"), "json") {
		_ = json.Unmarshal(body, &problem) //nolint:errcheck // best-effort
		herr.Title = problem.Title
		herr.Detail = problem.Detail
		if problem.Status != 0 {
			herr.StatusCode = problem.Status
		}
	}
	return herr
}

func withCallTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, callTimeout)
}

func repoPath(repoID, suffix string) string {
	return "/repos/" + url.PathEscape(repoID) + suffix
}

func addIf(q url.Values, key, value string) {
	if value != "" {
		q.Set(key, value)
	}
}

func addTimeIf(q url.Values, key string, value *time.Time) {
	if value != nil && !value.IsZero() {
		q.Set(key, value.Format(time.RFC3339))
	}
}

func addIntIf(q url.Values, key string, value int32) {
	if value != 0 {
		q.Set(key, strconv.FormatInt(int64(value), 10))
	}
}

// RepoResolve translates a prefixed repo reference ("et/project/repo",
// "git/owner/repo", or "gh/owner/repo" for GitHub-mirror repos) to its
// ULID. The surface prefix is required: it names the lookup path and the
// audience of the repo-scoped "pull" JWT this call mints. Intended as a
// one-shot lookup before calling any repo-scoped RPC.
func (c *Client) RepoResolve(ctx context.Context, repoPathValue string) (string, error) {
	ctx, cancel := withCallTimeout(ctx)
	defer cancel()
	q := url.Values{"repoPath": {repoPathValue}}
	var resp repov1.ResolveResponse
	if err := c.doResolveJSON(ctx, repoPathValue, q, &resp); err != nil {
		return "", fmt.Errorf("resolve repo: %w", err)
	}
	return resp.RepoID, nil
}

func (c *Client) RepoResolveWithReplicas(ctx context.Context, repoPathValue string) (id string, replicas []string, err error) {
	ctx, cancel := withCallTimeout(ctx)
	defer cancel()
	q := url.Values{"repoPath": {repoPathValue}, "includeReplicas": {"true"}}
	var resp repov1.ResolveResponse
	if err := c.doResolveJSON(ctx, repoPathValue, q, &resp); err != nil {
		return "", nil, fmt.Errorf("resolve repo: %w", err)
	}
	return resp.RepoID, resp.Replicas, nil
}

func (c *Client) RepoMergeBase(ctx context.Context, repoID, commitA, commitB string) (string, error) {
	resp, err := c.RepoMergeBaseFull(ctx, repoID, commitA, commitB)
	if err != nil {
		return "", err
	}
	return resp.MergeBaseSHA, nil
}

func (c *Client) RepoMergeBaseFull(ctx context.Context, repoID, commitA, commitB string) (*repov1.MergeBaseResponse, error) {
	ctx, cancel := withCallTimeout(ctx)
	defer cancel()
	q := url.Values{"commitA": {commitA}, "commitB": {commitB}}
	var resp repov1.MergeBaseResponse
	if err := c.doRepoJSON(ctx, http.MethodGet, repoID, "/merge-base", "pull", q, nil, &resp); err != nil {
		return nil, fmt.Errorf("failed to get merge base: %w", err)
	}
	return &resp, nil
}

func (c *Client) RepoListCommits(ctx context.Context, repoID string, req *repov1.ListCommitsRequest) (*repov1.ListCommitsResponse, error) {
	ctx, cancel := withCallTimeout(ctx)
	defer cancel()
	if req == nil {
		req = &repov1.ListCommitsRequest{}
	}
	q := url.Values{}
	addIf(q, "ref", req.Ref)
	addIf(q, "notRef", req.NotRef)
	addTimeIf(q, "since", req.Since)
	addTimeIf(q, "until", req.Until)
	addIf(q, "path", req.Path)
	addIf(q, "author", req.Author)
	addIntIf(q, "pageSize", req.PageSize)
	addIf(q, "pageToken", req.PageToken)
	if req.FirstParent {
		q.Set("firstParent", "true")
	}
	if req.ParseTrailers {
		q.Set("parseTrailers", "true")
	}
	var resp repov1.ListCommitsResponse
	if err := c.doRepoJSON(ctx, http.MethodGet, repoID, "/commits", "pull", q, nil, &resp); err != nil {
		return nil, fmt.Errorf("list commits: %w", err)
	}
	return &resp, nil
}

func (c *Client) RepoListBranches(ctx context.Context, repoID string, req *repov1.ListBranchesRequest) (*repov1.ListBranchesResponse, error) {
	ctx, cancel := withCallTimeout(ctx)
	defer cancel()
	if req == nil {
		req = &repov1.ListBranchesRequest{}
	}
	q := url.Values{}
	addIf(q, "search", req.Search)
	addIf(q, "regex", req.Regex)
	addIntIf(q, "pageSize", req.PageSize)
	addIf(q, "pageToken", req.PageToken)
	var resp repov1.ListBranchesResponse
	if err := c.doRepoJSON(ctx, http.MethodGet, repoID, "/branches", "pull", q, nil, &resp); err != nil {
		return nil, fmt.Errorf("list branches: %w", err)
	}
	return &resp, nil
}

func (c *Client) RepoListTags(ctx context.Context, repoID string, req *repov1.ListTagsRequest) (*repov1.ListTagsResponse, error) {
	ctx, cancel := withCallTimeout(ctx)
	defer cancel()
	if req == nil {
		req = &repov1.ListTagsRequest{}
	}
	q := url.Values{}
	addIf(q, "search", req.Search)
	addIf(q, "regex", req.Regex)
	addIntIf(q, "pageSize", req.PageSize)
	addIf(q, "pageToken", req.PageToken)
	var resp repov1.ListTagsResponse
	if err := c.doRepoJSON(ctx, http.MethodGet, repoID, "/tags", "pull", q, nil, &resp); err != nil {
		return nil, fmt.Errorf("list tags: %w", err)
	}
	return &resp, nil
}

func (c *Client) RepoGetTree(ctx context.Context, repoID string, req *repov1.GetTreeRequest) (*repov1.GetTreeResponse, error) {
	ctx, cancel := withCallTimeout(ctx)
	defer cancel()
	if req == nil {
		req = &repov1.GetTreeRequest{}
	}
	q := url.Values{}
	addIf(q, "ref", req.Ref)
	addIf(q, "path", req.Path)
	if req.Recursive {
		q.Set("recursive", "true")
	}
	var resp repov1.GetTreeResponse
	if err := c.doRepoJSON(ctx, http.MethodGet, repoID, "/tree", "pull", q, nil, &resp); err != nil {
		return nil, fmt.Errorf("get tree: %w", err)
	}
	return &resp, nil
}

// FileVisitor is invoked once per file in a GetFiles stream. The reader is the
// multipart part body for that file and is valid only until the visitor returns.
type FileVisitor func(header *repov1.FileHeader, r io.Reader) error

func (c *Client) RepoGetFiles(ctx context.Context, repoID string, req *repov1.GetFilesRequest, visit FileVisitor) error {
	ctx, cancel := withCallTimeout(ctx)
	defer cancel()
	if req == nil {
		return errors.New("get files: request is nil")
	}
	q := url.Values{}
	addIf(q, "ref", req.Ref)
	for _, p := range req.Paths {
		q.Add("path", p)
	}
	resp, err := c.doRepoStream(ctx, http.MethodGet, repoID, "/files", "pull", q, nil, "multipart/mixed")
	if err != nil {
		return fmt.Errorf("get files: %w", err)
	}
	defer resp.Body.Close()
	mediaType, params, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil {
		return fmt.Errorf("get files: parse content type: %w", err)
	}
	if mediaType != "multipart/mixed" {
		return fmt.Errorf("get files: unexpected content type %q", mediaType)
	}
	mr := multipart.NewReader(resp.Body, params["boundary"])
	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("get files: read part: %w", err)
		}
		header, err := fileHeaderFromPart(part.Header)
		if err != nil {
			_ = part.Close()
			return err
		}
		if err := visit(header, part); err != nil {
			_ = part.Close()
			return err
		}
		_ = part.Close()
	}
}

func fileHeaderFromPart(h textproto.MIMEHeader) (*repov1.FileHeader, error) {
	return fileHeaderFromHTTPHeader(http.Header(h))
}

func fileHeaderFromHTTPHeader(h http.Header) (*repov1.FileHeader, error) {
	path, err := url.PathUnescape(h.Get("Entire-Path"))
	if err != nil {
		return nil, fmt.Errorf("get files: bad Entire-Path: %w", err)
	}
	size, err := strconv.ParseInt(h.Get("Entire-Size"), 10, 64)
	if err != nil && h.Get("Entire-Size") != "" {
		return nil, fmt.Errorf("get files: bad Entire-Size: %w", err)
	}
	status := repov1.FileStatus(h.Get("Entire-Status"))
	if status == "" {
		status = repov1.FileStatusOK
	}
	return &repov1.FileHeader{Path: path, SHA: h.Get("Entire-Sha"), Size: size, Status: status}, nil
}

func (c *Client) RepoGetRawFile(ctx context.Context, repoID string, req *repov1.RawFileRequest, w io.Writer) (*repov1.FileHeader, error) {
	ctx, cancel := withCallTimeout(ctx)
	defer cancel()
	if req == nil {
		return nil, errors.New("get raw file: request is nil")
	}
	q := url.Values{}
	addIf(q, "ref", req.Ref)
	addIf(q, "path", req.Path)
	resp, err := c.doRepoStream(ctx, http.MethodGet, repoID, "/raw", "pull", q, nil, "application/octet-stream")
	if err != nil {
		return nil, fmt.Errorf("get raw file: %w", err)
	}
	header, err := fileHeaderFromHTTPHeader(resp.Header)
	if err != nil {
		_ = resp.Body.Close()
		return nil, err
	}
	if w == nil {
		w = io.Discard
	}
	_, copyErr := io.Copy(w, resp.Body)
	closeErr := resp.Body.Close()
	if copyErr != nil {
		return nil, fmt.Errorf("get raw file: write output: %w", copyErr)
	}
	if closeErr != nil {
		return nil, fmt.Errorf("get raw file: close body: %w", closeErr)
	}
	return header, nil
}

func (c *Client) RepoCompare(ctx context.Context, repoID string, req *repov1.CompareRequest) (*repov1.CompareResponse, error) {
	ctx, cancel := withCallTimeout(ctx)
	defer cancel()
	if req == nil {
		req = &repov1.CompareRequest{}
	}
	q := url.Values{}
	addIf(q, "base", req.Base)
	addIf(q, "head", req.Head)
	addIf(q, "path", req.Path)
	var resp repov1.CompareResponse
	if err := c.doRepoJSON(ctx, http.MethodGet, repoID, "/compare", "pull", q, nil, &resp); err != nil {
		return nil, fmt.Errorf("compare: %w", err)
	}
	return &resp, nil
}

func (c *Client) RepoDiff(ctx context.Context, repoID string, req *repov1.DiffRequest, w io.Writer) error {
	ctx, cancel := withCallTimeout(ctx)
	defer cancel()
	if req == nil {
		return errors.New("diff: request is nil")
	}
	q := url.Values{}
	addIf(q, "base", req.Base)
	addIf(q, "head", req.Head)
	addIf(q, "path", req.Path)
	resp, err := c.doRepoStream(ctx, http.MethodGet, repoID, "/diff", "pull", q, nil, "application/octet-stream")
	if err != nil {
		return fmt.Errorf("diff: %w", err)
	}
	_, copyErr := io.Copy(w, resp.Body)
	closeErr := resp.Body.Close()
	if copyErr != nil {
		return fmt.Errorf("diff: write output: %w", copyErr)
	}
	if closeErr != nil {
		return fmt.Errorf("diff: close body: %w", closeErr)
	}
	if strings.EqualFold(resp.Trailer.Get("X-Entire-Diff-Truncated"), "true") {
		return ErrDiffTruncated
	}
	return nil
}

func (c *Client) RepoMerge(ctx context.Context, repoID string, req *repov1.MergeRequest) (*repov1.MergeResponse, error) {
	ctx, cancel := withCallTimeout(ctx)
	defer cancel()
	var resp repov1.MergeResponse
	if err := c.doRepoJSON(ctx, http.MethodPost, repoID, "/merges", "push", nil, req, &resp); err != nil {
		return nil, fmt.Errorf("merge: %w", err)
	}
	return &resp, nil
}

func (c *Client) RepoRevert(ctx context.Context, repoID string, req *repov1.RevertRequest) (*repov1.RevertResponse, error) {
	ctx, cancel := withCallTimeout(ctx)
	defer cancel()
	var resp repov1.RevertResponse
	if err := c.doRepoJSON(ctx, http.MethodPost, repoID, "/reverts", "push", nil, req, &resp); err != nil {
		return nil, fmt.Errorf("revert: %w", err)
	}
	return &resp, nil
}

func (c *Client) RepoDryRunMerge(ctx context.Context, repoID string, req *repov1.DryRunMergeRequest) (*repov1.DryRunMergeResponse, error) {
	ctx, cancel := withCallTimeout(ctx)
	defer cancel()
	var resp repov1.DryRunMergeResponse
	if err := c.doRepoJSON(ctx, http.MethodPost, repoID, "/merge-previews", "pull", nil, req, &resp); err != nil {
		return nil, fmt.Errorf("dry-run merge: %w", err)
	}
	return &resp, nil
}

func (c *Client) RepoDryRunRebase(ctx context.Context, repoID string, req *repov1.DryRunRebaseRequest) (*repov1.DryRunRebaseResponse, error) {
	ctx, cancel := withCallTimeout(ctx)
	defer cancel()
	var resp repov1.DryRunRebaseResponse
	if err := c.doRepoJSON(ctx, http.MethodPost, repoID, "/rebase-previews", "pull", nil, req, &resp); err != nil {
		return nil, fmt.Errorf("dry-run rebase: %w", err)
	}
	return &resp, nil
}

func (c *Client) RepoDryRunRevert(ctx context.Context, repoID string, req *repov1.DryRunRevertRequest) (*repov1.DryRunRevertResponse, error) {
	ctx, cancel := withCallTimeout(ctx)
	defer cancel()
	var resp repov1.DryRunRevertResponse
	if err := c.doRepoJSON(ctx, http.MethodPost, repoID, "/revert-previews", "pull", nil, req, &resp); err != nil {
		return nil, fmt.Errorf("dry-run revert: %w", err)
	}
	return &resp, nil
}

type RepoRebaseStream struct {
	body io.ReadCloser
	dec  *json.Decoder
}

func (s *RepoRebaseStream) Recv() (*repov1.RebaseEvent, error) {
	var ev repov1.RebaseEvent
	if err := s.dec.Decode(&ev); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, io.EOF
		}
		return nil, fmt.Errorf("rebase recv: %w", err)
	}
	return &ev, nil
}

func (s *RepoRebaseStream) Close() error {
	if s.body != nil {
		if err := s.body.Close(); err != nil {
			return fmt.Errorf("close rebase stream: %w", err)
		}
	}
	return nil
}

func (c *Client) RepoRebase(ctx context.Context, repoID string, req *repov1.RebaseRequest) (*RepoRebaseStream, error) {
	resp, err := c.doRepoStream(ctx, http.MethodPost, repoID, "/rebases", "push", nil, req, "application/x-ndjson") //nolint:bodyclose // body is returned to caller via RepoRebaseStream.Close.
	if err != nil {
		return nil, fmt.Errorf("rebase: %w", err)
	}
	return &RepoRebaseStream{body: resp.Body, dec: json.NewDecoder(resp.Body)}, nil
}
