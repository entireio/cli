package redact

// NOTE: Tests in this file are intentionally NOT marked t.Parallel() because they
// modify package-level global state (piiConfig) via ConfigurePII/resetPIIConfig.

import (
	"strings"
	"sync"
	"testing"
)

// resetPIIConfig resets the global PII configuration between tests.
func resetPIIConfig() {
	piiConfigMu.Lock()
	defer piiConfigMu.Unlock()
	piiConfig = nil
	compiledPIIPatternsOnce = sync.Once{}
	compiledPIIPatterns = nil
}

func TestPII_EmailDetection(t *testing.T) {
	ConfigurePII(PIIConfig{
		Enabled:    true,
		Categories: map[PIICategory]bool{PIIEmail: true},
	})
	t.Cleanup(resetPIIConfig)

	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"simple email", "contact user@example.com for info", "contact [REDACTED_EMAIL] for info"},
		{"email with plus", "send to user+tag@domain.co.uk ok", "send to [REDACTED_EMAIL] ok"},
		{"email with dots", "send to first.last@company.org ok", "send to [REDACTED_EMAIL] ok"},
		{"no email", "this is normal text", "this is normal text"},
		{"multiple emails", "a@b.com and c@d.org", "[REDACTED_EMAIL] and [REDACTED_EMAIL]"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := String(tt.input)
			if got != tt.want {
				t.Errorf("String(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestPII_PhoneDetection(t *testing.T) {
	ConfigurePII(PIIConfig{
		Enabled:    true,
		Categories: map[PIICategory]bool{PIIPhone: true},
	})
	t.Cleanup(resetPIIConfig)

	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"US with dashes", "call 555-123-4567 now", "call [REDACTED_PHONE] now"},
		{"US with parens", "call (555) 123-4567 now", "call [REDACTED_PHONE] now"},
		{"US with +1", "call +1-555-123-4567 now", "call [REDACTED_PHONE] now"},
		{"US with dots", "call 555.123.4567 now", "call [REDACTED_PHONE] now"},
		{"no phone", "the number 42 is fine", "the number 42 is fine"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := String(tt.input)
			if got != tt.want {
				t.Errorf("String(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestPII_AddressDetection(t *testing.T) {
	ConfigurePII(PIIConfig{
		Enabled:    true,
		Categories: map[PIICategory]bool{PIIAddress: true},
	})
	t.Cleanup(resetPIIConfig)

	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"street", "lives at 123 Main Street ok", "lives at [REDACTED_ADDRESS] ok"},
		{"avenue", "office at 456 Oak Avenue", "office at [REDACTED_ADDRESS]"},
		{"blvd", "visit 789 Sunset Blvd today", "visit [REDACTED_ADDRESS] today"},
		{"drive", "go to 42 Pine Drive please", "go to [REDACTED_ADDRESS] please"},
		{"no address", "this is normal text", "this is normal text"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := String(tt.input)
			if got != tt.want {
				t.Errorf("String(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestPII_CustomPatterns(t *testing.T) {
	ConfigurePII(PIIConfig{
		Enabled:    true,
		Categories: map[PIICategory]bool{},
		CustomPatterns: map[string]string{
			"employee_id": `EMP-\d{6}`,
		},
	})
	t.Cleanup(resetPIIConfig)

	input := "employee EMP-123456 joined"
	want := "employee [REDACTED_EMPLOYEE_ID] joined"
	got := String(input)
	if got != want {
		t.Errorf("String(%q) = %q, want %q", input, got, want)
	}
}

func TestPII_DisabledByDefault(t *testing.T) {
	// No ConfigurePII call — PII should not be redacted
	resetPIIConfig()

	input := "contact user@example.com and call 555-123-4567"
	got := String(input)
	if got != input {
		t.Errorf("PII should not be redacted when not configured, got %q", got)
	}
}

func TestPII_CategoryToggle(t *testing.T) {
	ConfigurePII(PIIConfig{
		Enabled: true,
		Categories: map[PIICategory]bool{
			PIIEmail: true,
			PIIPhone: false, // explicitly disabled
		},
	})
	t.Cleanup(resetPIIConfig)

	input := "email user@example.com phone 555-123-4567"
	got := String(input)
	if !strings.Contains(got, "[REDACTED_EMAIL]") {
		t.Errorf("expected email redacted, got %q", got)
	}
	if !strings.Contains(got, "555-123-4567") {
		t.Errorf("expected phone preserved when category disabled, got %q", got)
	}
}

func TestPII_SecretAndPIICoexist(t *testing.T) {
	ConfigurePII(PIIConfig{
		Enabled:    true,
		Categories: map[PIICategory]bool{PIIEmail: true},
	})
	t.Cleanup(resetPIIConfig)

	// Secret detection should produce "REDACTED", email should produce "[REDACTED_EMAIL]"
	input := "key=" + highEntropySecret + " and user@example.com"
	got := String(input)
	if !strings.Contains(got, "REDACTED") {
		t.Errorf("expected secret redaction in output, got %q", got)
	}
	if !strings.Contains(got, "[REDACTED_EMAIL]") {
		t.Errorf("expected PII email redaction in output, got %q", got)
	}
	// The secret should NOT be in the output
	if strings.Contains(got, highEntropySecret) {
		t.Errorf("secret should be redacted, got %q", got)
	}
	// The email should NOT be in the output
	if strings.Contains(got, "user@example.com") {
		t.Errorf("email should be redacted, got %q", got)
	}
}

func TestPII_JSONLContent(t *testing.T) {
	ConfigurePII(PIIConfig{
		Enabled:    true,
		Categories: map[PIICategory]bool{PIIEmail: true},
	})
	t.Cleanup(resetPIIConfig)

	input := `{"content":"contact user@example.com"}`
	got, err := JSONLContent(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(got, "user@example.com") {
		t.Errorf("email should be redacted in JSONL, got %q", got)
	}
	if !strings.Contains(got, "[REDACTED_EMAIL]") {
		t.Errorf("expected [REDACTED_EMAIL] in JSONL output, got %q", got)
	}
}

func TestPII_Bytes(t *testing.T) {
	ConfigurePII(PIIConfig{
		Enabled:    true,
		Categories: map[PIICategory]bool{PIIEmail: true},
	})
	t.Cleanup(resetPIIConfig)

	input := []byte("contact user@example.com")
	got := Bytes(input)
	if strings.Contains(string(got), "user@example.com") {
		t.Errorf("email should be redacted in Bytes output, got %q", string(got))
	}
	if !strings.Contains(string(got), "[REDACTED_EMAIL]") {
		t.Errorf("expected [REDACTED_EMAIL] in Bytes output, got %q", string(got))
	}
}

func TestPII_ReplacementTokenFormat(t *testing.T) {
	tests := []struct {
		label string
		want  string
	}{
		{"", "REDACTED"},
		{"EMAIL", "[REDACTED_EMAIL]"},
		{"PHONE", "[REDACTED_PHONE]"},
		{"ADDRESS", "[REDACTED_ADDRESS]"},
		{"EMPLOYEE_ID", "[REDACTED_EMPLOYEE_ID]"},
	}
	for _, tt := range tests {
		t.Run(tt.label, func(t *testing.T) {
			got := replacementToken(tt.label)
			if got != tt.want {
				t.Errorf("replacementToken(%q) = %q, want %q", tt.label, got, tt.want)
			}
		})
	}
}
