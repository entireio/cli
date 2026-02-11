package cli

import "testing"

func TestExtractSessionIDFromMetadata(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		metadataDir string
		expected    string
	}{
		// New format: UUID session IDs (no date prefix)
		{
			name:        "UUID session ID",
			metadataDir: ".entire/metadata/0544a0f5-46a6-41b3-a89c-e7804df731b8",
			expected:    "0544a0f5-46a6-41b3-a89c-e7804df731b8",
		},
		{
			name:        "another UUID session ID",
			metadataDir: ".entire/metadata/f736da47-b2ca-4f86-bb32-a1bbe582e464",
			expected:    "f736da47-b2ca-4f86-bb32-a1bbe582e464",
		},
		// Legacy format: date-prefixed session IDs
		{
			name:        "legacy date-prefixed session ID",
			metadataDir: ".entire/metadata/2026-01-25-f736da47-b2ca-4f86-bb32-a1bbe582e464",
			expected:    "2026-01-25-f736da47-b2ca-4f86-bb32-a1bbe582e464",
		},
		{
			name:        "legacy with short uuid",
			metadataDir: ".entire/metadata/2026-02-10-abc123",
			expected:    "2026-02-10-abc123",
		},
		// Auto-commit format: sharded path
		{
			name:        "auto-commit sharded path with UUID",
			metadataDir: "ab/cdef1234/0544a0f5-46a6-41b3-a89c-e7804df731b8",
			expected:    "0544a0f5-46a6-41b3-a89c-e7804df731b8",
		},
		{
			name:        "auto-commit sharded path with legacy date",
			metadataDir: "ab/cdef1234/2025-11-28-session-uuid",
			expected:    "2025-11-28-session-uuid",
		},
		// Simple session IDs
		{
			name:        "simple session ID",
			metadataDir: ".entire/metadata/simple-session",
			expected:    "simple-session",
		},
		{
			name:        "short ID",
			metadataDir: ".entire/metadata/abc123",
			expected:    "abc123",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			result := extractSessionIDFromMetadata(tt.metadataDir)
			if result != tt.expected {
				t.Errorf("extractSessionIDFromMetadata(%q) = %q, want %q", tt.metadataDir, result, tt.expected)
			}
		})
	}
}
