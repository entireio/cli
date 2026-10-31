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
)

// A repository's gates are the checks entire-api runs before a trail can
// merge, under /api/v1/repos/{repo_id}/trails/gates. Eval gates (one per
// agent, keyed by the agent id) are saved as a whole set through
// .../trails/gates/evals; every other gate is edited one at a time.

// EvalSignal is an agent's eval: its value type (percent, size, boolean) and
// which direction is better (higher_is_better, lower_is_better, neutral).
type EvalSignal struct {
	ValueType string `json:"value_type"`
	Polarity  string `json:"polarity"`
}

// RepoGateDefinition is one gate as GET .../trails/gates lists it.
type RepoGateDefinition struct {
	ID       string `json:"id"`
	GateKey  string `json:"gateKey"`
	GateType string `json:"gateType"`
	Blocking bool   `json:"blocking"`
	Enabled  bool   `json:"enabled"`
}

// RepoGateConfig is a repository's gates. Raw holds each gate as the server
// sent it, for --json.
type RepoGateConfig struct {
	Gates        []RepoGateDefinition
	Raw          []json.RawMessage
	GatesEnabled bool
}

// RepoEvalGateAgent is an eval gate's agent as it is now.
type RepoEvalGateAgent struct {
	Name      string     `json:"name"`
	IsEnabled bool       `json:"is_enabled"`
	Signal    EvalSignal `json:"signal"`
}

// RepoEvalGate is one eval gate. Signal is the eval its threshold was set
// against; Agent is nil when the agent is archived or no longer runs on the
// repository.
type RepoEvalGate struct {
	ID                 string                    `json:"id"`
	AgentID            string                    `json:"agent_id"`
	AgentName          string                    `json:"agent_name"`
	Threshold          json.RawMessage           `json:"threshold"`
	Enabled            bool                      `json:"enabled"`
	Blocking           bool                      `json:"blocking"`
	Signal             *EvalSignal               `json:"signal"`
	Agent              *RepoEvalGateAgent        `json:"agent"`
	ConfigurationIssue *TrailEvalGateConfigIssue `json:"configuration_issue"`
}

// RepoEvalGates is a repository's eval gate set. ETag covers the whole set
// and guards the next replace.
type RepoEvalGates struct {
	Items []RepoEvalGate
	Raw   []json.RawMessage
	ETag  string
}

// RepoEvalGateInput is one entry of a replace. An entry whose threshold is
// unchanged and that sends no Signal keeps the eval it was set against.
// Signal confirms the eval a new threshold was checked against; the server
// refuses the write when the agent's eval differs.
type RepoEvalGateInput struct {
	AgentID   string          `json:"agent_id"`
	Threshold json.RawMessage `json:"threshold,omitempty"`
	Enabled   bool            `json:"enabled"`
	Blocking  bool            `json:"blocking"`
	Signal    *EvalSignal     `json:"signal,omitempty"`
}

// RepoGatePatch changes one gate; nil fields stay as they are.
type RepoGatePatch struct {
	Enabled  *bool `json:"enabled,omitempty"`
	Blocking *bool `json:"blocking,omitempty"`
}

// RepoAgent is an agent that runs on the repository.
type RepoAgent struct {
	ID        string     `json:"id"`
	Name      string     `json:"name"`
	IsEnabled bool       `json:"is_enabled"`
	Signal    EvalSignal `json:"signal"`
}

func repoGatesPath(repoID string) string {
	return "/api/v1/repos/" + url.PathEscape(repoID) + "/trails/gates"
}

// RepoGateConfig reads the repository's gates, disabled ones included.
func (c *Client) RepoGateConfig(ctx context.Context, repoID string) (*RepoGateConfig, error) {
	var body struct {
		Gates        []json.RawMessage `json:"gates"`
		GatesEnabled bool              `json:"gatesEnabled"`
	}
	if _, err := c.repoGatesRequest(ctx, http.MethodGet, repoGatesPath(repoID), nil, nil, &body); err != nil {
		return nil, err
	}
	out := &RepoGateConfig{Raw: body.Gates, GatesEnabled: body.GatesEnabled, Gates: make([]RepoGateDefinition, len(body.Gates))}
	for i, raw := range body.Gates {
		if err := json.Unmarshal(raw, &out.Gates[i]); err != nil {
			return nil, fmt.Errorf("decode gate: %w", err)
		}
	}
	return out, nil
}

// RepoEvalGates reads the repository's eval gates and the ETag of the set.
func (c *Client) RepoEvalGates(ctx context.Context, repoID string) (*RepoEvalGates, error) {
	return c.evalGatesRequest(ctx, http.MethodGet, repoID, nil, nil)
}

// ReplaceRepoEvalGates saves the repository's complete eval gate set under
// the ETag of the read it was built from: gates of agents left out are
// deleted. A stale ETag is a 412 (IsHTTPErrorStatus).
func (c *Client) ReplaceRepoEvalGates(ctx context.Context, repoID, etag string, items []RepoEvalGateInput) (*RepoEvalGates, error) {
	if etag == "" {
		return nil, errors.New("the eval gates were read without an ETag; read them again")
	}
	if items == nil {
		items = []RepoEvalGateInput{}
	}
	body := struct {
		Items []RepoEvalGateInput `json:"items"`
	}{items}
	return c.evalGatesRequest(ctx, http.MethodPut, repoID, http.Header{"If-Match": []string{etag}}, body)
}

func (c *Client) evalGatesRequest(ctx context.Context, method, repoID string, headers http.Header, body any) (*RepoEvalGates, error) {
	var resp struct {
		Items []json.RawMessage `json:"items"`
	}
	etag, err := c.repoGatesRequest(ctx, method, repoGatesPath(repoID)+"/evals", headers, body, &resp)
	if err != nil {
		return nil, err
	}
	out := &RepoEvalGates{Raw: resp.Items, ETag: etag, Items: make([]RepoEvalGate, len(resp.Items))}
	if out.Raw == nil {
		out.Raw = []json.RawMessage{}
	}
	for i, raw := range resp.Items {
		if err := json.Unmarshal(raw, &out.Items[i]); err != nil {
			return nil, fmt.Errorf("decode eval gate: %w", err)
		}
	}
	return out, nil
}

// PatchRepoGate changes one gate and returns it as the server sent it.
func (c *Client) PatchRepoGate(ctx context.Context, repoID, gateID string, patch RepoGatePatch) (json.RawMessage, error) {
	var resp struct {
		Gate json.RawMessage `json:"gate"`
	}
	if _, err := c.repoGatesRequest(ctx, http.MethodPatch, repoGatesPath(repoID)+"/"+url.PathEscape(gateID), nil, patch, &resp); err != nil {
		return nil, err
	}
	return resp.Gate, nil
}

// RepoAgents lists the active agents of the repository's project that run on
// it.
func (c *Client) RepoAgents(ctx context.Context, repoID string) ([]RepoAgent, error) {
	var resp struct {
		Items []RepoAgent `json:"items"`
	}
	if _, err := c.repoGatesRequest(ctx, http.MethodGet, "/api/v1/repos/"+url.PathEscape(repoID)+"/agents", nil, nil, &resp); err != nil {
		return nil, err
	}
	return resp.Items, nil
}

// repoGatesRequest sends one request and decodes the response into out. It
// returns the response's ETag, and an error that keeps the *HTTPError so a
// caller can tell a 412 apart.
func (c *Client) repoGatesRequest(ctx context.Context, method, path string, headers http.Header, body, out any) (string, error) {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return "", fmt.Errorf("encode request: %w", err)
		}
		reader = bytes.NewReader(encoded)
	}
	resp, err := c.Request(ctx, method, path, headers, reader)
	if err != nil {
		return "", fmt.Errorf("request %s: %w", path, err)
	}
	defer resp.Body.Close()
	if err := CheckResponse(resp); err != nil {
		return "", err
	}
	if out != nil {
		if err := DecodeJSON(resp, out); err != nil {
			return "", err
		}
	}
	return resp.Header.Get("ETag"), nil
}
