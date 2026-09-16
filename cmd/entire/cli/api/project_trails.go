package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"
)

// TrailParentReference is navigation returned by repository Change reads.
// Absence can mean inaccessible or unresolved, not necessarily parentless.
// Path is relative to the PROJECT cell, never the repository's cell.
type TrailParentReference struct {
	ID                    string `json:"id"`
	Number                int    `json:"number"`
	ProjectID             string `json:"projectId"`
	Host                  string `json:"host"`
	Project               string `json:"project"`
	Path                  string `json:"path"`
	Jurisdiction          string `json:"jurisdiction"`
	PrimaryProcessingCell string `json:"primaryProcessingCell"`
}

// ProjectTrail is project intent, separate from branch-backed TrailResource.
// Changes is absent from list responses. A detail is potentially access-filtered;
// neither its changes nor its repoIds are an authoritative complete inventory.
type ProjectTrail struct {
	ID                 string          `json:"id"`
	ProjectID          string          `json:"projectId"`
	Number             int             `json:"number"`
	Title              string          `json:"title"`
	Body               string          `json:"body"`
	Status             string          `json:"status"`
	Metadata           map[string]any  `json:"metadata"`
	Type               string          `json:"type"`
	Priority           string          `json:"priority"`
	Assignees          []string        `json:"assignees"`
	AssigneeAccountIDs []*string       `json:"assigneeAccountIds"`
	RepositoryIDs      []string        `json:"repoIds"`
	AuthorAccountID    string          `json:"authorAccountId"`
	CreatedAt          time.Time       `json:"createdAt"`
	UpdatedAt          time.Time       `json:"updatedAt"`
	ClosedAt           *time.Time      `json:"closedAt"`
	Changes            []ChangeSummary `json:"changes,omitempty"`
	IsPossiblyPartial  bool            `json:"isPossiblyPartial,omitempty"`
}

type ChangeSummary struct {
	ID             string    `json:"id"`
	RepositoryID   string    `json:"repositoryId"`
	Repository     string    `json:"repository"`
	Branch         string    `json:"branch"`
	Number         int       `json:"number"`
	Title          string    `json:"title"`
	Status         string    `json:"status"`
	HasCodeChanges bool      `json:"hasCodeChanges"`
	CreatedAt      time.Time `json:"createdAt"`
	UpdatedAt      time.Time `json:"updatedAt"`
}

type ProjectTrailListResponse struct {
	Items         []ProjectTrail `json:"items"`
	NextPageToken *string        `json:"nextPageToken"`
}

type ProjectTrailCreateRequest struct {
	Title         string                `json:"title"`
	Body          string                `json:"body,omitempty"`
	Status        string                `json:"status,omitempty"`
	Type          string                `json:"type,omitempty"`
	Priority      string                `json:"priority,omitempty"`
	Assignees     []string              `json:"assignees,omitempty"`
	RepositoryIDs []string              `json:"repoIds,omitempty"`
	Changes       []ChangeCreateRequest `json:"changes,omitempty"`
}

type ChangeCreateRequest struct {
	TrailCreateRequest

	RepositoryID string `json:"repositoryId"`
}

type ChangeCreateResponse struct {
	ID           string `json:"id"`
	TrailID      string `json:"trailId"`
	RepositoryID string `json:"repositoryId"`
}

// ProjectTrailUpdateRequest can update the description and intent atomically.
// Pointers preserve explicit clears, unlike omitempty on a plain string/slice.
type ProjectTrailUpdateRequest struct {
	Title     *string   `json:"title,omitempty"`
	Body      *string   `json:"body,omitempty"`
	Status    *string   `json:"status,omitempty"`
	Type      *string   `json:"type,omitempty"`
	Priority  *string   `json:"priority,omitempty"`
	Assignees *[]string `json:"assignees,omitempty"`
}

// ProjectTrailRequest decodes the unwrapped project API response and preserves
// the HTTP ETag (timestamp text in the body loses precision). It never retries
// writes or downgrades a failed conditional write to an unconditional one.
func (c *Client) ProjectTrailRequest(ctx context.Context, method, path string, body any, headers http.Header, out any) (string, error) {
	var encoded []byte
	if body != nil {
		var err error
		encoded, err = json.Marshal(body)
		if err != nil {
			return "", fmt.Errorf("encode project trail request: %w", err)
		}
	}
	resp, err := c.Request(ctx, method, path, headers, bytes.NewReader(encoded))
	if err != nil {
		return "", fmt.Errorf("project trail request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusPreconditionFailed {
		return "", errors.New("project trail changed since it was read; read it again before retrying")
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		var problem ErrorResponse
		if err := DecodeJSON(resp, &problem); err != nil {
			return "", fmt.Errorf("project trail request: HTTP %d (invalid error response): %w", resp.StatusCode, err)
		}
		return "", fmt.Errorf("project trail request: HTTP %d %s", resp.StatusCode, problem.Message())
	}
	if out != nil {
		if err := DecodeJSON(resp, out); err != nil {
			return "", fmt.Errorf("decode project trail: %w", err)
		}
	}
	return resp.Header.Get("ETag"), nil
}
