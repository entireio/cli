package cli

import (
	"testing"
	"time"
)

func strPtr(v string) *string { return &v }

func TestNormalizeAgentString(t *testing.T) {
	t.Parallel()
	tests := []struct {
		input string
		want  string
	}{
		{"Claude Code", "claude"},
		{"claude-code", "claude"},
		{"claude", "claude"},
		{"Gemini CLI", "gemini"},
		{"gemini", "gemini"},
		{"copilot-cli", "copilot"},
		{"Copilot CLI", "copilot"},
		{"OpenCode", "opencode"},
		{"open-code", "opencode"},
		{"factoryai-droid", "droid"},
		{"Factory AI Droid", "droid"},
		{"FactoryAIDroid", "droid"},
		{"codex", "codex"},
		{"pi", "pi"},
		{"cursor", "cursor"},
		{"kiro", "kiro"},
		{"amp", "amp"},
		{"some-unknown-agent", "unknown"},
		{"", "unknown"},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			t.Parallel()
			got := normalizeAgentString(tt.input)
			if got != tt.want {
				t.Errorf("normalizeAgentString(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestGroupCommitsByDay_SortsNewestFirst(t *testing.T) {
	t.Parallel()

	localDate := func(year int, month time.Month, day int) *string {
		return strPtr(time.Date(year, month, day, 12, 0, 0, 0, time.Local).Format(time.RFC3339))
	}

	commits := []userCommit{
		{CommitSHA: "aaa", CommitDate: localDate(2026, time.January, 10)},
		{CommitSHA: "bbb", CommitDate: localDate(2026, time.January, 12)},
		{CommitSHA: "ccc", CommitDate: localDate(2026, time.January, 11)},
	}
	days := groupCommitsByDay(commits)

	if len(days) != 3 {
		t.Fatalf("got %d day groups, want 3", len(days))
	}
	// Expect newest first: 2026-01-12, 2026-01-11, 2026-01-10
	if days[0].Commits[0].CommitSHA != "bbb" {
		t.Errorf("first day should contain commit bbb (2026-01-12)")
	}
	if days[1].Commits[0].CommitSHA != "ccc" {
		t.Errorf("second day should contain commit ccc (2026-01-11)")
	}
	if days[2].Commits[0].CommitSHA != "aaa" {
		t.Errorf("third day should contain commit aaa (2026-01-10)")
	}
}

func TestGroupCommitsByDay_UnknownDatesLast(t *testing.T) {
	t.Parallel()
	commits := []userCommit{
		{CommitSHA: "bad", CommitDate: nil},
		{CommitSHA: "good", CommitDate: strPtr("2026-01-15T10:00:00Z")},
	}
	days := groupCommitsByDay(commits)

	if len(days) != 2 {
		t.Fatalf("got %d day groups, want 2", len(days))
	}
	if days[0].Date == dateUnknown {
		t.Errorf("unknown-date commits should sort last, but appeared first")
	}
	if days[1].Date != dateUnknown {
		t.Errorf("unknown-date commits should be last group, got %q", days[1].Date)
	}
}

func TestGroupCommitsByDay_UnparseableDateGoesToUnknown(t *testing.T) {
	t.Parallel()
	commits := []userCommit{
		{CommitSHA: "x", CommitDate: strPtr("not-a-date")},
	}
	days := groupCommitsByDay(commits)

	if len(days) != 1 || days[0].Date != dateUnknown {
		t.Errorf("unparseable date should be grouped under %q", dateUnknown)
	}
}

func TestParseFlexibleTime(t *testing.T) {
	t.Parallel()
	tests := []struct {
		input   string
		wantErr bool
	}{
		{"2026-01-15T10:00:00Z", false},
		{"2026-01-15T10:00:00.123456789Z", false},
		{"2026-01-15T10:00:00+02:00", false},
		{"not-a-date", true},
		{"", true},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			t.Parallel()
			_, err := parseFlexibleTime(tt.input)
			if (err != nil) != tt.wantErr {
				t.Errorf("parseFlexibleTime(%q) err=%v, wantErr=%v", tt.input, err, tt.wantErr)
			}
		})
	}
}

func TestFormatCommitDate(t *testing.T) {
	t.Parallel()
	now := time.Now().Local()
	today := now.Format("2006-01-02")
	yesterday := now.AddDate(0, 0, -1).Format("2006-01-02")
	older := now.AddDate(0, 0, -5).Format("2006-01-02")
	future := now.AddDate(0, 0, 2).Format("2006-01-02")

	tests := []struct {
		name     string
		input    string
		contains string
		excludes string
	}{
		{"today", today, "(today)", ""},
		{"yesterday", yesterday, "(yesterday)", ""},
		{"older", older, "", "(today)"},
		{"future", future, "", "(today)"},
		{"invalid", "bad-date", "bad-date", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := formatCommitDate(tt.input)
			if tt.contains != "" && !containsStr(got, tt.contains) {
				t.Errorf("formatCommitDate(%q) = %q, want to contain %q", tt.input, got, tt.contains)
			}
			if tt.excludes != "" && containsStr(got, tt.excludes) {
				t.Errorf("formatCommitDate(%q) = %q, should not contain %q", tt.input, got, tt.excludes)
			}
		})
	}
}

func TestDetectTimezone_HonoursTZEnv(t *testing.T) {
	t.Setenv("TZ", "Europe/Berlin")
	if got := detectTimezone(); got != "Europe/Berlin" {
		t.Errorf("detectTimezone() = %q, want Europe/Berlin", got)
	}
}

func TestDetectTimezone_FallsBackToUTCForInvalidTZ(t *testing.T) {
	// Values Go can't parse as a zone must not reach the API verbatim — the
	// function should skip them and try the next source (/etc/localtime,
	// time.Local, or finally "UTC"). This doesn't guarantee strictly-IANA
	// names reach the server (Go accepts legacy aliases like EST5EDT);
	// the server is responsible for falling back to UTC for anything it
	// doesn't recognize.
	for _, tz := range []string{"UTC0", "bogus/zone", "Not_A_Zone"} {
		t.Run(tz, func(t *testing.T) {
			t.Setenv("TZ", tz)
			got := detectTimezone()
			// /etc/localtime may supply a valid IANA name, so we don't assert
			// strictly "UTC" — but the raw invalid TZ must never round-trip.
			if got == tz {
				t.Errorf("detectTimezone() forwarded raw TZ=%q unchanged", tz)
			}
			if _, err := time.LoadLocation(got); err != nil {
				t.Errorf("detectTimezone() = %q, not loadable as IANA zone", got)
			}
		})
	}
}

func TestNormalizeTimezone(t *testing.T) {
	t.Parallel()
	// These cases pin the contract we actually commit to: prefix stripping,
	// rejecting inputs Go can't load, and the "Local" sentinel. We don't
	// test that legacy aliases like EST5EDT are rejected — they're not;
	// Go loads them and we pass them through to the server.
	tests := []struct {
		name, input, want string
	}{
		{"plain IANA", "America/New_York", "America/New_York"},
		{"colon-prefixed", ":America/New_York", "America/New_York"},
		{"linux zoneinfo path", "/usr/share/zoneinfo/Europe/Berlin", "Europe/Berlin"},
		{"darwin zoneinfo path", "/var/db/timezone/zoneinfo/Europe/Berlin", "Europe/Berlin"},
		{"colon plus path", ":/usr/share/zoneinfo/UTC", "UTC"},
		{"UTC", "UTC", "UTC"},
		{"unloadable POSIX", "UTC0", ""},
		{"bogus", "Not_A_Zone", ""},
		{"empty", "", ""},
		{"Local sentinel", "Local", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := normalizeTimezone(tt.input)
			if got != tt.want {
				t.Errorf("normalizeTimezone(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func containsStr(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(sub) == 0 ||
		(len(s) > 0 && stringContains(s, sub)))
}

func stringContains(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
