package checkpoint

import "testing"

func TestShadowBranchNameForCommitWithSuffix(t *testing.T) {
	tests := []struct {
		name       string
		baseCommit string
		suffix     int
		expected   string
	}{
		{
			name:       "normal commit with suffix 1",
			baseCommit: "abc1234def",
			suffix:     1,
			expected:   "entire/abc1234-1",
		},
		{
			name:       "normal commit with suffix 2",
			baseCommit: "abc1234def",
			suffix:     2,
			expected:   "entire/abc1234-2",
		},
		{
			name:       "normal commit with suffix 10",
			baseCommit: "abc1234def",
			suffix:     10,
			expected:   "entire/abc1234-10",
		},
		{
			name:       "short commit with suffix",
			baseCommit: "short",
			suffix:     1,
			expected:   "entire/short-1",
		},
		{
			name:       "exact 7 char commit with suffix",
			baseCommit: "abc1234",
			suffix:     3,
			expected:   "entire/abc1234-3",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ShadowBranchNameForCommitWithSuffix(tc.baseCommit, tc.suffix)
			if got != tc.expected {
				t.Errorf("ShadowBranchNameForCommitWithSuffix(%q, %d) = %q, want %q",
					tc.baseCommit, tc.suffix, got, tc.expected)
			}
		})
	}
}

func TestParseShadowBranchName(t *testing.T) {
	tests := []struct {
		name       string
		branchName string
		wantBase   string
		wantSuffix int
		wantOK     bool
	}{
		{
			name:       "suffixed branch -1",
			branchName: "entire/abc1234-1",
			wantBase:   "abc1234",
			wantSuffix: 1,
			wantOK:     true,
		},
		{
			name:       "suffixed branch -2",
			branchName: "entire/abc1234-2",
			wantBase:   "abc1234",
			wantSuffix: 2,
			wantOK:     true,
		},
		{
			name:       "suffixed branch -10",
			branchName: "entire/abc1234-10",
			wantBase:   "abc1234",
			wantSuffix: 10,
			wantOK:     true,
		},
		{
			name:       "legacy format (no suffix)",
			branchName: "entire/abc1234",
			wantBase:   "abc1234",
			wantSuffix: 0,
			wantOK:     true,
		},
		{
			name:       "not a shadow branch - main",
			branchName: "main",
			wantBase:   "",
			wantSuffix: 0,
			wantOK:     false,
		},
		{
			name:       "not a shadow branch - feature",
			branchName: "feature/test",
			wantBase:   "",
			wantSuffix: 0,
			wantOK:     false,
		},
		{
			name:       "not a shadow branch - entire/sessions",
			branchName: "entire/sessions",
			wantBase:   "",
			wantSuffix: 0,
			wantOK:     false,
		},
		{
			name:       "short base with suffix",
			branchName: "entire/abc-5",
			wantBase:   "abc",
			wantSuffix: 5,
			wantOK:     true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gotBase, gotSuffix, gotOK := ParseShadowBranchName(tc.branchName)
			if gotOK != tc.wantOK {
				t.Errorf("ParseShadowBranchName(%q) ok = %v, want %v",
					tc.branchName, gotOK, tc.wantOK)
			}
			if gotBase != tc.wantBase {
				t.Errorf("ParseShadowBranchName(%q) base = %q, want %q",
					tc.branchName, gotBase, tc.wantBase)
			}
			if gotSuffix != tc.wantSuffix {
				t.Errorf("ParseShadowBranchName(%q) suffix = %d, want %d",
					tc.branchName, gotSuffix, tc.wantSuffix)
			}
		})
	}
}

func TestListShadowBranchNamesForCommit(t *testing.T) {
	// This test verifies the helper function that generates all possible
	// branch names to search for when looking for shadow branches.
	tests := []struct {
		name            string
		baseCommit      string
		maxSuffix       int
		wantContains    []string
		wantNotContains []string
	}{
		{
			name:       "generates legacy and suffixed names",
			baseCommit: "abc1234def",
			maxSuffix:  3,
			wantContains: []string{
				"entire/abc1234", // legacy format
				"entire/abc1234-1",
				"entire/abc1234-2",
				"entire/abc1234-3",
			},
			wantNotContains: []string{
				"entire/abc1234-4",
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ListShadowBranchNamesForCommit(tc.baseCommit, tc.maxSuffix)

			// Check expected names are present
			gotMap := make(map[string]bool)
			for _, name := range got {
				gotMap[name] = true
			}

			for _, want := range tc.wantContains {
				if !gotMap[want] {
					t.Errorf("ListShadowBranchNamesForCommit(%q, %d) missing %q, got %v",
						tc.baseCommit, tc.maxSuffix, want, got)
				}
			}

			for _, notWant := range tc.wantNotContains {
				if gotMap[notWant] {
					t.Errorf("ListShadowBranchNamesForCommit(%q, %d) should not contain %q, got %v",
						tc.baseCommit, tc.maxSuffix, notWant, got)
				}
			}
		})
	}
}
