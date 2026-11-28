package codex

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPreviousTurnStart(t *testing.T) {
	t.Parallel()
	const (
		meta     = `{"timestamp":"2026-11-27T09:59:59Z","type":"session_meta","payload":{"id":"s1"}}`
		started1 = `{"timestamp":"2026-11-27T10:00:00Z","type":"event_msg","payload":{"type":"task_started"}}`
		userMsg  = `{"timestamp":"2026-11-27T10:00:01Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}}`
		answer1  = `{"timestamp":"2026-11-27T10:00:02Z","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}}`
		started2 = `{"timestamp":"2026-11-27T11:00:00Z","type":"event_msg","payload":{"type":"task_started"}}`
		answer2  = `{"timestamp":"2026-11-27T11:00:02Z","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}}`
		started3 = `{"timestamp":"2026-11-27T12:00:00Z","type":"event_msg","payload":{"type":"task_started"}}`
	)
	tests := []struct {
		name   string
		lines  []string
		want   string
		wantOK bool
	}{
		{"empty transcript", nil, "", false},
		{"first turn starting", []string{meta, started1, userMsg}, "", false},
		{"one earlier turn", []string{meta, started1, userMsg, answer1}, "2026-11-27T10:00:00Z", true},
		{"multi-turn", []string{meta, started1, answer1, started2, answer2}, "2026-11-27T11:00:00Z", true},
		{"multi-turn, current turn started", []string{meta, started1, answer1, started2, answer2, started3, userMsg}, "2026-11-27T11:00:00Z", true},
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
}
