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
	"time"

	"github.com/google/uuid"
)

// Project agents are the per-project review agents entire-api runs on pushes
// under /api/v1/{host}/{project}/agents. They replaced the repo-file trail
// runners: the configuration lives in the API, versioned, with an etag write
// token for compare-and-swap replaces.

// ProjectAgentSignal is the reading each review distills to.
type ProjectAgentSignal struct {
	ValueType string `json:"value_type"`
	Polarity  string `json:"polarity,omitempty"`
}

// ProjectAgentPresetRef names the Entire preset an agent runs.
type ProjectAgentPresetRef struct {
	ID      string `json:"id"`
	Version int    `json:"version"`
}

// ProjectAgentTrigger is one saved trigger. The same shape is sent back on a
// write, so an unchanged trigger keeps its id and push history.
type ProjectAgentTrigger struct {
	ID       string   `json:"id,omitempty"`
	Kind     string   `json:"kind"`
	When     string   `json:"when,omitempty"`
	Branches []string `json:"branches,omitempty"`
	Paths    []string `json:"paths,omitempty"`
}

// ProjectAgentDefinition is what an agent is called and runs. For a preset
// agent these are the preset's, as the catalog has it now.
type ProjectAgentDefinition struct {
	Name            string             `json:"name"`
	Description     string             `json:"description"`
	Harness         string             `json:"harness"`
	Model           *string            `json:"model"`
	ReasoningEffort *string            `json:"reasoning_effort"`
	TimeoutMS       int                `json:"timeout_ms"`
	Instructions    string             `json:"instructions"`
	Signal          ProjectAgentSignal `json:"signal"`
}

// ProjectAgent is a project's agent at its current version.
type ProjectAgent struct {
	ProjectAgentDefinition

	ID            string                 `json:"id"`
	ProjectID     string                 `json:"project_id"`
	Version       int64                  `json:"version"`
	ETag          string                 `json:"etag"`
	Preset        *ProjectAgentPresetRef `json:"preset"`
	IsEnabled     bool                   `json:"is_enabled"`
	EnvironmentID string                 `json:"environment_id"`
	RepositoryIDs []string               `json:"repository_ids"`
	Triggers      []ProjectAgentTrigger  `json:"triggers"`
	// StaleRepositoryIDs are selected repositories the project no longer
	// owns. A write that keeps one is refused.
	StaleRepositoryIDs []string   `json:"stale_repository_ids"`
	ArchivedAt         *time.Time `json:"archived_at"`
}

// ProjectAgentPreset is one Entire-maintained agent from GET /agents/presets.
type ProjectAgentPreset struct {
	ID          string                 `json:"id"`
	Version     int                    `json:"version"`
	Name        string                 `json:"name"`
	Description string                 `json:"description"`
	Agent       ProjectAgentDefinition `json:"agent"`
}

// ProjectEnvironment is a project's saved sandbox environment.
type ProjectEnvironment struct {
	ID          string     `json:"id"`
	Name        string     `json:"name"`
	Description string     `json:"description"`
	ArchivedAt  *time.Time `json:"archived_at"`
}

// ProjectEnvironmentInput is an environment create body.
type ProjectEnvironmentInput struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

// ProjectAgentWrite is a create or replace body. A preset create sends Preset
// and the placement fields only; a custom agent sends every definition field
// and no Preset. A replace of a preset agent without Preset customizes it
// under the same id.
type ProjectAgentWrite struct {
	Preset      string `json:"preset,omitempty"`
	Name        string `json:"name,omitempty"`
	Description string `json:"description,omitempty"`
	// IsEnabled is always sent: the API defaults a missing value to false,
	// but a write should say what it means.
	IsEnabled       bool                  `json:"is_enabled"`
	Harness         string                `json:"harness,omitempty"`
	Model           *string               `json:"model,omitempty"`
	ReasoningEffort *string               `json:"reasoning_effort,omitempty"`
	TimeoutMS       int                   `json:"timeout_ms,omitempty"`
	Instructions    string                `json:"instructions,omitempty"`
	Signal          *ProjectAgentSignal   `json:"signal,omitempty"`
	RepositoryIDs   []string              `json:"repository_ids"`
	Triggers        []ProjectAgentTrigger `json:"triggers,omitempty"`
	EnvironmentID   string                `json:"environment_id,omitempty"`
}

// ProjectConfigPage is one page of a project configuration list.
type ProjectConfigPage[T any] struct {
	Items      []T     `json:"items"`
	NextCursor *string `json:"next_cursor,omitempty"`
}

// ProjectConfigRequest sends one request to a project configuration route
// (agents, environments, presets) and decodes the response into out. headers
// carry the write preconditions: If-Match on a replace (the etag write token of
// the last read), Idempotency-Key on a create (see CreateProjectConfig).
func (c *Client) ProjectConfigRequest(ctx context.Context, method, path string, headers http.Header, body, out any) error {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encode request: %w", err)
		}
		reader = bytes.NewReader(encoded)
	}
	resp, err := c.Request(ctx, method, path, headers, reader)
	if err != nil {
		return fmt.Errorf("request %s: %w", path, err)
	}
	defer resp.Body.Close()
	if err := CheckResponse(resp); err != nil {
		if IsHTTPErrorStatus(err, http.StatusPreconditionFailed) {
			return fmt.Errorf("it changed since it was read; run the command again: %w", err)
		}
		return err
	}
	if out != nil {
		if err := DecodeJSON(resp, out); err != nil {
			return err
		}
	}
	return nil
}

// CreateProjectConfig POSTs a configuration create. The API requires an
// Idempotency-Key on every create: a retry with the same key and body returns
// what the first attempt saved instead of saving another. Each call is one
// logical create, so it gets a fresh key.
func (c *Client) CreateProjectConfig(ctx context.Context, path string, body, out any) error {
	return c.ProjectConfigRequest(ctx, http.MethodPost, path, http.Header{"Idempotency-Key": []string{uuid.NewString()}}, body, out)
}

// ReplaceProjectConfig PUTs a configuration replace under the etag of the
// last read; a stale etag is refused rather than overwriting a newer version.
func (c *Client) ReplaceProjectConfig(ctx context.Context, path, etag string, body, out any) error {
	if etag == "" {
		return errors.New("no etag to replace it with; read it again")
	}
	return c.ProjectConfigRequest(ctx, http.MethodPut, path, http.Header{"If-Match": []string{etag}}, body, out)
}

// ListProjectConfig reads every page of a project configuration list. query
// holds the list's filters; the cursor is added per page.
func ListProjectConfig[T any](ctx context.Context, c *Client, path string, query url.Values) ([]T, error) {
	var all []T
	q := url.Values{}
	for k, v := range query {
		q[k] = v
	}
	for {
		target := path
		if len(q) > 0 {
			target += "?" + q.Encode()
		}
		var page ProjectConfigPage[T]
		if err := c.ProjectConfigRequest(ctx, http.MethodGet, target, nil, nil, &page); err != nil {
			return nil, err
		}
		all = append(all, page.Items...)
		if page.NextCursor == nil || *page.NextCursor == "" {
			return all, nil
		}
		// The cursor restores the list's filters; repeating them is refused.
		q = url.Values{"cursor": []string{*page.NextCursor}}
	}
}
