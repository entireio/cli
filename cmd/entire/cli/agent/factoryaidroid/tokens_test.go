package factoryaidroid

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/stretchr/testify/require"
)

// The token totals below are real snapshots of one Droid 0.243.0 session's
// .settings.json, taken at each Stop hook across a resumed session.

func writeDroidSession(t *testing.T, dir string, tokens *droidTokenUsage, transcriptLines ...string) string {
	t.Helper()
	ref := filepath.Join(dir, "session.jsonl")
	content := strings.Join(transcriptLines, "\n")
	if content != "" {
		content += "\n"
	}
	require.NoError(t, os.WriteFile(ref, []byte(content), 0o600))
	settings := map[string]any{"model": "custom:Gemini-2.5-Pro-0"}
	if tokens != nil {
		settings["tokenUsage"] = tokens
	}
	raw, err := json.Marshal(settings)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "session.settings.json"), raw, 0o600))
	return ref
}

const (
	userLine       = `{"type":"message","id":"u1","message":{"role":"user","content":[{"type":"text","text":"hi"}]}}`
	assistantLine1 = `{"type":"message","id":"a1","message":{"role":"assistant","content":[{"type":"text","text":"one"}]}}`
	assistantLine2 = `{"type":"message","id":"a2","message":{"role":"assistant","content":[{"type":"text","text":"two"}]}}`
	assistantLine3 = `{"type":"message","id":"a3","message":{"role":"assistant","content":[{"type":"text","text":"three"}]}}`
)

func TestCalculateTokenUsageSince_FreshSessionCountsTheWholeTotal(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	ag := &FactoryAIDroidAgent{}
	dir := t.TempDir()

	ref := writeDroidSession(t, dir, nil, `{"type":"session_start","id":"s"}`)
	baseline, err := ag.SnapshotTokenBaseline(ctx, "s", ref)
	require.NoError(t, err)
	require.NotEmpty(t, baseline, "a fresh session still records a baseline, so an absent one can mean only 'unknown'")

	writeDroidSession(t, dir, &droidTokenUsage{InputTokens: 8035, OutputTokens: 175, CacheReadTokens: 20095},
		`{"type":"session_start","id":"s"}`, userLine, assistantLine1, assistantLine2, assistantLine3)
	usage, err := ag.CalculateTokenUsageSince(ctx, "s", ref, baseline)
	require.NoError(t, err)
	require.Equal(t, &agent.TokenUsage{InputTokens: 8035, OutputTokens: 175, CacheReadTokens: 20095, APICallCount: 3}, usage)
}

func TestCalculateTokenUsageSince_ResumedSessionCountsOnlyThisTurn(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	ag := &FactoryAIDroidAgent{}
	dir := t.TempDir()

	ref := writeDroidSession(t, dir, &droidTokenUsage{InputTokens: 8035, OutputTokens: 175, CacheReadTokens: 20095},
		userLine, assistantLine1)
	baseline, err := ag.SnapshotTokenBaseline(ctx, "s", ref)
	require.NoError(t, err)

	writeDroidSession(t, dir, &droidTokenUsage{InputTokens: 30149, OutputTokens: 725, CacheReadTokens: 60603},
		userLine, assistantLine1, userLine, assistantLine2, assistantLine3)
	usage, err := ag.CalculateTokenUsageSince(ctx, "s", ref, baseline)
	require.NoError(t, err)
	require.Equal(t, &agent.TokenUsage{InputTokens: 22114, OutputTokens: 550, CacheReadTokens: 40508, APICallCount: 2}, usage)
}

// Droid's outputTokens already includes reasoning; thinkingTokens is a
// breakdown of it, so counting both would double count.
func TestCalculateTokenUsageSince_DoesNotAddThinkingTokens(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	ag := &FactoryAIDroidAgent{}
	dir := t.TempDir()

	ref := writeDroidSession(t, dir, &droidTokenUsage{InputTokens: 100, OutputTokens: 10})
	baseline, err := ag.SnapshotTokenBaseline(ctx, "s", ref)
	require.NoError(t, err)

	writeDroidSettingsJSON(t, dir, `{"tokenUsage":{"inputTokens":300,"outputTokens":40,"cacheCreationTokens":12,"cacheReadTokens":0,"thinkingTokens":25}}`)
	require.NoError(t, os.WriteFile(ref, []byte(assistantLine1+"\n"), 0o600))
	usage, err := ag.CalculateTokenUsageSince(ctx, "s", ref, baseline)
	require.NoError(t, err)
	require.Equal(t, &agent.TokenUsage{InputTokens: 200, OutputTokens: 30, CacheCreationTokens: 12, APICallCount: 1}, usage)
}

func writeDroidSettingsJSON(t *testing.T, dir, raw string) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "session.settings.json"), []byte(raw), 0o600))
}

func TestCalculateTokenUsageSince_NoBaselineIsNotTheSessionTotal(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	ref := writeDroidSession(t, t.TempDir(), &droidTokenUsage{InputTokens: 30149, OutputTokens: 725}, assistantLine1)

	usage, err := (&FactoryAIDroidAgent{}).CalculateTokenUsageSince(ctx, "s", ref, nil)
	require.NoError(t, err)
	require.Nil(t, usage, "a lost turn-start position must not re-add every earlier turn")
}

func TestCalculateTokenUsageSince_NoNewUsageIsNil(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	ag := &FactoryAIDroidAgent{}
	ref := writeDroidSession(t, t.TempDir(), &droidTokenUsage{InputTokens: 8035, OutputTokens: 175}, assistantLine1)

	baseline, err := ag.SnapshotTokenBaseline(ctx, "s", ref)
	require.NoError(t, err)
	usage, err := ag.CalculateTokenUsageSince(ctx, "s", ref, baseline)
	require.NoError(t, err)
	require.Nil(t, usage, "a second Stop for the same turn adds nothing")
}

func TestCalculateTokenUsageSince_TotalsGoingBackwardsAreARestart(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	ag := &FactoryAIDroidAgent{}
	dir := t.TempDir()

	ref := writeDroidSession(t, dir, &droidTokenUsage{InputTokens: 9000, OutputTokens: 300}, userLine, assistantLine1, assistantLine2)
	baseline, err := ag.SnapshotTokenBaseline(ctx, "s", ref)
	require.NoError(t, err)

	// The count restarted and the transcript was rewritten shorter.
	writeDroidSession(t, dir, &droidTokenUsage{InputTokens: 400, OutputTokens: 20}, assistantLine3)
	usage, err := ag.CalculateTokenUsageSince(ctx, "s", ref, baseline)
	require.NoError(t, err)
	require.Equal(t, &agent.TokenUsage{InputTokens: 400, OutputTokens: 20, APICallCount: 1}, usage)
}

// A compaction rewrites the transcript shorter without restarting the totals:
// the surviving messages are earlier turns' calls, so none are recounted.
func TestCalculateTokenUsageSince_CompactionDoesNotRecountHistory(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	ag := &FactoryAIDroidAgent{}
	dir := t.TempDir()

	ref := writeDroidSession(t, dir, &droidTokenUsage{InputTokens: 9000, OutputTokens: 300},
		userLine, assistantLine1, userLine, assistantLine2)
	baseline, err := ag.SnapshotTokenBaseline(ctx, "s", ref)
	require.NoError(t, err)

	writeDroidSession(t, dir, &droidTokenUsage{InputTokens: 9500, OutputTokens: 340}, assistantLine2, assistantLine3)
	usage, err := ag.CalculateTokenUsageSince(ctx, "s", ref, baseline)
	require.NoError(t, err)
	require.Equal(t, &agent.TokenUsage{InputTokens: 500, OutputTokens: 40}, usage)
}

// A settings file that exists but cannot be read at turn start is an unknown
// position, not a fresh session: reading it fine at Stop must not count the
// whole running total, which includes earlier turns.
func TestCalculateTokenUsageSince_UnreadableSettingsAtTurnStartIsNil(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	ag := &FactoryAIDroidAgent{}
	dir := t.TempDir()

	ref := writeDroidSession(t, dir, nil, userLine, assistantLine1)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "session.settings.json"), []byte(`{"tokenUsage":`), 0o600))
	baseline, err := ag.SnapshotTokenBaseline(ctx, "s", ref)
	require.NoError(t, err)
	require.Empty(t, baseline)

	writeDroidSession(t, dir, &droidTokenUsage{InputTokens: 30149, OutputTokens: 725, CacheReadTokens: 60603},
		userLine, assistantLine1, userLine, assistantLine2)
	usage, err := ag.CalculateTokenUsageSince(ctx, "s", ref, baseline)
	require.NoError(t, err)
	require.Nil(t, usage)
}

func TestCalculateTokenUsageSince_MissingSettingsIsNil(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	ag := &FactoryAIDroidAgent{}
	dir := t.TempDir()
	ref := filepath.Join(dir, "session.jsonl")
	require.NoError(t, os.WriteFile(ref, []byte(assistantLine1+"\n"), 0o600))

	baseline, err := ag.SnapshotTokenBaseline(ctx, "s", ref)
	require.NoError(t, err)
	usage, err := ag.CalculateTokenUsageSince(ctx, "s", ref, baseline)
	require.NoError(t, err)
	require.Nil(t, usage)

	empty, err := ag.SnapshotTokenBaseline(ctx, "s", "")
	require.NoError(t, err)
	require.Nil(t, empty, "no transcript path, no position")
}

func TestAssistantMessagesSince_CountsEachMessageOnce(t *testing.T) {
	t.Parallel()
	ref := writeDroidSession(t, t.TempDir(), nil,
		assistantLine1, userLine, assistantLine2, assistantLine2, assistantLine3)

	require.Equal(t, 3, assistantMessagesSince(ref, 0, false), "a message written twice is one call")
	require.Equal(t, 2, assistantMessagesSince(ref, 2, false))
}

func TestExtractModelFromTranscript_StillReadsTheModel(t *testing.T) {
	t.Parallel()
	ref := writeDroidSession(t, t.TempDir(), &droidTokenUsage{InputTokens: 1})
	require.Equal(t, "Gemini-2.5-Pro-0", ExtractModelFromTranscript(context.Background(), ref))
}
