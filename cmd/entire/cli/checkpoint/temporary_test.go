package checkpoint

import (
	"testing"

	"entire.io/cli/cmd/entire/cli/paths"
)

func TestHashWorktreeID(t *testing.T) {
	tests := []struct {
		name       string
		worktreeID string
		wantLen    int
	}{
		{
			name:       "empty string (main worktree)",
			worktreeID: "",
			wantLen:    6,
		},
		{
			name:       "simple worktree name",
			worktreeID: "test-123",
			wantLen:    6,
		},
		{
			name:       "complex worktree name",
			worktreeID: "feature/auth-system",
			wantLen:    6,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := HashWorktreeID(tt.worktreeID)
			if len(got) != tt.wantLen {
				t.Errorf("HashWorktreeID(%q) length = %d, want %d", tt.worktreeID, len(got), tt.wantLen)
			}
		})
	}
}

func TestHashWorktreeID_Deterministic(t *testing.T) {
	// Same input should always produce same output
	id := "test-worktree"
	hash1 := HashWorktreeID(id)
	hash2 := HashWorktreeID(id)
	if hash1 != hash2 {
		t.Errorf("HashWorktreeID not deterministic: %q != %q", hash1, hash2)
	}
}

func TestHashWorktreeID_DifferentInputs(t *testing.T) {
	// Different inputs should produce different outputs
	hash1 := HashWorktreeID("worktree-a")
	hash2 := HashWorktreeID("worktree-b")
	if hash1 == hash2 {
		t.Errorf("HashWorktreeID produced same hash for different inputs: %q", hash1)
	}
}

func TestShadowBranchNameForCommit(t *testing.T) {
	tests := []struct {
		name       string
		baseCommit string
		worktreeID string
		want       string
	}{
		{
			name:       "main worktree",
			baseCommit: "abc1234567890",
			worktreeID: "",
			want:       "entire/abc1234-" + HashWorktreeID(""),
		},
		{
			name:       "linked worktree",
			baseCommit: "abc1234567890",
			worktreeID: "test-123",
			want:       "entire/abc1234-" + HashWorktreeID("test-123"),
		},
		{
			name:       "short commit hash",
			baseCommit: "abc",
			worktreeID: "wt",
			want:       "entire/abc-" + HashWorktreeID("wt"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ShadowBranchNameForCommit(tt.baseCommit, tt.worktreeID)
			if got != tt.want {
				t.Errorf("ShadowBranchNameForCommit(%q, %q) = %q, want %q",
					tt.baseCommit, tt.worktreeID, got, tt.want)
			}
		})
	}
}

func TestParseShadowBranchName(t *testing.T) {
	tests := []struct {
		name         string
		branchName   string
		wantCommit   string
		wantWorktree string
		wantOK       bool
	}{
		{
			name:         "new format with worktree hash",
			branchName:   "entire/abc1234-e3b0c4",
			wantCommit:   "abc1234",
			wantWorktree: "e3b0c4",
			wantOK:       true,
		},
		{
			name:         "old format without worktree hash",
			branchName:   "entire/abc1234",
			wantCommit:   "abc1234",
			wantWorktree: "",
			wantOK:       true,
		},
		{
			name:         "full commit hash with worktree",
			branchName:   "entire/abcdef1234567890-fedcba",
			wantCommit:   "abcdef1234567890",
			wantWorktree: "fedcba",
			wantOK:       true,
		},
		{
			name:         "not a shadow branch",
			branchName:   "main",
			wantCommit:   "",
			wantWorktree: "",
			wantOK:       false,
		},
		{
			name:         "entire/sessions/v1 is not a shadow branch",
			branchName:   paths.MetadataBranchName,
			wantCommit:   "sessions/v1",
			wantWorktree: "",
			wantOK:       true, // Parser doesn't validate content, just extracts
		},
		{
			name:         "empty suffix after prefix",
			branchName:   "entire/",
			wantCommit:   "",
			wantWorktree: "",
			wantOK:       true, // Empty commit, empty worktree
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			commit, worktree, ok := ParseShadowBranchName(tt.branchName)
			if ok != tt.wantOK {
				t.Errorf("ParseShadowBranchName(%q) ok = %v, want %v", tt.branchName, ok, tt.wantOK)
			}
			if commit != tt.wantCommit {
				t.Errorf("ParseShadowBranchName(%q) commit = %q, want %q", tt.branchName, commit, tt.wantCommit)
			}
			if worktree != tt.wantWorktree {
				t.Errorf("ParseShadowBranchName(%q) worktree = %q, want %q", tt.branchName, worktree, tt.wantWorktree)
			}
		})
	}
}

func TestParseShadowBranchName_RoundTrip(t *testing.T) {
	// Test that ShadowBranchNameForCommit and ParseShadowBranchName are inverses
	testCases := []struct {
		baseCommit string
		worktreeID string
	}{
		{"abc1234567890", ""},
		{"abc1234567890", "test-worktree"},
		{"deadbeef", "feature/auth"},
	}

	for _, tc := range testCases {
		branchName := ShadowBranchNameForCommit(tc.baseCommit, tc.worktreeID)
		commitPrefix, worktreeHash, ok := ParseShadowBranchName(branchName)

		if !ok {
			t.Errorf("ParseShadowBranchName failed for %q", branchName)
			continue
		}

		// Commit should be truncated to 7 chars
		expectedCommit := tc.baseCommit
		if len(expectedCommit) > ShadowBranchHashLength {
			expectedCommit = expectedCommit[:ShadowBranchHashLength]
		}
		if commitPrefix != expectedCommit {
			t.Errorf("Round trip commit mismatch: got %q, want %q", commitPrefix, expectedCommit)
		}

		// Worktree hash should match
		expectedWorktree := HashWorktreeID(tc.worktreeID)
		if worktreeHash != expectedWorktree {
			t.Errorf("Round trip worktree mismatch: got %q, want %q", worktreeHash, expectedWorktree)
		}
	}
}
