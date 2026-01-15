package cli

import (
	"testing"
)

func TestCleanPromptForCommit(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		// Basic prefix removal
		{
			name:     "removes 'Can you ' prefix",
			input:    "Can you fix the bug",
			expected: "Fix the bug",
		},
		{
			name:     "removes 'can you ' prefix (lowercase)",
			input:    "can you fix the bug",
			expected: "Fix the bug",
		},
		{
			name:     "removes 'Please ' prefix",
			input:    "Please update the readme",
			expected: "Update the readme",
		},
		{
			name:     "removes 'please ' prefix (lowercase)",
			input:    "please update the readme",
			expected: "Update the readme",
		},
		{
			name:     "removes 'Let's ' prefix",
			input:    "Let's add a new feature",
			expected: "Add a new feature",
		},
		{
			name:     "removes 'let's ' prefix (lowercase)",
			input:    "let's add a new feature",
			expected: "Add a new feature",
		},
		{
			name:     "removes 'Could you ' prefix",
			input:    "Could you refactor this code",
			expected: "Refactor this code",
		},
		{
			name:     "removes 'could you ' prefix (lowercase)",
			input:    "could you refactor this code",
			expected: "Refactor this code",
		},
		{
			name:     "removes 'Would you ' prefix",
			input:    "Would you implement the API",
			expected: "Implement the API",
		},
		{
			name:     "removes 'would you ' prefix (lowercase)",
			input:    "would you implement the API",
			expected: "Implement the API",
		},
		{
			name:     "removes 'I want you to ' prefix",
			input:    "I want you to create a test file",
			expected: "Create a test file",
		},
		{
			name:     "removes 'I'd like you to ' prefix",
			input:    "I'd like you to optimize the query",
			expected: "Optimize the query",
		},
		{
			name:     "removes 'I need you to ' prefix",
			input:    "I need you to fix the auth flow",
			expected: "Fix the auth flow",
		},

		// Chained prefixes
		{
			name:     "removes chained prefixes 'Can you please '",
			input:    "Can you please fix the bug",
			expected: "Fix the bug",
		},
		{
			name:     "removes chained prefixes 'Could you please '",
			input:    "Could you please update the config",
			expected: "Update the config",
		},
		{
			name:     "removes chained prefixes 'Would you please '",
			input:    "Would you please add tests",
			expected: "Add tests",
		},

		// Question mark removal
		{
			name:     "removes trailing question mark",
			input:    "Can you fix this?",
			expected: "Fix this",
		},
		{
			name:     "handles prompt with no question mark",
			input:    "Fix the authentication issue",
			expected: "Fix the authentication issue",
		},

		// Capitalization
		{
			name:     "capitalizes first letter",
			input:    "fix the bug",
			expected: "Fix the bug",
		},
		{
			name:     "preserves already capitalized",
			input:    "Fix the bug",
			expected: "Fix the bug",
		},
		{
			name:     "capitalizes after prefix removal",
			input:    "please fix the bug",
			expected: "Fix the bug",
		},

		// Long prompts (no longer truncated)
		{
			name:     "long prompts are kept intact",
			input:    "This is a very long prompt that exceeds the seventy two character limit and should be truncated",
			expected: "This is a very long prompt that exceeds the seventy two character limit and should be truncated",
		},
		{
			name:     "short prompts stay intact",
			input:    "Short prompt",
			expected: "Short prompt",
		},

		// Edge cases
		{
			name:     "handles empty string",
			input:    "",
			expected: "",
		},
		{
			name:     "handles whitespace only",
			input:    "   ",
			expected: "",
		},
		{
			name:     "trims leading/trailing whitespace",
			input:    "  fix the bug  ",
			expected: "Fix the bug",
		},
		{
			name:     "handles single character after prefix removal",
			input:    "Can you x",
			expected: "X",
		},
		{
			name:     "handles prefix that leaves empty string",
			input:    "Can you ",
			expected: "",
		},
		{
			name:     "handles only question mark after prefix",
			input:    "Can you ?",
			expected: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := cleanPromptForCommit(tt.input)
			if result != tt.expected {
				t.Errorf("cleanPromptForCommit(%q) = %q, want %q", tt.input, result, tt.expected)
			}
		})
	}
}

func TestGenerateCommitMessage(t *testing.T) {
	tests := []struct {
		name     string
		prompt   string
		expected string
	}{
		{
			name:     "returns cleaned prompt",
			prompt:   "Can you fix the login bug?",
			expected: "Fix the login bug",
		},
		{
			name:     "returns default for empty prompt",
			prompt:   "",
			expected: "Claude Code session updates",
		},
		{
			name:     "returns default when cleaned prompt is empty",
			prompt:   "Can you ?",
			expected: "Claude Code session updates",
		},
		{
			name:     "returns default for whitespace only prompt",
			prompt:   "   ",
			expected: "Claude Code session updates",
		},
		{
			name:     "handles direct command prompt",
			prompt:   "Add unit tests for the auth module",
			expected: "Add unit tests for the auth module",
		},
		{
			name:     "handles polite request",
			prompt:   "Please refactor the database connection handling",
			expected: "Refactor the database connection handling",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := generateCommitMessage(tt.prompt)
			if result != tt.expected {
				t.Errorf("generateCommitMessage(%q) = %q, want %q", tt.prompt, result, tt.expected)
			}
		})
	}
}
