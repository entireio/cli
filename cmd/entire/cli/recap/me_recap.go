package recap

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strconv"

	"github.com/entireio/cli/cmd/entire/cli/api"
)

// MeRecapResponse mirrors the shape returned by GET /api/v1/me/recap on
// entire.io. Bearer-auth, repo-scoped (optional).
//
// Mirrors api/src/routes/recap.ts + api/src/lib/recap-aggregator.ts on the
// server. Keep these types in sync when either side changes.
type MeRecapResponse struct {
	Timeframe    string                `json:"timeframe"`
	Repo         *string               `json:"repo"`
	Agents       map[string]AgentEntry `json:"agents"`
	Contributors *ContribSummary       `json:"contributors"`
	UpdatedAt    string                `json:"updated_at"`
}

// AgentEntry is one agent row with both me + contributors columns.
type AgentEntry struct {
	Me           AgentAggregate  `json:"me"`
	Contributors *AgentAggregate `json:"contributors"`
}

// AgentAggregate is the per-side per-agent payload. transcriptTokens and
// filesChanged are the deterministic "survives LLM-off" fields; labels
// require LLM analysis to be complete.
type AgentAggregate struct {
	Checkpoints      int          `json:"checkpoints"`
	Tokens           int          `json:"tokens"`
	TranscriptTokens int          `json:"transcriptTokens"`
	FilesChanged     int          `json:"filesChanged"`
	Labels           []LabelCount `json:"labels"`
	Skills           []SkillCount `json:"skills"`
	MCPServers       []McpCount   `json:"mcpServers"`
	ToolMix          ToolMix      `json:"toolMix"`
}

// SkillCount pairs a skill name with its usage count.
type SkillCount struct {
	Skill string `json:"skill"`
	Count int    `json:"count"`
}

// McpCount pairs an MCP server name with its usage count.
type McpCount struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

// ToolMix is the per-agent tool-category usage breakdown returned by
// /me/recap. Distinct from ToolProfile in model.go (which is the richer
// per-checkpoint shape from the analysis pipeline).
type ToolMix struct {
	Shell   int `json:"shell"`
	FileOps int `json:"fileOps"`
	Search  int `json:"search"`
	MCP     int `json:"mcp"`
	Agent   int `json:"agent"`
	Other   int `json:"other"`
}

// ContribSummary is the repo-wide rollup returned alongside per-agent
// data when ?repo= is set. Nil when no repo was specified.
type ContribSummary struct {
	DistinctUsers    int `json:"distinctUsers"`
	TotalTokens      int `json:"totalTokens"`
	TotalCheckpoints int `json:"totalCheckpoints"`
}

// FetchMeRecap calls GET /api/v1/me/recap?timeframe=&repo=&limit= and
// returns the decoded response. Any error (network, non-2xx, decode)
// surfaces to the caller; this is the "fast path" that replaces the old
// commits + batch-analyses + overview fetch stack once the server endpoint
// is deployed.
//
// timeframe is one of the values the server accepts ("last-month",
// "last-3-months", "last-6-months"). repo is "org/name" or empty.
func FetchMeRecap(
	ctx context.Context,
	client *api.Client,
	timeframe, repo string,
	limit int,
) (*MeRecapResponse, error) {
	if client == nil {
		return nil, errors.New("me/recap: nil client")
	}
	q := url.Values{}
	if timeframe != "" {
		q.Set("timeframe", timeframe)
	}
	if repo != "" {
		q.Set("repo", repo)
	}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	path := "/api/v1/me/recap"
	if encoded := q.Encode(); encoded != "" {
		path += "?" + encoded
	}
	resp, err := client.Get(ctx, path)
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

// TimeframeForRange maps our RangeKey to the timeframe value the server
// endpoint accepts. The server only supports last-month / last-3-months /
// last-6-months today; day / week / month are rounded up to last-month,
// 90d maps to last-3-months.
func TimeframeForRange(r RangeKey) string {
	switch r {
	case RangeDay, RangeWeek, Range30d, RangeMonth:
		return "last-month"
	case Range90d:
		return "last-3-months"
	}
	return "last-month"
}

// ContributorsFromMeRecap converts a MeRecapResponse into the
// ContributorsData shape the rest of the recap package consumes. This
// lets us drop in the new endpoint without changing downstream rendering.
func ContributorsFromMeRecap(resp *MeRecapResponse) *ContributorsData {
	if resp == nil || len(resp.Agents) == 0 {
		return nil
	}
	out := &ContributorsData{ByAgent: map[string]*AgentContrib{}}
	if resp.Repo != nil {
		out.Repo = *resp.Repo
	}
	for name, entry := range resp.Agents {
		if entry.Contributors == nil {
			continue
		}
		c := entry.Contributors
		distinctContribs := 0
		if resp.Contributors != nil {
			distinctContribs = resp.Contributors.DistinctUsers
		}
		agg := &AgentContrib{
			TotalCount:       c.Checkpoints,
			Tokens:           c.Tokens,
			DistinctContribs: distinctContribs,
			Skills:           flattenSkills(c.Skills),
			MCPServers:       flattenMCP(c.MCPServers),
			ToolMix:          toolMixToMap(c.ToolMix),
		}
		agg.Labels = append(agg.Labels, c.Labels...)
		out.ByAgent[name] = agg
	}
	return out
}

func flattenSkills(xs []SkillCount) []string {
	out := make([]string, 0, len(xs))
	for _, x := range xs {
		out = append(out, x.Skill)
	}
	if len(out) > 3 {
		out = out[:3]
	}
	return out
}

func flattenMCP(xs []McpCount) []string {
	out := make([]string, 0, len(xs))
	for _, x := range xs {
		out = append(out, x.Name)
	}
	if len(out) > 3 {
		out = out[:3]
	}
	return out
}

func toolMixToMap(t ToolMix) map[string]int {
	return map[string]int{
		"shell":   t.Shell,
		"fileOps": t.FileOps,
		"search":  t.Search,
		"mcp":     t.MCP,
		"agent":   t.Agent,
		"other":   t.Other,
	}
}
