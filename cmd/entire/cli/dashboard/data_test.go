package dashboard

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestTimeAgo(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		d    time.Duration
		want string
	}{
		{name: "just_now", d: 30 * time.Second, want: "just now"},
		{name: "minutes", d: 15 * time.Minute, want: "15m ago"},
		{name: "one_minute", d: 1 * time.Minute, want: "1m ago"},
		{name: "hours", d: 3 * time.Hour, want: "3h ago"},
		{name: "one_hour", d: 1 * time.Hour, want: "1h ago"},
		{name: "days", d: 48 * time.Hour, want: "2d ago"},
		{name: "one_day", d: 25 * time.Hour, want: "1d ago"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := timeAgo(time.Now().Add(-tt.d))
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestTruncate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		s      string
		maxLen int
		want   string
	}{
		{name: "short", s: "hi", maxLen: 10, want: "hi"},
		{name: "exact", s: "hello", maxLen: 5, want: "hello"},
		{name: "long", s: "hello world", maxLen: 8, want: "hello..."},
		{name: "maxLen_less_than_3", s: "hello", maxLen: 2, want: "he"},
		{name: "empty", s: "", maxLen: 5, want: ""},
		{name: "unicode", s: "こんにちは世界", maxLen: 5, want: "こん..."},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := truncate(tt.s, tt.maxLen)
			assert.Equal(t, tt.want, got)
		})
	}
}
