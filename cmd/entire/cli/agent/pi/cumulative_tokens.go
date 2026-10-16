package pi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/pi/pijsonl"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
)

var _ agent.CumulativeTokenSource = (*PiAgent)(nil)

// CumulativeTokenUsage sums usage over every assistant message in the file.
// Unlike CalculateTokenUsage it does not keep to the active branch: a branch
// the user navigated away from still spent its tokens, and dropping it would
// make the running total shrink. Pi reports input without the cache shares.
func (a *PiAgent) CumulativeTokenUsage(_ context.Context, in agent.CumulativeTokenInput) (*agent.TokenUsage, map[string]*agent.TokenUsage, error) {
	var total *agent.TokenUsage
	scanner := pijsonl.NewScanner(in.TranscriptData)
	for scanner.Scan() {
		if !bytes.Contains(scanner.Bytes(), []byte(`"usage"`)) {
			continue // only assistant messages carry usage; skip decoding the rest
		}
		var entry pijsonl.Entry
		if json.Unmarshal(scanner.Bytes(), &entry) != nil || entry.Type != pijsonl.EntryTypeMessage ||
			entry.Message.Role != pijsonl.RoleAssistant || entry.Message.Usage == nil {
			continue
		}
		u := entry.Message.Usage
		total = types.AddTokenUsage(total, &agent.TokenUsage{
			InputTokens: u.Input, OutputTokens: u.Output, CacheReadTokens: u.CacheRead, CacheCreationTokens: u.CacheWrite, APICallCount: 1,
		})
	}
	if err := scanner.Err(); err != nil {
		return nil, nil, fmt.Errorf("scan transcript: %w", err)
	}
	return total, nil, nil
}
