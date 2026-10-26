package copilotcli

import (
	"context"
	"encoding/json"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
)

var _ agent.UsageCounterSource = (*CopilotCLIAgent)(nil)

const eventTypeUsageRecord = "session.usage_record"

// recentCallWindow is how many recent calls stay open to a re-written copy.
const recentCallWindow = 64

// NewUsageCounter counts the per-call session.usage_record events once per
// apiCallId, the latest copy winning; their inputTokens include the cache
// shares, which are split out. A record carrying a subagent's agentId (set on
// every event a subagent emits) is that child's usage.
//
// The main agent's session.shutdown aggregates (one per process, cumulative
// across resumes, summed per segment) are a second lower bound on the calls
// before the last shutdown, and also cover calls made before Copilot wrote
// usage records (see KeyedUsage.CoveredBy). They include the subagents' usage
// (captured sessions: main records + child records = shutdown total;
// subagents emit no shutdown of their own), so the children's records are
// subtracted from the main total.
func (c *CopilotCLIAgent) NewUsageCounter() agent.UsageCounter {
	return &usageCounter{Calls: agent.KeyedUsage{Policy: agent.KeepLast, Window: recentCallWindow, Remember: true}}
}

type usageCounter struct {
	Calls     agent.KeyedUsage   `json:"calls"`
	Shutdowns agent.RunningTotal `json:"shutdowns"`
}

func (u *usageCounter) Files(in agent.CumulativeTokenInput) []agent.UsageFile {
	return []agent.UsageFile{{Path: in.TranscriptPath}}
}

func (u *usageCounter) Add(_ string, data []byte) {
	agent.ForEachLine(data, func(raw []byte) {
		var event copilotEvent
		if json.Unmarshal(raw, &event) != nil {
			return
		}
		switch event.Type {
		case eventTypeUsageRecord:
			var data struct {
				Usage *struct {
					modelUsage

					APICallID string `json:"apiCallId"`
				} `json:"usage"`
			}
			if json.Unmarshal(event.Data, &data) != nil || data.Usage == nil {
				return
			}
			u.Calls.Add(event.AgentID, data.Usage.APICallID, *data.Usage.tokenUsage(1))
		case eventTypeSessionShutdown:
			var data sessionShutdownData
			if event.AgentID != "" || json.Unmarshal(event.Data, &data) != nil || len(data.ModelMetrics) == 0 {
				return
			}
			var usage *agent.TokenUsage
			for _, m := range data.ModelMetrics {
				usage = types.AddTokenUsage(usage, m.Usage.tokenUsage(m.Requests.Count))
			}
			if usage = agent.UsageOrNil(usage); usage != nil {
				u.Shutdowns.Add(usage)
				u.Calls.Mark()
			}
		}
	})
}

func (u *usageCounter) Reset(string) {
	u.Calls.Reset()
	u.Shutdowns = agent.RunningTotal{}
}

func (u *usageCounter) Totals(context.Context, agent.CumulativeTokenInput) (*agent.TokenUsage, map[string]*agent.TokenUsage) {
	children := u.Calls.Children()
	var childTotal *agent.TokenUsage
	for _, child := range children {
		childTotal = types.AddTokenUsage(childTotal, child)
	}
	return types.SubtractTokenUsage(u.Calls.CoveredBy(u.Shutdowns.Value()), childTotal), children
}

// tokenUsage converts Copilot's cache-inclusive usage counts.
func (u modelUsage) tokenUsage(calls int) *agent.TokenUsage {
	return &agent.TokenUsage{
		InputTokens:         agent.NonCacheInput(u.InputTokens, u.CacheReadTokens, u.CacheWriteTokens),
		CacheReadTokens:     u.CacheReadTokens,
		CacheCreationTokens: u.CacheWriteTokens,
		OutputTokens:        u.OutputTokens,
		APICallCount:        calls,
	}
}
