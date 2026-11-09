package factoryaidroid

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"math"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/logging"
)

var _ agent.OutOfBandTokenSource = (*FactoryAIDroidAgent)(nil)

// droidTokenUsage is the token block Droid keeps in a session's
// .settings.json. It is the session's running total, carried across resumes,
// and already current when the Stop hook fires. It counts only this session's
// own calls: a Worker keeps its own total in its own settings file, and the
// parent's "inclusiveTokenUsage" (deliberately not read) adds the Workers in.
//
// inputTokens excludes cached input, the same convention as agent.TokenUsage.
// outputTokens already includes reasoning: Droid's Anthropic, OpenAI
// Responses, native Gemini, and Bedrock adapters all store the provider's
// output total, reasoning included. Its "thinkingTokens" is a breakdown of
// that figure (often estimated from text length), so it is not read: adding
// it would count reasoning twice. The generic Chat Completions adapter stores
// completion_tokens, which for some providers (Gemini) excludes reasoning;
// those tokens never reach the file.
type droidTokenUsage struct {
	InputTokens         int `json:"inputTokens"`
	OutputTokens        int `json:"outputTokens"`
	CacheCreationTokens int `json:"cacheCreationTokens"`
	CacheReadTokens     int `json:"cacheReadTokens"`
}

// droidTokenBaseline is the turn-start position, stored opaquely in the
// pre-prompt state. It is written even when the settings file or its token
// block does not exist yet (Tokens nil, meaning a fresh session), so an absent
// baseline at turn end always means "position unknown" and never "count from
// zero". A settings file that exists but cannot be read writes no baseline.
type droidTokenBaseline struct {
	Tokens          *droidTokenUsage `json:"tokens"`
	TranscriptLines int              `json:"transcript_lines"`
}

// SnapshotTokenBaseline records the session's running token total and the
// transcript's line count at turn start.
func (f *FactoryAIDroidAgent) SnapshotTokenBaseline(ctx context.Context, _, sessionRef string) (json.RawMessage, error) {
	if sessionRef == "" {
		return nil, nil // no transcript path, no position
	}
	base := droidTokenBaseline{TranscriptLines: transcriptLineCount(sessionRef)}
	settings, err := readSessionSettings(ctx, sessionRef)
	switch {
	case err == nil:
		base.Tokens = settings.TokenUsage
	case !errors.Is(err, fs.ErrNotExist):
		// Unreadable is not "fresh": a zero start would re-add earlier turns
		// if the file reads fine at Stop.
		return nil, nil // position unknown, no token counts for the turn
	}
	raw, err := json.Marshal(base)
	if err != nil {
		return nil, nil //nolint:nilerr // no baseline degrades to no token counts for the turn
	}
	return raw, nil
}

// CalculateTokenUsageSince returns the tokens used since the baseline: the
// running total minus the turn-start total.
// The settings file has no call count, so APICallCount is the number of
// distinct assistant messages written since the baseline, one per model call.
//
// An absent baseline returns nil rather than the session total: the turn-start
// position is unknown, and counting from zero would re-add every earlier turn.
func (f *FactoryAIDroidAgent) CalculateTokenUsageSince(ctx context.Context, sessionID, sessionRef string, baseline json.RawMessage) (*agent.TokenUsage, error) {
	if sessionRef == "" || len(baseline) == 0 {
		return nil, nil //nolint:nilnil // no position, no token counts for the turn
	}
	var base droidTokenBaseline
	if err := json.Unmarshal(baseline, &base); err != nil {
		logging.Warn(ctx, "factoryai-droid: unreadable token baseline, skipping turn tokens",
			slog.String("session_id", sessionID))
		return nil, nil //nolint:nilerr,nilnil // an unreadable baseline degrades to no token counts
	}
	settings, err := readSessionSettings(ctx, sessionRef)
	if err != nil || settings.TokenUsage == nil {
		return nil, nil //nolint:nilnil,nilerr // no readable token data, no token counts for the turn
	}

	cur := *settings.TokenUsage
	var start droidTokenUsage
	if base.Tokens != nil {
		start = *base.Tokens
	}
	// A total below its turn-start value means Droid restarted the count, so
	// everything in the current total was used after the baseline.
	restarted := cur.InputTokens < start.InputTokens || cur.OutputTokens < start.OutputTokens ||
		cur.CacheCreationTokens < start.CacheCreationTokens || cur.CacheReadTokens < start.CacheReadTokens
	if restarted {
		start = droidTokenUsage{}
	}

	usage := &agent.TokenUsage{
		InputTokens:         cur.InputTokens - start.InputTokens,
		CacheCreationTokens: cur.CacheCreationTokens - start.CacheCreationTokens,
		CacheReadTokens:     cur.CacheReadTokens - start.CacheReadTokens,
		OutputTokens:        cur.OutputTokens - start.OutputTokens,
		APICallCount:        assistantMessagesSince(sessionRef, base.TranscriptLines, restarted),
	}
	if usage.InputTokens == 0 && usage.CacheCreationTokens == 0 && usage.CacheReadTokens == 0 && usage.OutputTokens == 0 {
		return nil, nil //nolint:nilnil // nothing used since the baseline
	}
	return usage, nil
}

// transcriptLineCount returns the transcript's raw line count, 0 when it
// cannot be read (a session whose transcript does not exist yet). A start
// past every line counts lines without decoding any of them.
func transcriptLineCount(sessionRef string) int {
	_, total, err := ParseDroidTranscript(sessionRef, math.MaxInt)
	if err != nil {
		return 0
	}
	return total
}

// assistantMessagesSince counts distinct assistant messages after startLine.
// A transcript shorter than startLine was rewritten: after a restart every
// message is new, but after a compaction the survivors are earlier turns' calls
// and which messages are new is unknown, so the count is 0.
func assistantMessagesSince(sessionRef string, startLine int, restarted bool) int {
	lines, total, err := ParseDroidTranscript(sessionRef, startLine)
	if err != nil {
		return 0
	}
	if total < startLine {
		if !restarted {
			return 0
		}
		if lines, _, err = ParseDroidTranscript(sessionRef, 0); err != nil {
			return 0
		}
	}
	seen := make(map[string]struct{})
	count := 0
	for _, line := range lines {
		if line.Type != roleAssistant {
			continue
		}
		if line.UUID != "" {
			if _, dup := seen[line.UUID]; dup {
				continue
			}
			seen[line.UUID] = struct{}{}
		}
		count++
	}
	return count
}
