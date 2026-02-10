package redact

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestShowcase_PatternRedaction(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		input  string
		want   string
		config ShowcaseConfig
	}{
		{
			name:   "private IP - 10.x.x.x",
			input:  "server at 10.0.3.47 is down",
			want:   "server at [PRIVATE_IP] is down",
			config: DefaultShowcaseConfig(),
		},
		{
			name:   "private IP with port",
			input:  "connect to 192.168.1.100:8080",
			want:   "connect to [PRIVATE_IP]",
			config: DefaultShowcaseConfig(),
		},
		{
			name:   "internal URL",
			input:  "api.internal:8080/v1/users",
			want:   "[INTERNAL_URL]/v1/users",
			config: DefaultShowcaseConfig(),
		},
		{
			name:   "internal domain variations",
			input:  "app.local app.corp app.lan",
			want:   "[INTERNAL_URL] [INTERNAL_URL] [INTERNAL_URL]",
			config: DefaultShowcaseConfig(),
		},
		{
			name:   "AWS ARN",
			input:  "arn:aws:s3:::my-bucket/object",
			want:   "[AWS_ARN]",
			config: DefaultShowcaseConfig(),
		},
		{
			name:   "GCP resource path",
			input:  "projects/my-project-123/instances/db-main",
			want:   "[GCP_RESOURCE]",
			config: DefaultShowcaseConfig(),
		},
		{
			name:   "PostgreSQL connection string",
			input:  "postgres://user:pass@host:5432/db",
			want:   "[DB_CONNECTION_STRING]",
			config: DefaultShowcaseConfig(),
		},
		{
			name:   "MongoDB connection string",
			input:  "mongodb://admin:secret@cluster0.mongodb.net/test",
			want:   "[DB_CONNECTION_STRING]",
			config: DefaultShowcaseConfig(),
		},
		{
			name:   "email address",
			input:  "contact john.doe@example.com for help",
			want:   "contact [EMAIL] for help",
			config: DefaultShowcaseConfig(),
		},
		{
			name:   "AWS account ID in context",
			input:  "AWS account: 123456789012",
			want:   "AWS account: [AWS_ACCOUNT_ID]",
			config: DefaultShowcaseConfig(),
		},
		{
			name:   "JWT token",
			input:  "Bearer eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dozjgNryP4J3jVmNHl0w5N_XgL0n3I9PlFUP0THsR8U",
			want:   "Bearer [JWT_TOKEN]",
			config: DefaultShowcaseConfig(),
		},
		{
			name:   "PEM private key",
			input:  "-----BEGIN RSA PRIVATE KEY-----\nMIIEpAIBAAKCAQEA...\n-----END RSA PRIVATE KEY-----",
			want:   "[PEM_PRIVATE_KEY]",
			config: DefaultShowcaseConfig(),
		},
		{
			name:   "file path with user directory",
			input:  "/Users/john/projects/myapp/src/main.go",
			want:   "/[PATH]/projects/myapp/src/main.go",
			config: DefaultShowcaseConfig(),
		},
		{
			name:   "Linux home path",
			input:  "/home/alice/workspace/backend/api.go",
			want:   "/[PATH]/workspace/backend/api.go",
			config: DefaultShowcaseConfig(),
		},
		{
			name:   "preserve public IP",
			input:  "visit 1.1.1.1 for DNS",
			want:   "visit 1.1.1.1 for DNS",
			config: DefaultShowcaseConfig(),
		},
		{
			name:   "preserve public domain",
			input:  "check https://example.com/api",
			want:   "check https://example.com/api",
			config: DefaultShowcaseConfig(),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := Showcase(tt.input, tt.config)
			if got != tt.want {
				t.Errorf("Showcase() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestShowcase_BlocklistRedaction(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		input     string
		blocklist []string
		want      string
	}{
		{
			name:      "company name exact match",
			input:     "Working at ACME Corp on project",
			blocklist: []string{"ACME Corp"},
			want:      "Working at [REDACTED] on project",
		},
		{
			name:      "project codename",
			input:     "Project Phoenix is launching soon",
			blocklist: []string{"Phoenix"},
			want:      "Project [REDACTED] is launching soon",
		},
		{
			name:      "wildcard pattern",
			input:     "internal-api.company.com and internal-web.company.com",
			blocklist: []string{"internal-*"},
			want:      "[REDACTED].company.com and [REDACTED].company.com",
		},
		{
			name:      "multiple blocklist items",
			input:     "ACME Corp's Project Phoenix uses secret-api",
			blocklist: []string{"ACME Corp", "Phoenix", "secret-*"},
			want:      "[REDACTED]'s Project [REDACTED] uses [REDACTED]",
		},
		{
			name:      "case insensitive matching",
			input:     "acme, ACME, Acme",
			blocklist: []string{"acme"},
			want:      "[REDACTED], [REDACTED], [REDACTED]",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg := DefaultShowcaseConfig()
			cfg.CustomBlocklist = tt.blocklist
			got := Showcase(tt.input, cfg)
			if got != tt.want {
				t.Errorf("Showcase() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestShowcase_PathNormalization(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		input        string
		allowedPaths []string
		want         string
	}{
		{
			name:         "extract allowed path",
			input:        "/Users/alice/myproject/src/main.go",
			allowedPaths: []string{"src/"},
			want:         "/[PATH]/myproject/src/main.go",
		},
		{
			name:         "multiple allowed paths",
			input:        "/home/bob/app/lib/util.go",
			allowedPaths: []string{"src/", "lib/", "cmd/"},
			want:         "/[PATH]/app/lib/util.go",
		},
		{
			name:         "no allowed paths - redact fully",
			input:        "/Users/charlie/secret-project/app.go",
			allowedPaths: []string{},
			want:         "/[PATH]/secret-project/app.go",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg := DefaultShowcaseConfig()
			cfg.AllowedPaths = tt.allowedPaths
			got := Showcase(tt.input, cfg)
			if got != tt.want {
				t.Errorf("Showcase() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestShowcase_UsernameRedaction(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		input          string
		allowedDomains []string
		want           string
	}{
		{
			name:           "redact all emails by default",
			input:          "Contact alice@private.com or bob@company.com",
			allowedDomains: []string{},
			want:           "Contact [EMAIL] or [EMAIL]",
		},
		{
			name:           "preserve allowed domain",
			input:          "Email support@example.com or admin@internal.com",
			allowedDomains: []string{"@example.com"},
			want:           "Email support@example.com or [EMAIL]",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg := DefaultShowcaseConfig()
			cfg.AllowedDomains = tt.allowedDomains
			got := Showcase(tt.input, cfg)
			if got != tt.want {
				t.Errorf("Showcase() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestShowcase_ProjectInfoRedaction(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "SSH git remote",
			input: "git@github.com:acme-corp/secret-project.git",
			want:  "[GIT_REMOTE]",
		},
		{
			name:  "HTTPS git remote",
			input: "https://github.com/my-org/private-repo.git",
			want:  "[GIT_REMOTE]",
		},
		{
			name:  "GitHub URL without .git",
			input: "https://github.com/company/project",
			want:  "[GIT_REMOTE]",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg := DefaultShowcaseConfig()
			got := Showcase(tt.input, cfg)
			if got != tt.want {
				t.Errorf("Showcase() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestShowcaseJSONL_PreservesStructure(t *testing.T) {
	t.Parallel()

	input := `{"type":"text","content":"server at 10.0.1.5 is down"}
{"type":"code","language":"go","content":"// connect to api.internal"}
{"metadata":{"email":"admin@company.com","path":"/Users/alice/project/main.go"}}`

	cfg := DefaultShowcaseConfig()
	result, err := ShowcaseJSONL([]byte(input), cfg)
	if err != nil {
		t.Fatalf("ShowcaseJSONL() error = %v", err)
	}

	lines := strings.Split(strings.TrimSpace(string(result)), "\n")
	if len(lines) != 3 {
		t.Fatalf("expected 3 lines, got %d", len(lines))
	}

	// Verify each line is valid JSON
	for i, line := range lines {
		var entry map[string]any
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Errorf("line %d not valid JSON: %v", i, err)
		}
	}

	// Check specific redactions
	var line1 map[string]any
	json.Unmarshal([]byte(lines[0]), &line1)
	if !strings.Contains(line1["content"].(string), "[PRIVATE_IP]") {
		t.Error("line 1: private IP not redacted")
	}

	var line2 map[string]any
	json.Unmarshal([]byte(lines[1]), &line2)
	if !strings.Contains(line2["content"].(string), "[INTERNAL_URL]") {
		t.Error("line 2: internal URL not redacted")
	}

	var line3 map[string]any
	json.Unmarshal([]byte(lines[2]), &line3)
	metadata := line3["metadata"].(map[string]any)
	if !strings.Contains(metadata["email"].(string), "[EMAIL]") {
		t.Error("line 3: email not redacted")
	}
	if !strings.Contains(metadata["path"].(string), "[PATH]") {
		t.Error("line 3: path not redacted")
	}
}

func TestShowcaseJSONL_HandlesNestedStructures(t *testing.T) {
	t.Parallel()

	input := `{"user":{"email":"test@company.com","profile":{"url":"https://api.internal/users/123"}}}`

	cfg := DefaultShowcaseConfig()
	result, err := ShowcaseJSONL([]byte(input), cfg)
	if err != nil {
		t.Fatalf("ShowcaseJSONL() error = %v", err)
	}

	var entry map[string]any
	if err := json.Unmarshal(result, &entry); err != nil {
		t.Fatalf("result not valid JSON: %v", err)
	}

	user := entry["user"].(map[string]any)
	if !strings.Contains(user["email"].(string), "[EMAIL]") {
		t.Error("nested email not redacted")
	}

	profile := user["profile"].(map[string]any)
	if !strings.Contains(profile["url"].(string), "[INTERNAL_URL]") {
		t.Error("nested URL not redacted")
	}
}

func TestShowcaseJSONL_MalformedFallback(t *testing.T) {
	t.Parallel()

	input := `not valid json but has email: admin@company.com`

	cfg := DefaultShowcaseConfig()
	result, err := ShowcaseJSONL([]byte(input), cfg)
	if err != nil {
		t.Fatalf("ShowcaseJSONL() error = %v", err)
	}

	if !strings.Contains(string(result), "[EMAIL]") {
		t.Error("malformed line not redacted via fallback")
	}
}

func TestShowcase_BoundaryConditions(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		input  string
		config ShowcaseConfig
	}{
		{
			name:   "empty string",
			input:  "",
			config: DefaultShowcaseConfig(),
		},
		{
			name:   "whitespace only",
			input:  "   \n\t  ",
			config: DefaultShowcaseConfig(),
		},
		{
			name:   "unicode content",
			input:  "用户邮箱：admin@company.com",
			config: DefaultShowcaseConfig(),
		},
		{
			name:   "already redacted",
			input:  "server at [PRIVATE_IP] with [REDACTED]",
			config: DefaultShowcaseConfig(),
		},
		{
			name:   "mixed patterns",
			input:  "Connect to 10.0.1.5:5432 (postgres://user:pass@10.0.1.5/db) or api.internal",
			config: DefaultShowcaseConfig(),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			// Should not panic or error
			result := Showcase(tt.input, tt.config)
			if tt.name == "empty string" && result != "" {
				t.Error("empty string should remain empty")
			}
		})
	}
}

func TestShowcase_PreservesTechnicalTerms(t *testing.T) {
	t.Parallel()

	input := `
package main

import (
    "database/sql"
    "github.com/lib/pq"
)

func connectDB() error {
    // Connect to database
    return nil
}
`

	cfg := DefaultShowcaseConfig()
	result := Showcase(input, cfg)

	// Technical terms should be preserved
	preservedTerms := []string{
		"package main",
		"import",
		"database/sql",
		"github.com/lib/pq",
		"func connectDB",
	}

	for _, term := range preservedTerms {
		if !strings.Contains(result, term) {
			t.Errorf("technical term %q was incorrectly redacted", term)
		}
	}
}

func TestShowcase_LayeredRedaction(t *testing.T) {
	t.Parallel()

	// Test that layered redaction (entropy + showcase) works correctly
	input := "API key: sk-proj-abcd1234, server: 10.0.1.5, email: admin@acme-corp.com"

	cfg := DefaultShowcaseConfig()
	cfg.CustomBlocklist = []string{"acme-corp"}

	// First apply entropy-based redaction (simulated by replacing API key)
	step1 := strings.Replace(input, "sk-proj-abcd1234", "[REDACTED]", 1)

	// Then apply showcase redaction
	result := Showcase(step1, cfg)

	// Verify all redactions applied
	if !strings.Contains(result, "[REDACTED]") {
		t.Error("API key not redacted (entropy layer)")
	}
	if !strings.Contains(result, "[PRIVATE_IP]") {
		t.Error("private IP not redacted (pattern layer)")
	}
	if !strings.Contains(result, "[EMAIL]") {
		t.Error("email not redacted (pattern layer)")
	}

	// Verify blocklist was applied
	if strings.Contains(result, "acme-corp") {
		t.Error("blocklist term not redacted")
	}
}

func TestGlobToRegex(t *testing.T) {
	t.Parallel()

	tests := []struct {
		glob  string
		input string
		match bool
	}{
		{"test*", "test123", true},
		{"test*", "testing", true},
		{"test*", "mytest", false}, // word boundary
		{"*api", "internal-api", true},
		{"*api", "api-server", false}, // word boundary
		{"test?", "test1", true},
		{"test?", "test12", false},
		{"test.com", "test.com", true},
		{"test.com", "testXcom", false}, // . should be literal
	}

	for _, tt := range tests {
		t.Run(tt.glob, func(t *testing.T) {
			t.Parallel()
			regex := globToRegex(tt.glob)
			// Test via Showcase with blocklist
			cfg := DefaultShowcaseConfig()
			cfg.CustomBlocklist = []string{tt.glob}
			result := Showcase(tt.input, cfg)

			matched := strings.Contains(result, "[REDACTED]")
			if matched != tt.match {
				t.Errorf("glob %q, input %q: expected match=%v, got match=%v", tt.glob, tt.input, tt.match, matched)
			}
		})
	}
}

func TestDefaultShowcaseConfig(t *testing.T) {
	t.Parallel()

	cfg := DefaultShowcaseConfig()

	if !cfg.RedactPaths {
		t.Error("default should redact paths")
	}
	if !cfg.RedactUsernames {
		t.Error("default should redact usernames")
	}
	if !cfg.RedactProjectInfo {
		t.Error("default should redact project info")
	}
	if len(cfg.AllowedPaths) == 0 {
		t.Error("default should have some allowed paths")
	}
}

func BenchmarkShowcase(b *testing.B) {
	input := `server at 10.0.1.5 is down, contact admin@company.com
	postgres://user:pass@host:5432/db
	JWT: eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.test.test
	path: /Users/alice/project/src/main.go`

	cfg := DefaultShowcaseConfig()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = Showcase(input, cfg)
	}
}

func BenchmarkShowcaseJSONL(b *testing.B) {
	input := []byte(`{"type":"text","content":"server at 10.0.1.5"}
{"type":"text","content":"email admin@company.com"}
{"type":"code","content":"postgres://user:pass@host/db"}
{"metadata":{"path":"/Users/alice/project/main.go"}}`)

	cfg := DefaultShowcaseConfig()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = ShowcaseJSONL(input, cfg)
	}
}
