package cli

import (
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/api"
)

func TestSeverityDisplay(t *testing.T) {
	t.Parallel()
	high := changeReviewSeverityHigh
	blank := "   "
	tests := []struct {
		name string
		in   *string
		want string
	}{
		{"nil", nil, "-"},
		{"blank", &blank, "-"},
		{"value", &high, changeReviewSeverityHigh},
	}
	for _, tt := range tests {
		if got := severityDisplay(tt.in); got != tt.want {
			t.Errorf("%s: severityDisplay = %q, want %q", tt.name, got, tt.want)
		}
	}
}

func TestChangeReviewTargetDisplay(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		target changeReviewTarget
		want   string
	}{
		{
			name:   "number wins",
			target: changeReviewTarget{Change: api.ChangeResource{Number: 7, Title: "Fix auth", ID: "abc", Branch: "fix/auth"}},
			want:   "change #7 (Fix auth)",
		},
		{
			name:   "branch when no number",
			target: changeReviewTarget{Change: api.ChangeResource{ID: "abc", Branch: "fix/auth"}},
			want:   "change abc on fix/auth",
		},
		{
			name:   "id only",
			target: changeReviewTarget{Change: api.ChangeResource{ID: "abc"}},
			want:   "change abc",
		},
	}
	for _, tt := range tests {
		if got := changeReviewTargetDisplay(tt.target); got != tt.want {
			t.Errorf("%s: changeReviewTargetDisplay = %q, want %q", tt.name, got, tt.want)
		}
	}
}

func TestDefaultChangeReviewStatusReason(t *testing.T) {
	t.Parallel()
	tests := []struct {
		status, want string
	}{
		{changeReviewStatusResolved, "Resolved via Entire CLI"},
		{changeReviewStatusDismissed, "Dismissed via Entire CLI"},
		{changeReviewStatusOpen, "Reopened via Entire CLI"},
		{"something-else", "Updated via Entire CLI"},
	}
	for _, tt := range tests {
		if got := defaultChangeReviewStatusReason(tt.status); got != tt.want {
			t.Errorf("defaultChangeReviewStatusReason(%q) = %q, want %q", tt.status, got, tt.want)
		}
	}
}

func TestParseOptionalChangeSelector(t *testing.T) {
	t.Parallel()

	got, err := parseOptionalChangeSelector(nil, "  42  ")
	if err != nil || got != "42" {
		t.Fatalf("flag only = (%q, %v), want (\"42\", nil)", got, err)
	}

	got, err = parseOptionalChangeSelector([]string{" main "}, "")
	if err != nil || got != "main" {
		t.Fatalf("positional only = (%q, %v), want (\"main\", nil)", got, err)
	}

	if _, err := parseOptionalChangeSelector([]string{"main"}, "42"); err == nil {
		t.Error("both positional and flag should error")
	}

	if _, err := parseOptionalChangeSelector([]string{"   "}, ""); err == nil {
		t.Error("empty positional selector should error")
	}

	if got, err := parseOptionalChangeSelector(nil, ""); err != nil || got != "" {
		t.Fatalf("neither = (%q, %v), want (\"\", nil)", got, err)
	}
}

func TestTruncateForLog(t *testing.T) {
	t.Parallel()

	// Newlines collapse to spaces.
	if got := truncateForLog("line1\nline2\r\nline3", 100); got != "line1 line2 line3" {
		t.Errorf("newline collapse = %q", got)
	}

	// Short input is returned unchanged.
	if got := truncateForLog("short", 100); got != "short" {
		t.Errorf("short input = %q, want unchanged", got)
	}

	// Over-length input is clipped on a rune boundary with an ellipsis.
	got := truncateForLog("abcdef", 3)
	if got != "abc…" {
		t.Errorf("clip = %q, want %q", got, "abc…")
	}

	// Clipping counts runes, not bytes (each ▶ is 3 bytes).
	multibyte := truncateForLog("▶▶▶▶▶", 2)
	if multibyte != "▶▶…" {
		t.Errorf("multibyte clip = %q, want %q", multibyte, "▶▶…")
	}
	if strings.Count(multibyte, "▶") != 2 {
		t.Errorf("multibyte clip kept %d runes, want 2", strings.Count(multibyte, "▶"))
	}
}
