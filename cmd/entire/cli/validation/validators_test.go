package validation

import (
	"strings"
	"testing"
)

func TestValidateSessionID(t *testing.T) {
	tests := []struct {
		name      string
		sessionID string
		wantErr   bool
		errMsg    string
	}{
		// Valid cases
		{
			name:      "valid session ID with date prefix and uuid",
			sessionID: "2026-01-25-f736da47-b2ca-4f86-bb32-a1bbe582e464",
			wantErr:   false,
		},
		{
			name:      "valid session ID with uuid only",
			sessionID: "f736da47-b2ca-4f86-bb32-a1bbe582e464",
			wantErr:   false,
		},
		{
			name:      "valid session ID with special characters",
			sessionID: "session-2026.01.25_test@123",
			wantErr:   false,
		},
		// Empty string (security-critical)
		{
			name:      "empty session ID",
			sessionID: "",
			wantErr:   true,
			errMsg:    "session ID cannot be empty",
		},
		// Path separators (security-critical - path traversal prevention)
		{
			name:      "session ID with forward slash",
			sessionID: "session/123",
			wantErr:   true,
			errMsg:    "contains path separators",
		},
		{
			name:      "session ID with backslash",
			sessionID: "session\\123",
			wantErr:   true,
			errMsg:    "contains path separators",
		},
		{
			name:      "path traversal attempt",
			sessionID: "../../etc/passwd",
			wantErr:   true,
			errMsg:    "contains path separators",
		},
		{
			name:      "absolute unix path",
			sessionID: "/etc/passwd",
			wantErr:   true,
			errMsg:    "contains path separators",
		},
		{
			name:      "absolute windows path",
			sessionID: "C:\\Windows\\System32",
			wantErr:   true,
			errMsg:    "contains path separators",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateSessionID(tt.sessionID)
			if tt.wantErr {
				if err == nil {
					t.Errorf("ValidateSessionID(%q) expected error containing %q, got nil", tt.sessionID, tt.errMsg)
					return
				}
				if !strings.Contains(err.Error(), tt.errMsg) {
					t.Errorf("ValidateSessionID(%q) error = %q, want error containing %q", tt.sessionID, err.Error(), tt.errMsg)
				}
			} else if err != nil {
				t.Errorf("ValidateSessionID(%q) unexpected error: %v", tt.sessionID, err)
			}
		})
	}
}

func TestValidateToolUseID(t *testing.T) {
	tests := []struct {
		name      string
		toolUseID string
		wantErr   bool
		errMsg    string
	}{
		// Valid cases
		{
			name:      "valid uuid format",
			toolUseID: "f736da47-b2ca-4f86-bb32-a1bbe582e464",
			wantErr:   false,
		},
		{
			name:      "valid anthropic tool use id format",
			toolUseID: "toolu_abc123def456",
			wantErr:   false,
		},
		{
			name:      "valid alphanumeric only",
			toolUseID: "abc123DEF456",
			wantErr:   false,
		},
		{
			name:      "valid with mixed underscores and hyphens",
			toolUseID: "tool_use-id-123",
			wantErr:   false,
		},
		{
			name:      "empty tool use ID is allowed",
			toolUseID: "",
			wantErr:   false,
		},
		// Invalid cases - security-critical
		{
			name:      "path traversal attempt",
			toolUseID: "../../../etc/passwd",
			wantErr:   true,
			errMsg:    "must be alphanumeric with underscores/hyphens only",
		},
		{
			name:      "forward slash",
			toolUseID: "tool/use",
			wantErr:   true,
			errMsg:    "must be alphanumeric with underscores/hyphens only",
		},
		{
			name:      "backslash",
			toolUseID: "tool\\use",
			wantErr:   true,
			errMsg:    "must be alphanumeric with underscores/hyphens only",
		},
		// Invalid cases - common errors
		{
			name:      "space in ID",
			toolUseID: "tool use id",
			wantErr:   true,
			errMsg:    "must be alphanumeric with underscores/hyphens only",
		},
		{
			name:      "dot in ID",
			toolUseID: "tool.use.id",
			wantErr:   true,
			errMsg:    "must be alphanumeric with underscores/hyphens only",
		},
		{
			name:      "special characters",
			toolUseID: "tool@use!id",
			wantErr:   true,
			errMsg:    "must be alphanumeric with underscores/hyphens only",
		},
		// Invalid cases - control characters
		{
			name:      "null byte",
			toolUseID: "tool\x00use",
			wantErr:   true,
			errMsg:    "must be alphanumeric with underscores/hyphens only",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateToolUseID(tt.toolUseID)
			if tt.wantErr {
				if err == nil {
					t.Errorf("ValidateToolUseID(%q) expected error containing %q, got nil", tt.toolUseID, tt.errMsg)
					return
				}
				if !strings.Contains(err.Error(), tt.errMsg) {
					t.Errorf("ValidateToolUseID(%q) error = %q, want error containing %q", tt.toolUseID, err.Error(), tt.errMsg)
				}
			} else if err != nil {
				t.Errorf("ValidateToolUseID(%q) unexpected error: %v", tt.toolUseID, err)
			}
		})
	}
}

// TestValidateAgentID has minimal tests since it uses identical logic to ValidateToolUseID
func TestValidateAgentID(t *testing.T) {
	tests := []struct {
		name    string
		agentID string
		wantErr bool
	}{
		{name: "valid agent ID", agentID: "agent-test-123", wantErr: false},
		{name: "valid uuid format", agentID: "a1b2c3d4-e5f6-7890-abcd-ef1234567890", wantErr: false},
		{name: "empty is allowed", agentID: "", wantErr: false},
		{name: "slash rejected", agentID: "agent/test", wantErr: true},
		{name: "dot rejected", agentID: "agent.test", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateAgentID(tt.agentID)
			if (err != nil) != tt.wantErr {
				t.Errorf("ValidateAgentID(%q) error = %v, wantErr %v", tt.agentID, err, tt.wantErr)
			}
		})
	}
}
