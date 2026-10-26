package pi

import (
	"bytes"
	"context"
	"encoding/json"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/internal/transcript/pijsonl"
)

var _ agent.UsageCounterSource = (*PiAgent)(nil)

// NewUsageCounter sums usage over every assistant message in the file.
// Unlike CalculateTokenUsage it does not keep to the active branch: a branch
// the user navigated away from still spent its tokens, and dropping it would
// make the running total shrink. Pi reports input without the cache shares,
// and writes each message once.
func (a *PiAgent) NewUsageCounter() agent.UsageCounter { return &usageCounter{} }

type usageCounter struct {
	Total *agent.TokenUsage `json:"total,omitempty"`
}

func (u *usageCounter) Files(in agent.CumulativeTokenInput) []agent.UsageFile {
	return []agent.UsageFile{{Path: in.TranscriptPath}}
}

func (u *usageCounter) Add(_ string, data []byte) {
	scanner := pijsonl.NewScanner(data)
	for scanner.Scan() {
		if !bytes.Contains(scanner.Bytes(), []byte(`"usage"`)) {
			continue // only assistant messages carry usage; skip decoding the rest
		}
		var entry pijsonl.Entry
		if json.Unmarshal(scanner.Bytes(), &entry) != nil || entry.Type != pijsonl.EntryTypeMessage ||
			entry.Message.Role != pijsonl.RoleAssistant || entry.Message.Usage == nil {
			continue
		}
		usage := entry.Message.Usage
		u.Total = types.AddTokenUsage(u.Total, &agent.TokenUsage{
			InputTokens: usage.Input, OutputTokens: usage.Output, CacheReadTokens: usage.CacheRead, CacheCreationTokens: usage.CacheWrite, APICallCount: 1,
		})
	}
}

func (u *usageCounter) Reset(string) { u.Total = nil }

func (u *usageCounter) Totals(context.Context, agent.CumulativeTokenInput) (*agent.TokenUsage, map[string]*agent.TokenUsage) {
	return u.Total, nil
}
