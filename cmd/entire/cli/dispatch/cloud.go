package dispatch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/versioninfo"
)

type CloudConfig struct {
	BaseURL string
	Token   string
	HTTP    *http.Client
	Timeout time.Duration
}

type CloudClient struct {
	baseURL string
	token   string
	http    *http.Client
}

const defaultCloudHTTPTimeout = 120 * time.Second

// The gateway's one-shot dispatch route (generated in-request, nothing
// persisted). `?jurisdiction=` is the gateway-only selector for which
// jurisdiction's cell generates it — see Options.Jurisdiction.
const (
	dispatchGeneratePath   = "/api/v1/dispatches/generate"
	jurisdictionQueryParam = "jurisdiction"
)

func NewCloudClient(cfg CloudConfig) *CloudClient {
	baseURL := cfg.BaseURL
	if baseURL == "" {
		baseURL = api.BaseURL()
	}

	httpClient := cfg.HTTP
	if httpClient == nil {
		timeout := cfg.Timeout
		if timeout <= 0 {
			timeout = defaultCloudHTTPTimeout
		}
		httpClient = &http.Client{Timeout: timeout}
	} else if cfg.Timeout > 0 && httpClient.Timeout == 0 {
		httpClient.Timeout = cfg.Timeout
	}

	return &CloudClient{
		baseURL: strings.TrimRight(baseURL, "/"),
		token:   cfg.Token,
		http:    httpClient,
	}
}

type CreateDispatchRequest struct {
	Repos    []string `json:"repos,omitempty"`
	Since    string   `json:"since"`
	Until    string   `json:"until"`
	Generate bool     `json:"generate"`
	Voice    string   `json:"voice,omitempty"`
}

type CreateDispatchResponse struct {
	// Jurisdiction is the slug the gateway stamps onto the response: the
	// jurisdiction whose cell the dispatch was generated from. Empty from a
	// gateway that predates the selector — see runServer for how a sent
	// selector is checked against it.
	Jurisdiction      string      `json:"jurisdiction,omitempty"`
	Window            APIWindow   `json:"window"`
	Title             string      `json:"title,omitempty"`
	CoveredRepos      []string    `json:"covered_repos,omitempty"`
	Branches          APIBranches `json:"branches,omitempty"`
	Voice             *string     `json:"voice"`
	Repos             []APIRepo   `json:"repos,omitempty"`
	Totals            APITotals   `json:"totals"`
	Warnings          APIWarnings `json:"warnings"`
	GeneratedText     string      `json:"generated_text,omitempty"`
	GeneratedMarkdown string      `json:"generated_markdown,omitempty"`
}

type APIBranches struct {
	Values []string
	All    bool
}

type APIWindow struct {
	NormalizedSince          string `json:"normalized_since"`
	NormalizedUntil          string `json:"normalized_until"`
	FirstCheckpointCreatedAt string `json:"first_checkpoint_created_at,omitempty"`
	LastCheckpointCreatedAt  string `json:"last_checkpoint_created_at,omitempty"`
}

type APIRepo struct {
	FullName string       `json:"full_name"`
	URL      string       `json:"url,omitempty"`
	Sections []APISection `json:"sections"`
}

type APISection struct {
	Label   string      `json:"label"`
	Bullets []APIBullet `json:"bullets"`
}

type APIBullet struct {
	CheckpointID string   `json:"checkpoint_id"`
	Text         string   `json:"text"`
	Source       string   `json:"source"`
	Branch       string   `json:"branch"`
	CreatedAt    string   `json:"created_at"`
	Labels       []string `json:"labels"`
}

type APITotals struct {
	Checkpoints         int `json:"checkpoints"`
	UsedCheckpointCount int `json:"used_checkpoint_count"`
	Branches            int `json:"branches"`
	FilesTouched        int `json:"files_touched"`
}

type APIWarnings struct {
	AccessDeniedCount  int `json:"access_denied_count"`
	PendingCount       int `json:"pending_count"`
	FailedCount        int `json:"failed_count"`
	UnknownCount       int `json:"unknown_count"`
	UncategorizedCount int `json:"uncategorized_count"`
	TruncatedCount     int `json:"truncated_count"`
}

func (b *APIBranches) UnmarshalJSON(data []byte) error {
	if bytes.Equal(data, []byte("null")) {
		*b = APIBranches{}
		return nil
	}

	var values []string
	if err := json.Unmarshal(data, &values); err == nil {
		*b = APIBranches{Values: values}
		return nil
	}

	var sentinel string
	if err := json.Unmarshal(data, &sentinel); err != nil {
		return fmt.Errorf("decode branches: %w", err)
	}
	if sentinel != "all" {
		return fmt.Errorf("decode branches: unexpected sentinel %q", sentinel)
	}
	*b = APIBranches{All: true}
	return nil
}

// RepoNotFoundError is the gateway's 404 for a repo that is not placed in (or
// not visible in) the jurisdiction the request was routed to — a repo mirrored
// only in US, requested from an AU home, is simply unknown to the AU cell.
// Jurisdiction is the selector the caller sent ("" = home) and Home the
// caller's home jurisdiction when known (so callers know which cell answered
// when no selector was sent); Repos are the requested slugs the gateway's
// message named; Message is its sentence; Cause is the underlying
// *api.HTTPError.
type RepoNotFoundError struct {
	Jurisdiction string
	Home         string
	Repos        []string
	Message      string
	Cause        error
}

// FailedJurisdiction is the jurisdiction whose cell answered "not found": the
// selector when one was sent, else the home jurisdiction ("" if unknown).
func (e *RepoNotFoundError) FailedJurisdiction() string {
	if j := strings.TrimSpace(e.Jurisdiction); j != "" {
		return j
	}
	return e.Home
}

const repoNotFoundPrefix = "repository not found"

func (e *RepoNotFoundError) Error() string {
	scope := "your home jurisdiction"
	if j := strings.TrimSpace(e.Jurisdiction); j != "" {
		scope = strings.ToUpper(j)
	}
	// Render our own sentence when the repos were parsed, so the wording does
	// not depend on (or double up with) the gateway's prose; fall back to its
	// message only when parsing found nothing.
	sentence := e.Message
	if len(e.Repos) > 0 {
		sentence = repoNotFoundPrefix + ": " + strings.Join(e.Repos, ", ")
	}
	return "In " + scope + ": " + sentence + ". Pick a jurisdiction the repository is mirrored into (entire dispatch --jurisdiction <slug>), or mirror it there."
}

func (e *RepoNotFoundError) Unwrap() error { return e.Cause }

// statusError keeps the dispatch command's established non-2xx wording while
// exposing the shared *api.HTTPError underneath (errors.As /
// api.IsHTTPErrorStatus), so dispatch failures classify like any other API call.
type statusError struct {
	*api.HTTPError
}

func (e *statusError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("dispatch service returned status %d", e.StatusCode)
	}
	return fmt.Sprintf("dispatch service returned status %d: %s", e.StatusCode, strconv.Quote(e.Message))
}

func (e *statusError) Unwrap() error { return e.HTTPError }

// CreateDispatch generates a one-off dispatch from the cell of `jurisdiction`
// ("" = the caller's home jurisdiction).
func (c *CloudClient) CreateDispatch(ctx context.Context, reqBody CreateDispatchRequest, jurisdiction string) (*CreateDispatchResponse, error) {
	path := dispatchGeneratePath
	if j := strings.TrimSpace(jurisdiction); j != "" {
		path += "?" + jurisdictionQueryParam + "=" + url.QueryEscape(j)
	}

	var out CreateDispatchResponse
	if err := c.doJSON(ctx, http.MethodPost, path, reqBody, &out); err != nil {
		var httpErr *api.HTTPError
		if errors.As(err, &httpErr) && httpErr.StatusCode == http.StatusNotFound &&
			strings.HasPrefix(strings.ToLower(httpErr.Message), repoNotFoundPrefix) {
			return nil, &RepoNotFoundError{
				Jurisdiction: jurisdiction,
				Repos:        parseNotFoundRepos(httpErr.Message, reqBody.Repos),
				Message:      httpErr.Message,
				Cause:        err,
			}
		}
		return nil, err
	}
	return &out, nil
}

// parseNotFoundRepos pulls the slugs out of a "repository not found: a/b, c/d"
// message, keeping only the repos this request asked for (in the request's
// spelling) so downstream lookups are bounded by CloudRepoLimit and never fan
// out over arbitrary prose. Best-effort: an unexpected format yields nil and
// the caller still has the message.
func parseNotFoundRepos(message string, requested []string) []string {
	_, rest, ok := strings.Cut(message, ":")
	if !ok {
		return nil
	}
	named := normalizeScopeValues(strings.Split(rest, ","))
	var repos []string
	for _, repo := range requested {
		for _, candidate := range named {
			if strings.EqualFold(candidate, repo) {
				repos = append(repos, repo)
				break
			}
		}
	}
	return repos
}

func (c *CloudClient) doJSON(ctx context.Context, method, path string, reqBody, out any) error {
	var body io.Reader
	if reqBody != nil {
		data, err := json.Marshal(reqBody)
		if err != nil {
			return fmt.Errorf("marshal request body: %w", err)
		}
		body = bytes.NewReader(data)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", versioninfo.UserAgent())
	if reqBody != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized {
		return errors.New("dispatch requires login — run `entire login`")
	}
	if checkErr := api.CheckResponse(resp); checkErr != nil {
		logging.Warn(ctx, "dispatch request failed", "method", method, "path", path, "status_code", resp.StatusCode)
		httpErr, ok := checkErr.(*api.HTTPError) //nolint:errorlint // CheckResponse returns the concrete type unwrapped
		if !ok {
			return fmt.Errorf("dispatch service: %w", checkErr)
		}
		return &statusError{HTTPError: httpErr}
	}
	if out == nil {
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}
