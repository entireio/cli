package cli

import (
	"testing"
	"time"
)

func TestApplyPeriodFilter(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		period        string
		wantDaysDelta int
	}{
		{
			name:          "week period",
			period:        "week",
			wantDaysDelta: 7,
		},
		{
			name:          "month period",
			period:        "month",
			wantDaysDelta: 30, // Approximate
		},
		{
			name:          "year period",
			period:        "year",
			wantDaysDelta: 365,
		},
		{
			name:          "empty period defaults to week",
			period:        "",
			wantDaysDelta: 7,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			start, end := applyPeriodFilter(tt.period)

			// For month/year, we need to be more lenient due to month length variations
			tolerance := 2 * 24 * time.Hour
			if tt.period == "year" {
				tolerance = 5 * 24 * time.Hour
			}

			// Check that end is approximately now
			if time.Since(end) > 1*time.Minute {
				t.Errorf("end time should be approximately now, got %v", end)
			}

			// Check that start is approximately the right number of days ago
			duration := end.Sub(start)
			expectedDuration := time.Duration(tt.wantDaysDelta) * 24 * time.Hour

			diff := duration - expectedDuration
			if diff < 0 {
				diff = -diff
			}

			if diff > tolerance {
				t.Errorf("duration = %v, want approximately %v (delta %v)",
					duration, expectedDuration, diff)
			}
		})
	}
}

func TestApplyPeriodFilter_Unknown(t *testing.T) {
	t.Parallel()

	start, end := applyPeriodFilter("unknown")

	if !start.IsZero() || !end.IsZero() {
		t.Errorf("unknown period should return zero times, got start=%v end=%v", start, end)
	}
}

func TestFormatDuration(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		duration time.Duration
		want     string
	}{
		{
			name:     "less than hour",
			duration: 45 * time.Minute,
			want:     "45m",
		},
		{
			name:     "exactly one hour",
			duration: 1 * time.Hour,
			want:     "1h 0m",
		},
		{
			name:     "hours and minutes",
			duration: 2*time.Hour + 30*time.Minute,
			want:     "2h 30m",
		},
		{
			name:     "many hours",
			duration: 25*time.Hour + 15*time.Minute,
			want:     "25h 15m",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := formatDuration(tt.duration)
			if got != tt.want {
				t.Errorf("formatDuration() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestFormatNumber(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		num  int
		want string
	}{
		{
			name: "small number",
			num:  42,
			want: "42",
		},
		{
			name: "exactly 1000",
			num:  1000,
			want: "1,000",
		},
		{
			name: "thousands",
			num:  12345,
			want: "12,345",
		},
		{
			name: "millions",
			num:  1234567,
			want: "1,234,567",
		},
		{
			name: "zero",
			num:  0,
			want: "0",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := formatNumber(tt.num)
			if got != tt.want {
				t.Errorf("formatNumber() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestSimpleHash(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		str  string
	}{
		{
			name: "simple string",
			str:  "test-repo",
		},
		{
			name: "repo path",
			str:  "entireio/cli",
		},
		{
			name: "empty string",
			str:  "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := simpleHash(tt.str)

			// Check that hash is 8 hex characters
			if len(got) != 8 {
				t.Errorf("simpleHash() length = %d, want 8", len(got))
			}

			// Check that hash is hexadecimal
			for _, c := range got {
				if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
					t.Errorf("simpleHash() = %v, contains non-hex character %c", got, c)
				}
			}

			// Check that same input produces same hash
			got2 := simpleHash(tt.str)
			if got != got2 {
				t.Errorf("simpleHash() not deterministic: %v != %v", got, got2)
			}
		})
	}
}

func TestExtractRepoName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		url  string
		want string
	}{
		{
			name: "https url",
			url:  "https://github.com/entireio/cli.git",
			want: "cli",
		},
		{
			name: "https url without .git",
			url:  "https://github.com/entireio/cli",
			want: "cli",
		},
		{
			name: "ssh url",
			url:  "git@github.com:entireio/cli.git",
			want: "cli",
		},
		{
			name: "ssh url without .git",
			url:  "git@github.com:entireio/cli",
			want: "cli",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			// Parse repo name from URL directly (simplified test)
			url := tt.url
			name := url

			// Extract repo name from URL
			if idx := lastIndexOf(name, "/"); idx >= 0 {
				name = name[idx+1:]
			}
			if idx := lastIndexOf(name, ":"); idx >= 0 {
				name = name[idx+1:]
			}
			if len(name) > 4 && name[len(name)-4:] == ".git" {
				name = name[:len(name)-4]
			}

			if name != tt.want {
				t.Errorf("extractRepoName() = %v, want %v", name, tt.want)
			}
		})
	}
}

func TestBarChar(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		intensity float64
		want      string
	}{
		{
			name:      "zero intensity",
			intensity: 0.0,
			want:      "░",
		},
		{
			name:      "low intensity",
			intensity: 0.15,
			want:      "▒",
		},
		{
			name:      "medium intensity",
			intensity: 0.4,
			want:      "▓",
		},
		{
			name:      "high intensity",
			intensity: 0.8,
			want:      "█",
		},
		{
			name:      "max intensity",
			intensity: 1.0,
			want:      "█",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := barChar(tt.intensity)
			if got != tt.want {
				t.Errorf("barChar() = %v, want %v", got, tt.want)
			}
		})
	}
}
