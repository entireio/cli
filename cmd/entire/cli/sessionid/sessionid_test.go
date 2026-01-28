package sessionid

import (
	"testing"
)

func TestModelSessionID(t *testing.T) {
	tests := []struct {
		name            string
		entireSessionID string
		expectedModelID string
	}{
		// New format - returns as-is (agent ID = entire ID)
		{
			name:            "plain uuid - new format",
			entireSessionID: "f736da47-b2ca-4f86-bb32-a1bbe582e464",
			expectedModelID: "f736da47-b2ca-4f86-bb32-a1bbe582e464",
		},
		{
			name:            "short id - new format",
			entireSessionID: "abc123",
			expectedModelID: "abc123",
		},
		{
			name:            "empty string",
			entireSessionID: "",
			expectedModelID: "",
		},
		// Legacy format - extracts UUID (backwards compatibility)
		{
			name:            "legacy format with full uuid",
			entireSessionID: "2026-01-23-f736da47-b2ca-4f86-bb32-a1bbe582e464",
			expectedModelID: "f736da47-b2ca-4f86-bb32-a1bbe582e464",
		},
		{
			name:            "legacy format with short uuid",
			entireSessionID: "2026-01-23-abc123",
			expectedModelID: "abc123",
		},
		{
			name:            "legacy format different year",
			entireSessionID: "2025-12-31-test-session-uuid",
			expectedModelID: "test-session-uuid",
		},
		{
			name:            "legacy format single digit day",
			entireSessionID: "2026-01-05-uuid-here",
			expectedModelID: "uuid-here",
		},
		{
			name:            "legacy format with complex uuid",
			entireSessionID: "2026-11-30-a1b2c3d4_e5f6_7890",
			expectedModelID: "a1b2c3d4_e5f6_7890",
		},
		{
			name:            "legacy format edge case - exactly 11 char prefix",
			entireSessionID: "2026-01-23-x",
			expectedModelID: "x",
		},
		// Malformed legacy format - returns as-is
		{
			name:            "malformed date - missing second hyphen",
			entireSessionID: "2026-0123-uuid",
			expectedModelID: "2026-0123-uuid",
		},
		{
			name:            "malformed date - missing third hyphen",
			entireSessionID: "2026-01-23uuid",
			expectedModelID: "2026-01-23uuid",
		},
		{
			name:            "too short - only date prefix",
			entireSessionID: "2026-01-23-",
			expectedModelID: "2026-01-23-",
		},
		{
			name:            "too short - less than 11 chars",
			entireSessionID: "2026-01-23",
			expectedModelID: "2026-01-23",
		},
		{
			name:            "wrong hyphen positions",
			entireSessionID: "20260-1-23-uuid",
			expectedModelID: "20260-1-23-uuid",
		},
		{
			name:            "date with slashes instead of hyphens",
			entireSessionID: "2026/01/23-uuid",
			expectedModelID: "2026/01/23-uuid",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := ModelSessionID(tt.entireSessionID)
			if result != tt.expectedModelID {
				t.Errorf("ModelSessionID(%q) = %q, want %q", tt.entireSessionID, result, tt.expectedModelID)
			}
		})
	}
}

func TestEntireSessionID(t *testing.T) {
	tests := []struct {
		name             string
		agentSessionUUID string
		expected         string
	}{
		{
			name:             "full uuid",
			agentSessionUUID: "f736da47-b2ca-4f86-bb32-a1bbe582e464",
			expected:         "f736da47-b2ca-4f86-bb32-a1bbe582e464",
		},
		{
			name:             "short id",
			agentSessionUUID: "abc123",
			expected:         "abc123",
		},
		{
			name:             "empty uuid",
			agentSessionUUID: "",
			expected:         "",
		},
		{
			name:             "uuid with underscores",
			agentSessionUUID: "test_session_uuid_123",
			expected:         "test_session_uuid_123",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := EntireSessionID(tt.agentSessionUUID)

			// EntireSessionID is now an identity function
			if result != tt.expected {
				t.Errorf("EntireSessionID(%q) = %q, want %q", tt.agentSessionUUID, result, tt.expected)
			}
		})
	}
}

// TestRoundTrip verifies that EntireSessionID and ModelSessionID are inverses
func TestRoundTrip(t *testing.T) {
	tests := []struct {
		name string
		uuid string
	}{
		{name: "full uuid", uuid: "f736da47-b2ca-4f86-bb32-a1bbe582e464"},
		{name: "short id", uuid: "abc123"},
		{name: "uuid with underscores", uuid: "test_session_uuid_123"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// UUID -> Entire session ID -> UUID
			entireID := EntireSessionID(tt.uuid)
			extractedUUID := ModelSessionID(entireID)

			if extractedUUID != tt.uuid {
				t.Errorf("Round trip failed: %q -> EntireSessionID -> %q -> ModelSessionID -> %q",
					tt.uuid, entireID, extractedUUID)
			}
		})
	}
}

// TestBackwardsCompatibility verifies that ModelSessionID handles legacy date-prefixed IDs
func TestBackwardsCompatibility(t *testing.T) {
	// Legacy format should still extract the agent session ID
	legacyID := "2026-01-23-f736da47-b2ca-4f86-bb32-a1bbe582e464"
	expected := "f736da47-b2ca-4f86-bb32-a1bbe582e464"

	result := ModelSessionID(legacyID)
	if result != expected {
		t.Errorf("ModelSessionID(%q) = %q, want %q (backwards compatibility)", legacyID, result, expected)
	}

	// New format should return as-is
	newID := "f736da47-b2ca-4f86-bb32-a1bbe582e464"
	result = ModelSessionID(newID)
	if result != newID {
		t.Errorf("ModelSessionID(%q) = %q, want %q (new format)", newID, result, newID)
	}
}
