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

func TestType_IsValid(t *testing.T) {
	t.Parallel()
	tests := []struct {
		typ   Type
		valid bool
	}{
		{TypeBug, true}, {TypeFeature, true}, {TypeTask, true},
		{"", false}, {"epic", false},
	}
	for _, tt := range tests {
		t.Run(string(tt.typ), func(t *testing.T) {
			t.Parallel()
			if got := tt.typ.IsValid(); got != tt.valid {
				t.Errorf("Type(%q).IsValid() = %v, want %v", tt.typ, got, tt.valid)
			}
		})
	}
	if len(ValidTypes()) != 3 {
		t.Errorf("ValidTypes() len = %d, want 3", len(ValidTypes()))
	}
}

func TestPriority_IsValid(t *testing.T) {
	t.Parallel()
	tests := []struct {
		p     Priority
		valid bool
	}{
		{PriorityUrgent, true}, {PriorityHigh, true}, {PriorityMedium, true},
		{PriorityLow, true}, {PriorityNone, true},
		{"", false}, {"critical", false},
	}
	for _, tt := range tests {
		t.Run(string(tt.p), func(t *testing.T) {
			t.Parallel()
			if got := tt.p.IsValid(); got != tt.valid {
				t.Errorf("Priority(%q).IsValid() = %v, want %v", tt.p, got, tt.valid)
			}
		})
	}
	if len(ValidPriorities()) != 5 {
		t.Errorf("ValidPriorities() len = %d, want 5", len(ValidPriorities()))
	}
}
