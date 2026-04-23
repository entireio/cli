package recap

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/cmd/entire/cli/logging"
)

// ContributorsData is the per-agent roll-up fetched from the entire.io
// repo-overview endpoints. Keyed by canonical agent name (e.g. "claude-code").
// Empty map = no data (not logged in, repo not tracked, or empty window).
type ContributorsData struct {
	Repo    string
	ByAgent map[string]*AgentContrib
}

// AgentContrib is what the contributors column shows per agent. Mirrors the
// shape of the "me" side of AgentCard but populated from server data.
//
// The "breadth" fields (Labels, Skills, MCPServers, ToolMix) are aggregated
// client-side by collecting every checkpoint ID in the repo for the range,
// batch-fetching their analyses, and grouping by the agent that produced
// each checkpoint. No per-agent aggregation exists server-side for these.
type AgentContrib struct {
	TotalCount       int // activity events from agent-activity daily count
	Tokens           int
	DistinctContribs int

	Labels     []LabelCount
	Skills     []string
	MCPServers []string
	ToolMix    map[string]int // category (shell/fileOps/…) → count
}

// FetchContributors pulls the three repo-overview endpoints in parallel and
// merges them into a per-agent summary. Any endpoint failure degrades to an
// empty result — this is optional enrichment, never a hard error.
//
// The repo argument is "<owner>/<name>" as resolved by ResolveRepoFromWorktree.
// Returns (nil, nil) for an empty repo string so the command can pass whatever
// ResolveRepoFromWorktree returned without a pre-check.
func FetchContributors(
	ctx context.Context,
	client *api.Client,
	repo string,
	start, end time.Time,
) (*ContributorsData, error) {
	if client == nil || repo == "" || repo == repoUnknown {
		return nil, nil //nolint:nilnil // optional data; no-op call is a valid state
	}
	parts := strings.SplitN(repo, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return nil, fmt.Errorf("contributors: invalid repo %q (want \"org/name\")", repo)
	}
	org, name := url.PathEscape(parts[0]), url.PathEscape(parts[1])
	params := fmt.Sprintf("?since=%s&until=%s",
		url.QueryEscape(start.Format(time.RFC3339)),
		url.QueryEscape(end.Format(time.RFC3339)))
	// Routes are mounted server-side at /api/v1/cache/:org/:repo/overview/*
	// (see api/src/app.ts: v1.route("/cache", cacheRoutes), then
	// cacheRoutes.route("/", overviewRoutes)). Missing this prefix yielded
	// silent 404s that surfaced as "repo may not be tracked" in v1.
	base := fmt.Sprintf("/api/v1/cache/%s/%s/overview", org, name)

	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		agentAct *agentActivityResponse
		contribA *contributorAgentsResponse
		contribT *contributorTokensResponse
		firstErr error
	)
	set := func(err error) {
		mu.Lock()
		defer mu.Unlock()
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}

	var commitsResp *commitsResponse

	wg.Add(4)
	go func() {
		defer wg.Done()
		// /commits returns commits + their checkpoints (with agent field),
		// which we then batch into /checkpoints/analyses/batch to get labels,
		// skills, MCP servers, and tool profile per agent.
		out, err := fetchJSON[commitsResponse](ctx, client,
			fmt.Sprintf("/api/v1/cache/%s/%s/commits?branch=main&since=%s&until=%s",
				org, name,
				url.QueryEscape(start.Format(time.RFC3339)),
				url.QueryEscape(end.Format(time.RFC3339))))
		mu.Lock()
		commitsResp = out
		mu.Unlock()
		set(err)
	}()
	go func() {
		defer wg.Done()
		out, err := fetchJSON[agentActivityResponse](ctx, client, base+"/agent-activity"+params)
		mu.Lock()
		agentAct = out
		mu.Unlock()
		set(err)
	}()
	go func() {
		defer wg.Done()
		out, err := fetchJSON[contributorAgentsResponse](ctx, client, base+"/contributor-agents"+params)
		mu.Lock()
		contribA = out
		mu.Unlock()
		set(err)
	}()
	go func() {
		defer wg.Done()
		out, err := fetchJSON[contributorTokensResponse](ctx, client, base+"/contributor-tokens"+params)
		mu.Lock()
		contribT = out
		mu.Unlock()
		set(err)
	}()
	wg.Wait()

	if firstErr != nil {
		return nil, firstErr
	}

	// Second-stage: batch-fetch analyses for every checkpoint we got via
	// /commits. Aggregate labels/skills/MCP/toolProfile by agent. Runs after
	// the first parallel wave so we know which checkpoint IDs to request.
	analysesByAgent := fetchAgentAnalysisAggregates(ctx, client, repo, commitsResp)

	return mergeContributors(repo, agentAct, contribA, contribT, analysesByAgent), nil
}

// fetchAgentAnalysisAggregates collects checkpoint IDs by agent from the
// /commits response, batch-fetches their analyses via
// POST /api/v1/cache/checkpoints/analyses/batch, and returns a per-agent
// aggregate of labels, skills, MCP servers, and tool-profile categories.
//
// Batch size is capped server-side at 200, so we chunk. A failed chunk logs
// at debug and skips forward — partial aggregates are better than nothing.
func fetchAgentAnalysisAggregates(
	ctx context.Context,
	client *api.Client,
	repo string,
	commits *commitsResponse,
) map[string]*agentAnalysisAgg {
	out := map[string]*agentAnalysisAgg{}
	if commits == nil {
		return out
	}

	// Map each checkpoint ID to its producing agent.
	agentByCP := map[string]string{}
	for _, c := range commits.Commits {
		for _, cp := range c.Checkpoints {
			if cp.CheckpointID == "" || cp.Agent == "" {
				continue
			}
			agentByCP[cp.CheckpointID] = cp.Agent
		}
	}
	if len(agentByCP) == 0 {
		return out
	}

	ids := make([]string, 0, len(agentByCP))
	for id := range agentByCP {
		ids = append(ids, id)
	}

	const batchSize = 200
	for start := 0; start < len(ids); start += batchSize {
		end := start + batchSize
		if end > len(ids) {
			end = len(ids)
		}
		batch := ids[start:end]

		resp, err := postBatchAnalyses(ctx, client, repo, batch)
		if err != nil {
			logging.Debug(ctx, "recap: batch analyses failed", "count", len(batch), "error", err.Error())
			continue
		}
		for cpID, a := range resp.Analyses {
			if a == nil || a.Status != AnalysisStatusComplete {
				continue
			}
			agent := agentByCP[cpID]
			if agent == "" {
				continue
			}
			agg, ok := out[agent]
			if !ok {
				agg = &agentAnalysisAgg{
					labels:  map[string]int{},
					skills:  map[string]int{},
					mcp:     map[string]int{},
					toolMix: map[string]int{},
				}
				out[agent] = agg
			}
			for _, lbl := range a.Extraction.Labels {
				agg.labels[lbl]++
			}
			for _, s := range a.SkillsUsed {
				agg.skills[s]++
			}
			for _, m := range a.MCPServersUsed {
				agg.mcp[m.Name] += m.Count
			}
			if a.ToolProfile != nil {
				for k, v := range a.ToolProfile.Categories {
					agg.toolMix[k] += v.Count
				}
			}
		}
	}
	return out
}

func postBatchAnalyses(ctx context.Context, client *api.Client, repo string, ids []string) (*batchAnalysesResponse, error) {
	body := map[string]any{"checkpointIds": ids, "repoFullName": repo}
	resp, err := client.Post(ctx, "/api/v1/cache/checkpoints/analyses/batch", body)
	if err != nil {
		return nil, fmt.Errorf("batch analyses post: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("batch analyses: http %d", resp.StatusCode)
	}
	var out batchAnalysesResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("batch analyses decode: %w", err)
	}
	return &out, nil
}

// agentAnalysisAgg is the per-agent tally built up during batch aggregation.
// Promoted to AgentContrib fields by mergeContributors.
type agentAnalysisAgg struct {
	labels  map[string]int
	skills  map[string]int
	mcp     map[string]int
	toolMix map[string]int
}

// mergeContributors collapses the overview responses into the per-agent
// AgentContrib map. Missing inputs degrade to zero-valued fields rather than
// dropping the agent — a card with "7 sessions · — tokens · 3 contributors"
// is better than silently omitting the agent.
func mergeContributors(
	repo string,
	act *agentActivityResponse,
	agents *contributorAgentsResponse,
	tokens *contributorTokensResponse,
	analyses map[string]*agentAnalysisAgg,
) *ContributorsData {
	out := &ContributorsData{Repo: repo, ByAgent: map[string]*AgentContrib{}}
	byAgent := out.ByAgent

	if act != nil {
		for _, d := range act.Daily {
			if d.Agent == "" {
				continue
			}
			a, ok := byAgent[d.Agent]
			if !ok {
				a = &AgentContrib{}
				byAgent[d.Agent] = a
			}
			a.TotalCount += d.Count
			a.Tokens += d.Tokens
		}
	}

	// Distinct contributors per agent: walk contributor-agents and count any
	// contributor whose agents map includes that agent with > 0 commits.
	if agents != nil {
		agentDistinct := map[string]int{}
		for _, c := range agents.Contributors {
			for agentName, cnt := range c.Agents {
				if cnt > 0 {
					agentDistinct[agentName]++
				}
			}
		}
		for agentName, distinct := range agentDistinct {
			a, ok := byAgent[agentName]
			if !ok {
				a = &AgentContrib{}
				byAgent[agentName] = a
			}
			a.DistinctContribs = distinct
		}
	}

	// contributor-tokens is user-scoped (sum across all agents per user), not
	// per-agent. We surface it separately via ContributorsData.TotalTokens
	// aggregates if callers need it — today AgentCard.ContribTokens already
	// came from agent-activity above, which is more accurate per-agent.
	_ = tokens

	// Promote per-agent analysis aggregates (labels/skills/MCP/toolMix) into
	// the AgentContrib slots. Sorted-descending lists for display stability.
	for agent, agg := range analyses {
		if agg == nil {
			continue
		}
		a, ok := byAgent[agent]
		if !ok {
			a = &AgentContrib{}
			byAgent[agent] = a
		}
		a.Labels = labelCountsSorted(agg.labels)
		a.Skills = topNByCount(agg.skills, 3)
		a.MCPServers = topNByCount(agg.mcp, 3)
		a.ToolMix = agg.toolMix
	}

	return out
}

// response types — minimal shapes matching entire.io API ----------------------

// commitsResponse maps the /api/v1/cache/:org/:repo/commits payload down to
// what we need: each commit's checkpoint IDs + producing agent.
type commitsResponse struct {
	Commits []struct {
		Checkpoints []struct {
			CheckpointID string `json:"checkpoint_id"`
			Agent        string `json:"agent"`
		} `json:"checkpoints"`
	} `json:"commits"`
}

// batchAnalysesResponse maps the
// POST /api/v1/cache/checkpoints/analyses/batch payload.
type batchAnalysesResponse struct {
	Analyses map[string]*CheckpointAnalysisResponse `json:"analyses"`
}

type agentActivityResponse struct {
	Daily []struct {
		Date         string `json:"date"`
		Agent        string `json:"agent"`
		Count        int    `json:"count"`
		Tokens       int    `json:"tokens"`
		InputTokens  int    `json:"inputTokens"`
		OutputTokens int    `json:"outputTokens"`
	} `json:"daily"`
}

type contributorAgentsResponse struct {
	TotalCommits int `json:"total_commits"`
	Contributors []struct {
		Username     *string        `json:"username"`
		GithubID     *int           `json:"github_id"`
		TotalCommits int            `json:"total_commits"`
		Agents       map[string]int `json:"agents"`
		Untracked    int            `json:"untracked"`
	} `json:"contributors"`
}

type contributorTokensResponse struct {
	Contributors []struct {
		Username     *string `json:"username"`
		GithubID     *int    `json:"github_id"`
		InputTokens  int     `json:"input_tokens"`
		OutputTokens int     `json:"output_tokens"`
	} `json:"contributors"`
}

// fetchJSON is a tiny generic GET+decode helper kept in this file because it's
// only used for the three overview calls. If a second consumer appears, move
// it to the api package.
func fetchJSON[T any](ctx context.Context, client *api.Client, path string) (*T, error) {
	resp, err := client.Get(ctx, path)
	if err != nil {
		return nil, fmt.Errorf("contributors get %s: %w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body) //nolint:errcheck // best-effort error body
		return nil, fmt.Errorf("contributors %s: http %d: %s", path, resp.StatusCode, string(body))
	}
	var out T
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("contributors decode %s: %w", path, err)
	}
	return &out, nil
}
