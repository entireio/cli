package sessionid

import (
	"strings"
	"testing"
	"time"
)

func TestModelSessionID(t *testing.T) {
	tests := []struct {
		name             string
		entireSessionID  string
		expectedModelID  string
	}{
		// Valid format - extracts UUID
		{
			name:            "valid format with full uuid",
			entireSessionID: "2026-01-23-f736da47-b2ca-4f86-bb32-a1bbe582e464",
			expectedModelID: "f736da47-b2ca-4f86-bb32-a1bbe582e464",
		},
		{
			name:            "valid format with short uuid",
			entireSessionID: "2026-01-23-abc123",
			expectedModelID: "abc123",
		},
		{
			name:            "valid format different year",
			entireSessionID: "2025-12-31-test-session-uuid",
			expectedModelID: "test-session-uuid",
		},
		{
			name:            "valid format single digit day",
			entireSessionID: "2026-01-05-uuid-here",
			expectedModelID: "uuid-here",
		},
		{
			name:            "valid format with complex uuid",
			entireSessionID: "2026-11-30-a1b2c3d4_e5f6_7890",
			expectedModelID: "a1b2c3d4_e5f6_7890",
		},
		// Invalid format - returns as-is (backwards compatibility)
		{
			name:            "no date prefix - plain uuid",
			entireSessionID: "f736da47-b2ca-4f86-bb32-a1bbe582e464",
			expectedModelID: "f736da47-b2ca-4f86-bb32-a1bbe582e464",
		},
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
			name:            "empty string",
			entireSessionID: "",
			expectedModelID: "",
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
		{
			name:            "valid format edge case - exactly 11 char prefix",
			entireSessionID: "2026-01-23-x",
			expectedModelID: "x",
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
	}{
		{name: "full uuid", agentSessionUUID: "f736da47-b2ca-4f86-bb32-a1bbe582e464"},
		{name: "short id", agentSessionUUID: "abc123"},
		{name: "empty uuid", agentSessionUUID: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := EntireSessionID(tt.agentSessionUUID)

			// Verify format: YYYY-MM-DD-<uuid>
			expectedPrefix := time.Now().Format("2006-01-02") + "-"
			if !strings.HasPrefix(result, expectedPrefix) {
				t.Errorf("EntireSessionID(%q) = %q, expected to start with %q", tt.agentSessionUUID, result, expectedPrefix)
			}

			// Verify UUID is appended correctly
			expectedSuffix := tt.agentSessionUUID
			if !strings.HasSuffix(result, expectedSuffix) {
				t.Errorf("EntireSessionID(%q) = %q, expected to end with %q", tt.agentSessionUUID, result, expectedSuffix)
			}

			// Verify complete format
			expected := expectedPrefix + tt.agentSessionUUID
			if result != expected {
				t.Errorf("EntireSessionID(%q) = %q, want %q", tt.agentSessionUUID, result, expected)
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
