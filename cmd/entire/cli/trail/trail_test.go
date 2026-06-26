package trail

import (
	"testing"
)

func TestID_IsEmpty(t *testing.T) {
	t.Parallel()

	if !EmptyID.IsEmpty() {
		t.Error("EmptyID.IsEmpty() should return true")
	}
	id := ID("abcdef123456")
	if id.IsEmpty() {
		t.Error("non-empty ID.IsEmpty() should return false")
	}
}

func TestStatus_IsValid(t *testing.T) {
	t.Parallel()

	tests := []struct {
		status Status
		valid  bool
	}{
		{StatusDraft, true},
		{StatusOpen, true},
		{StatusMerged, true},
		{StatusClosed, true},
		// Retired server-side (folded into open); no longer accepted.
		{"in_progress", false},
		{"in_review", false},
		{"invalid", false},
		{"", false},
	}

	for _, tt := range tests {
		t.Run(string(tt.status), func(t *testing.T) {
			t.Parallel()
			if got := tt.status.IsValid(); got != tt.valid {
				t.Errorf("Status(%q).IsValid() = %v, want %v", tt.status, got, tt.valid)
			}
		})
	}
}

func TestValidStatuses(t *testing.T) {
	t.Parallel()

	statuses := ValidStatuses()
	if len(statuses) != 4 {
		t.Errorf("expected 4 statuses, got %d", len(statuses))
	}
	// Verify lifecycle order
	expected := []Status{StatusDraft, StatusOpen, StatusMerged, StatusClosed}
	for i, s := range expected {
		if statuses[i] != s {
			t.Errorf("status[%d] = %q, want %q", i, statuses[i], s)
		}
	}
}

func TestHumanizeBranchName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		branch string
		want   string
	}{
		{"feature prefix", "feature/add-auth", "Add auth"},
		{"fix prefix", "fix/login-bug", "Login bug"},
		{"bugfix prefix", "bugfix/typo-fix", "Typo fix"},
		{"chore prefix", "chore/update-deps", "Update deps"},
		{"hotfix prefix", "hotfix/security-patch", "Security patch"},
		{"release prefix", "release/v2.0", "V2.0"},
		{"no prefix", "add-auth", "Add auth"},
		{"underscores", "add_user_auth", "Add user auth"},
		{"mixed separators", "fix/some_complex-name", "Some complex name"},
		{"simple name", "main", "Main"},
		{"empty after prefix", "feature/", "feature/"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := HumanizeBranchName(tt.branch); got != tt.want {
				t.Errorf("HumanizeBranchName(%q) = %q, want %q", tt.branch, got, tt.want)
			}
		})
	}
}
