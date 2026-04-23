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
type AgentContrib struct {
	TotalCount       int // activity events (checkpoints or commits, depending on endpoint)
	Tokens           int
	DistinctContribs int
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

	wg.Add(3)
	go func() {
		defer wg.Done()
		out, err := fetchJSON[agentActivityResponse](ctx, client,
			fmt.Sprintf("/%s/%s/overview/agent-activity%s", org, name, params))
		mu.Lock()
		agentAct = out
		mu.Unlock()
		set(err)
	}()
	go func() {
		defer wg.Done()
		out, err := fetchJSON[contributorAgentsResponse](ctx, client,
			fmt.Sprintf("/%s/%s/overview/contributor-agents%s", org, name, params))
		mu.Lock()
		contribA = out
		mu.Unlock()
		set(err)
	}()
	go func() {
		defer wg.Done()
		out, err := fetchJSON[contributorTokensResponse](ctx, client,
			fmt.Sprintf("/%s/%s/overview/contributor-tokens%s", org, name, params))
		mu.Lock()
		contribT = out
		mu.Unlock()
		set(err)
	}()
	wg.Wait()

	if firstErr != nil {
		return nil, firstErr
	}
	return mergeContributors(repo, agentAct, contribA, contribT), nil
}

// mergeContributors collapses the three response shapes into the per-agent
// AgentContrib map. Missing inputs degrade to zero-valued fields rather than
// dropping the agent — a card with "7 sessions · — tokens · 3 contributors"
// is better than silently omitting the agent.
func mergeContributors(
	repo string,
	act *agentActivityResponse,
	agents *contributorAgentsResponse,
	tokens *contributorTokensResponse,
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

	return out
}

// response types — minimal shapes matching entire.io/api/src/routes/repo-overview.ts ---------

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
