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

// MeRecapResponse mirrors the shape returned by GET /api/v1/me/recap on
// entire.io. Bearer-auth, repo-scoped (optional). Keep in sync with the
// zod schema at api/src/routes/recap.ts when either side changes.
type MeRecapResponse struct {
	Timeframe    string                `json:"timeframe"`
	Repo         *string               `json:"repo"`
	Since        string                `json:"since"`
	Until        string                `json:"until"`
	Agents       map[string]AgentEntry `json:"agents"`
	Contributors *ContribSummary       `json:"contributors"`
	Daily        []DailyCount          `json:"daily"`
	UpdatedAt    string                `json:"updated_at"`
}

// DailyCount is one entry in the response's daily activity array — one per
// day in the window, with zero for days the user had no checkpoints.
// Powers the CLI's activity strip (cross-repo sum of user's work per day).
type DailyCount struct {
	Date  string `json:"date"`
	Count int    `json:"count"`
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

// FetchMeRecap calls GET /api/v1/me/recap?since=&until=&repo=&limit=
// and returns the decoded response. The server accepts explicit ISO 8601
// date bounds so the CLI's range math (--day / --week / --month / --90)
// lines up exactly with the server's aggregation window. repo is
// "org/name" or empty; when empty the response spans every repo the
// user has activity in.
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
	path := "/api/v1/me/recap?" + q.Encode()
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

// ContributorsFromMeRecap converts the contributors side of a MeRecapResponse
// into the ContributorsData shape the rest of the recap package consumes.
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

// MeFromMeRecap extracts the me-side of a MeRecapResponse — the server's
// authoritative view of THIS user's work, used to override the CLI's
// local-only numbers so CLI and entire.io dashboard show the same counts.
// Returns nil if the response has no agent data at all.
//
// Fields we pull from server (authoritative): checkpoints, tokens,
// transcriptTokens, filesChanged, labels, skills, mcpServers, toolMix.
// Fields that stay local-only (server doesn't track them): sessions,
// models, repos — see applyServerMe in agents_card.go.
func MeFromMeRecap(resp *MeRecapResponse) *ContributorsData {
	if resp == nil || len(resp.Agents) == 0 {
		return nil
	}
	out := &ContributorsData{ByAgent: map[string]*AgentContrib{}}
	if resp.Repo != nil {
		out.Repo = *resp.Repo
	}
	for name, entry := range resp.Agents {
		m := entry.Me
		agg := &AgentContrib{
			TotalCount: m.Checkpoints, // reused field — represents checkpoints here
			Tokens:     m.Tokens,
			Skills:     flattenSkills(m.Skills),
			MCPServers: flattenMCP(m.MCPServers),
			ToolMix:    toolMixToMap(m.ToolMix),
		}
		agg.Labels = append(agg.Labels, m.Labels...)
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
