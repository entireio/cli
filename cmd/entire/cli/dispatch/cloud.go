package dispatch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/cmd/entire/cli/logging"
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

func (c *CloudClient) CreateDispatch(ctx context.Context, reqBody CreateDispatchRequest) (*CreateDispatchResponse, error) {
	var out CreateDispatchResponse
	if err := c.doJSON(ctx, http.MethodPost, "/api/v1/dispatches/generate", reqBody, &out); err != nil {
		return nil, err
	}
	return &out, nil
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
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20)) //nolint:errcheck // best-effort body read for error message
		trimmed := strings.TrimSpace(string(body))
		logging.Warn(ctx, "dispatch request failed", "method", method, "path", path, "status_code", resp.StatusCode)
		if trimmed == "" {
			return fmt.Errorf("dispatch service returned status %d", resp.StatusCode)
		}
		return fmt.Errorf("dispatch service returned status %d: %s", resp.StatusCode, strconv.Quote(trimmed))
	}
	if out == nil {
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}
