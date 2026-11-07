package factoryaidroid

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/agent"
)

var _ agent.UsageCounterSource = (*FactoryAIDroidAgent)(nil)

// recentMessageWindow is how many recent messages stay open to later lines.
const recentMessageWindow = 64

const settingsSuffix = ".settings.json"

// NewUsageCounter prefers the session-cumulative tokenUsage Droid keeps in the
// settings.json beside the transcript (verified on captured Droid 0.243.0
// sessions, BYOK included), with one call per distinct assistant message in
// the transcript. Droid writes settings.json on a debounce, so it can lag the
// transcript; the per-field max with the transcript's own usage (by message
// ID, most output winning) keeps the total from going down when it does, and
// covers versions that write usage there. A session's tokenUsage counts only
// its own calls: a Worker's usage reaches the parent once, credited as
// child-session tokens when its task completes.
func (f *FactoryAIDroidAgent) NewUsageCounter() agent.UsageCounter {
	return &usageCounter{
		Calls:    agent.KeyedUsage{Policy: agent.KeepMostOutput, Window: recentMessageWindow},
		Messages: agent.KeyedUsage{Policy: agent.KeepFirst, Remember: true},
	}
}

type usageCounter struct {
	// Calls holds the transcript's usage by message ID and Messages one call
	// per assistant message.
	Calls    agent.KeyedUsage `json:"calls"`
	Messages agent.KeyedUsage `json:"messages"`
	// Settings is settings.json's latest total.
	Settings *agent.TokenUsage `json:"settings,omitempty"`
}

// Files are the transcript and its settings.json. The transcript is not the
// only source of the total, so its rewrite is not reported (see
// UsageFile.NoRestart).
func (u *usageCounter) Files(in agent.CumulativeTokenInput) []agent.UsageFile {
	return []agent.UsageFile{
		{Path: in.TranscriptPath, NoRestart: true},
		{Path: strings.TrimSuffix(in.TranscriptPath, ".jsonl") + settingsSuffix, Whole: true},
	}
}

func (u *usageCounter) Add(path string, data []byte) {
	if strings.HasSuffix(path, settingsSuffix) {
		var settings droidSettings
		if json.Unmarshal(data, &settings) != nil || settings.TokenUsage == nil {
			return
		}
		t := settings.TokenUsage
		u.Settings = &agent.TokenUsage{
			InputTokens: t.InputTokens, OutputTokens: t.OutputTokens, CacheCreationTokens: t.CacheCreationTokens, CacheReadTokens: t.CacheReadTokens,
		}
		return
	}
	lines, _, err := ParseDroidTranscriptFromBytes(data, 0)
	if err != nil {
		return
	}
	for _, line := range lines {
		if line.Type != roleAssistant {
			continue
		}
		u.Messages.Add("", line.UUID, agent.TokenUsage{APICallCount: 1})
		var msg messageWithUsage
		if json.Unmarshal(line.Message, &msg) != nil || msg.ID == "" {
			continue
		}
		u.Calls.Add("", msg.ID, agent.TokenUsage{
			InputTokens:         msg.Usage.InputTokens,
			CacheCreationTokens: msg.Usage.CacheCreationInputTokens,
			CacheReadTokens:     msg.Usage.CacheReadInputTokens,
			OutputTokens:        msg.Usage.OutputTokens,
			APICallCount:        1,
		})
	}
}

func (u *usageCounter) Reset(path string) {
	if strings.HasSuffix(path, settingsSuffix) {
		u.Settings = nil
		return
	}
	u.Calls.Reset()
	u.Messages.Reset()
}

// Totals is the per-field max of settings.json's total (an all-zero
// placeholder is no data), with the transcript's call count, and the
// transcript's own usage.
func (u *usageCounter) Totals(context.Context, agent.CumulativeTokenInput) (*agent.TokenUsage, map[string]*agent.TokenUsage) {
	settings := agent.UsageOrNil(u.Settings)
	if settings != nil {
		c := *settings
		if messages := u.Messages.Sum(""); messages != nil {
			c.APICallCount = messages.APICallCount
		}
		settings = &c
	}
	transcript := u.Calls.Sum("")
	if transcript != nil && transcript.APICallCount == 0 {
		transcript = nil
	}
	return agent.MaxTokenUsage(settings, transcript), nil
}
