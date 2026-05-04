package recap

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strconv"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/api"
)

// MeRecapResponse mirrors GET /api/v1/me/recap.
type MeRecapResponse struct {
	Timeframe    string                `json:"timeframe"`
	Repo         *string               `json:"repo"`
	Since        string                `json:"since"`
	Until        string                `json:"until"`
	Agents       map[string]AgentEntry `json:"agents"`
	Summary      Summary               `json:"summary"`
	Contributors *ContribSummary       `json:"contributors"`
	Daily        []DailyCount          `json:"daily"`
	UpdatedAt    string                `json:"updated_at"`
}

// Summary contains top-level counts intended for CLI rendering.
type Summary struct {
	Me         SummaryTotals  `json:"me"`
	Team       *SummaryTotals `json:"team"`
	RepoCount  int            `json:"repoCount"`
	ActiveDays int            `json:"activeDays"`
	Analysis   AnalysisStatus `json:"analysis"`
}

type SummaryTotals struct {
	Sessions    int `json:"sessions"`
	Checkpoints int `json:"checkpoints"`
	Tokens      int `json:"tokens"`
}

type AnalysisStatus struct {
	Complete int `json:"complete"`
	Pending  int `json:"pending"`
	Failed   int `json:"failed"`
}

type DailyCount struct {
	Date  string `json:"date"`
	Count int    `json:"count"`
}

type AgentEntry struct {
	AgentID      string          `json:"agentId"`
	AgentLabel   string          `json:"agentLabel"`
	Me           AgentAggregate  `json:"me"`
	Contributors *AgentAggregate `json:"contributors"`
}

type AgentAggregate struct {
	Sessions         int          `json:"sessions"`
	Checkpoints      int          `json:"checkpoints"`
	Tokens           int          `json:"tokens"`
	TranscriptTokens int          `json:"transcriptTokens"`
	FilesChanged     int          `json:"filesChanged"`
	Labels           []LabelCount `json:"labels"`
	Skills           []SkillCount `json:"skills"`
	MCPServers       []McpCount   `json:"mcpServers"`
	ToolMix          ToolMix      `json:"toolMix"`
}

type LabelCount struct {
	Label string `json:"label"`
	Count int    `json:"count"`
}

type SkillCount struct {
	Skill string `json:"skill"`
	Count int    `json:"count"`
}

type McpCount struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

type ToolMix struct {
	Shell   int `json:"shell"`
	FileOps int `json:"fileOps"`
	Search  int `json:"search"`
	MCP     int `json:"mcp"`
	Agent   int `json:"agent"`
	Other   int `json:"other"`
}

type ContribSummary struct {
	DistinctUsers    int `json:"distinctUsers"`
	TotalTokens      int `json:"totalTokens"`
	TotalCheckpoints int `json:"totalCheckpoints"`
}

// FetchMeRecap fetches one server-backed recap window.
func FetchMeRecap(
	ctx context.Context,
	client *api.Client,
	since, until time.Time,
	repo string,
	limit int,
) (*MeRecapResponse, error) {
	if client == nil {
		return nil, errors.New("me/recap: nil client")
	}
	q := url.Values{}
	q.Set("since", since.UTC().Format(time.RFC3339))
	q.Set("until", until.UTC().Format(time.RFC3339))
	if repo != "" {
		q.Set("repo", repo)
	}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	resp, err := client.Get(ctx, "/api/v1/me/recap?"+q.Encode())
	if err != nil {
		return nil, fmt.Errorf("me/recap get: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body) //nolint:errcheck // best-effort error body
		return nil, fmt.Errorf("me/recap: http %d: %s", resp.StatusCode, string(body))
	}
	var out MeRecapResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("me/recap decode: %w", err)
	}
	return &out, nil
}
