package claudecode

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPreviousTurnStart(t *testing.T) {
	t.Parallel()
	const (
		prompt1   = `{"type":"user","timestamp":"2026-11-27T10:00:00Z","message":{"content":"first"}}`
		toolRes   = `{"type":"user","timestamp":"2026-11-27T10:00:02Z","message":{"content":[{"type":"tool_result","tool_use_id":"t1","content":""}]}}`
		answer1   = `{"type":"assistant","timestamp":"2026-11-27T10:00:03Z","message":{"content":[{"type":"text","text":"ok"}]}}`
		prompt2   = `{"type":"user","timestamp":"2026-11-27T11:00:00Z","message":{"content":"second"}}`
		answer2   = `{"type":"assistant","timestamp":"2026-11-27T11:00:01Z","message":{"content":[{"type":"text","text":"ok"}]}}`
		pending   = `{"type":"user","timestamp":"2026-11-27T12:00:00Z","message":{"content":"current"}}`
		badPrompt = `{"type":"user","timestamp":"yesterday","message":{"content":"first"}}`
	)
	tests := []struct {
		name   string
		lines  []string
		want   string
		wantOK bool
	}{
		{"empty transcript", nil, "", false},
		{"first turn, current prompt flushed", []string{pending}, "", false},
		{"one earlier turn", []string{prompt1, answer1}, "2026-11-27T10:00:00Z", true},
		{"tool results are not prompts", []string{prompt1, answer1, toolRes, answer1}, "2026-11-27T10:00:00Z", true},
		{"multi-turn", []string{prompt1, answer1, prompt2, answer2}, "2026-11-27T11:00:00Z", true},
		{"multi-turn, current prompt flushed", []string{prompt1, answer1, prompt2, answer2, pending}, "2026-11-27T11:00:00Z", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var data []byte
			for _, l := range tt.lines {
				data = append(data, l+"\n"...)
			}
			got, ok, err := previousTurnStart(data)
			require.NoError(t, err)
			assert.Equal(t, tt.wantOK, ok)
			if tt.wantOK {
				assert.Equal(t, tt.want, got.UTC().Format(time.RFC3339))
			}
		})
	}

	_, _, err := previousTurnStart([]byte(badPrompt + "\n" + answer1 + "\n"))
	assert.Error(t, err, "an unparseable timestamp must fail so the caller keeps the full baseline")
}

func TestPreviousTurnStart_MissingTranscript(t *testing.T) {
	t.Parallel()
	_, ok, err := (&ClaudeCodeAgent{}).PreviousTurnStart(t.TempDir() + "/missing.jsonl")
	require.NoError(t, err)
	assert.False(t, ok)
}
