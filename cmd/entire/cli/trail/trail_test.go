package trail

import (
	"testing"
)

func TestGenerateID(t *testing.T) {
	t.Parallel()

	id, err := GenerateID()
	if err != nil {
		t.Fatalf("GenerateID() error = %v", err)
	}
	if len(id) != 12 {
		t.Errorf("expected 12-char ID, got %d: %q", len(id), id)
	}
	if err := ValidateID(id.String()); err != nil {
		t.Errorf("generated ID failed validation: %v", err)
	}
}

func TestGenerateID_Unique(t *testing.T) {
	t.Parallel()

	seen := make(map[ID]bool)
	for range 100 {
		id, err := GenerateID()
		if err != nil {
			t.Fatalf("GenerateID() error = %v", err)
		}
		if seen[id] {
			t.Errorf("duplicate ID generated: %s", id)
		}
		seen[id] = true
	}
}

func TestValidateID(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		id      string
		wantErr bool
	}{
		{"valid", "abcdef123456", false},
		{"valid_all_hex", "0123456789ab", false},
		{"too_short", "abcdef", true},
		{"too_long", "abcdef1234567", true},
		{"uppercase", "ABCDEF123456", true},
		{"non_hex", "ghijkl123456", true},
		{"empty", "", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := ValidateID(tt.id)
			if (err != nil) != tt.wantErr {
				t.Errorf("ValidateID(%q) error = %v, wantErr %v", tt.id, err, tt.wantErr)
			}
		})
	}
}

func TestID_Path(t *testing.T) {
	t.Parallel()

	tests := []struct {
		id   ID
		want string
	}{
		{"abcdef123456", "ab/cdef123456"},
		{"0123456789ab", "01/23456789ab"},
		{"ab", "ab"},
	}

	for _, tt := range tests {
		t.Run(string(tt.id), func(t *testing.T) {
			t.Parallel()
			if got := tt.id.Path(); got != tt.want {
				t.Errorf("ID(%q).Path() = %q, want %q", tt.id, got, tt.want)
			}
		})
	}
}

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
		{StatusInProgress, true},
		{StatusInReview, true},
		{StatusMerged, true},
		{StatusClosed, true},
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
	if len(statuses) != 6 {
		t.Errorf("expected 6 statuses, got %d", len(statuses))
	}
	// Verify lifecycle order
	expected := []Status{StatusDraft, StatusOpen, StatusInProgress, StatusInReview, StatusMerged, StatusClosed}
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
