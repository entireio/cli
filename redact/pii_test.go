package redact

import (
	"strings"
	"testing"
)

// =============================================================================
// Level 1: Pure regex tests — no config, no globals, t.Parallel()
// =============================================================================

func TestEmailRegex(t *testing.T) {
	t.Parallel()
	match := []string{
		"user@example.com",
		"user+tag@domain.co.uk",
		"first.last@company.org",
		"a@b.com",
	}
	noMatch := []string{
		"not an email",
		"@missing.local",
		"missing@",
		"no-at-sign-here",
	}
	for _, s := range match {
		if !emailRegex.MatchString(s) {
			t.Errorf("emailRegex should match %q", s)
		}
	}
	for _, s := range noMatch {
		if emailRegex.MatchString(s) {
			t.Errorf("emailRegex should NOT match %q", s)
		}
	}
}

func TestPhoneRegex(t *testing.T) {
	t.Parallel()
	match := []string{
		"555-123-4567",
		"(555) 123-4567",
		"+1-555-123-4567",
		"+1.555.123.4567",
		"1-555-123-4567",
		"555 123 4567",
	}
	noMatch := []string{
		"42",
		"12345",
		"not a phone",
		"1.234.567.8901",   // version-like dotted decimal
		"192.168.001.0001", // IP-like dotted decimal
		"555.123.4567",     // bare dots without +1 prefix (intentionally rejected)
	}
	for _, s := range match {
		if !phoneRegex.MatchString(s) {
			t.Errorf("phoneRegex should match %q", s)
		}
	}
	for _, s := range noMatch {
		if phoneRegex.MatchString(s) {
			t.Errorf("phoneRegex should NOT match %q", s)
		}
	}
}

func TestAddressRegex(t *testing.T) {
	t.Parallel()
	match := []string{
		"123 Main Street",
		"456 Oak Avenue",
		"789 Sunset Blvd",
		"42 Pine Drive",
	}
	noMatch := []string{
		"this is normal text",
		"123 lowercase street",
		"no number Street",
	}
	for _, s := range match {
		if !addressRegex.MatchString(s) {
			t.Errorf("addressRegex should match %q", s)
		}
	}
	for _, s := range noMatch {
		if addressRegex.MatchString(s) {
			t.Errorf("addressRegex should NOT match %q", s)
		}
	}
}

// =============================================================================
// Level 2: detectPII unit tests — explicit config, no globals, t.Parallel()
// =============================================================================

func TestDetectPII_EmailRegions(t *testing.T) {
	t.Parallel()
	cfg := &PIIConfig{
		Enabled:    true,
		Categories: map[PIICategory]bool{PIIEmail: true},
	}
	input := "contact user@example.com for info"
	regions := detectPII(cfg, input)
	if len(regions) != 1 {
		t.Fatalf("expected 1 region, got %d", len(regions))
	}
	if regions[0].label != "EMAIL" {
		t.Errorf("expected label EMAIL, got %q", regions[0].label)
	}
	got := input[regions[0].start:regions[0].end]
	if got != "user@example.com" {
		t.Errorf("expected matched text %q, got %q", "user@example.com", got)
	}
}

func TestDetectPII_PhoneRegions(t *testing.T) {
	t.Parallel()
	cfg := &PIIConfig{
		Enabled:    true,
		Categories: map[PIICategory]bool{PIIPhone: true},
	}
	regions := detectPII(cfg, "call 555-123-4567 now")
	if len(regions) != 1 {
		t.Fatalf("expected 1 region, got %d", len(regions))
	}
	if regions[0].label != "PHONE" {
		t.Errorf("expected label PHONE, got %q", regions[0].label)
	}
}

func TestDetectPII_AddressRegions(t *testing.T) {
	t.Parallel()
	cfg := &PIIConfig{
		Enabled:    true,
		Categories: map[PIICategory]bool{PIIAddress: true},
	}
	regions := detectPII(cfg, "lives at 123 Main Street ok")
	if len(regions) != 1 {
		t.Fatalf("expected 1 region, got %d", len(regions))
	}
	if regions[0].label != "ADDRESS" {
		t.Errorf("expected label ADDRESS, got %q", regions[0].label)
	}
}

func TestDetectPII_CategoryToggle(t *testing.T) {
	t.Parallel()
	cfg := &PIIConfig{
		Enabled:    true,
		Categories: map[PIICategory]bool{PIIEmail: true, PIIPhone: false},
	}
	regions := detectPII(cfg, "email user@example.com phone 555-123-4567")
	for _, r := range regions {
		if r.label == "PHONE" {
			t.Errorf("phone should not be detected when category is disabled")
		}
	}
	hasEmail := false
	for _, r := range regions {
		if r.label == "EMAIL" {
			hasEmail = true
		}
	}
	if !hasEmail {
		t.Error("expected at least one EMAIL region")
	}
}

func TestDetectPII_CustomPatterns(t *testing.T) {
	t.Parallel()
	cfg := &PIIConfig{
		Enabled:        true,
		Categories:     map[PIICategory]bool{},
		CustomPatterns: map[string]string{"employee_id": `EMP-\d{6}`},
	}
	regions := detectPII(cfg, "employee EMP-123456 joined")
	if len(regions) != 1 {
		t.Fatalf("expected 1 region, got %d", len(regions))
	}
	if regions[0].label != "EMPLOYEE_ID" {
		t.Errorf("expected label EMPLOYEE_ID, got %q", regions[0].label)
	}
}

func TestDetectPII_NilConfig(t *testing.T) {
	t.Parallel()
	regions := detectPII(nil, "user@example.com 555-123-4567")
	if len(regions) != 0 {
		t.Errorf("expected no regions with nil config, got %d", len(regions))
	}
}

func TestDetectPII_Disabled(t *testing.T) {
	t.Parallel()
	cfg := &PIIConfig{
		Enabled:    false,
		Categories: map[PIICategory]bool{PIIEmail: true},
	}
	regions := detectPII(cfg, "user@example.com")
	if len(regions) != 0 {
		t.Errorf("expected no regions when disabled, got %d", len(regions))
	}
}

func TestDetectPII_InvalidCustomPattern(t *testing.T) {
	t.Parallel()
	cfg := &PIIConfig{
		Enabled:        true,
		Categories:     map[PIICategory]bool{},
		CustomPatterns: map[string]string{"bad": "[invalid"},
	}
	regions := detectPII(cfg, "some text")
	if len(regions) != 0 {
		t.Errorf("expected no regions with invalid custom pattern, got %d", len(regions))
	}
}

func TestDetectPII_MultipleMatches(t *testing.T) {
	t.Parallel()
	cfg := &PIIConfig{
		Enabled:    true,
		Categories: map[PIICategory]bool{PIIEmail: true},
	}
	regions := detectPII(cfg, "a@b.com and c@d.org")
	if len(regions) != 2 {
		t.Fatalf("expected 2 regions, got %d", len(regions))
	}
}

// =============================================================================
// Level 3: Integration smoke tests through String() — needs global, few cases
// =============================================================================

// NOTE: Level 3 tests modify global state via ConfigurePII, so they
// are NOT t.Parallel(). Keep this section small — detailed coverage
// lives in Level 1 (regex) and Level 2 (detectPII).

// resetPIIConfig resets the global PII configuration between tests.
func resetPIIConfig() {
	piiConfigMu.Lock()
	defer piiConfigMu.Unlock()
	piiConfig = nil
}

func TestPIIIntegration_EmailThroughString(t *testing.T) {
	ConfigurePII(PIIConfig{
		Enabled:    true,
		Categories: map[PIICategory]bool{PIIEmail: true},
	})
	t.Cleanup(resetPIIConfig)

	got := String("contact user@example.com for info")
	want := "contact [REDACTED_EMAIL] for info"
	if got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
}

func TestPIIIntegration_SecretAndPIICoexist(t *testing.T) {
	ConfigurePII(PIIConfig{
		Enabled:    true,
		Categories: map[PIICategory]bool{PIIEmail: true},
	})
	t.Cleanup(resetPIIConfig)

	input := "key=" + highEntropySecret + " and user@example.com"
	got := String(input)
	if strings.Contains(got, highEntropySecret) {
		t.Errorf("secret should be redacted, got %q", got)
	}
	if strings.Contains(got, "user@example.com") {
		t.Errorf("email should be redacted, got %q", got)
	}
}

func TestPIIIntegration_DisabledByDefault(t *testing.T) {
	resetPIIConfig()
	t.Cleanup(resetPIIConfig)

	input := "contact user@example.com and call 555-123-4567"
	got := String(input)
	if got != input {
		t.Errorf("PII should not be redacted when not configured, got %q", got)
	}
}

func TestPIIIntegration_JSONLContent(t *testing.T) {
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
}

func TestPIIIntegration_Bytes(t *testing.T) {
	ConfigurePII(PIIConfig{
		Enabled:    true,
		Categories: map[PIICategory]bool{PIIEmail: true},
	})
	t.Cleanup(resetPIIConfig)

	got := Bytes([]byte("contact user@example.com"))
	if !strings.Contains(string(got), "[REDACTED_EMAIL]") {
		t.Errorf("expected [REDACTED_EMAIL] in Bytes output, got %q", string(got))
	}
}

func TestPII_ReplacementTokenFormat(t *testing.T) {
	t.Parallel()
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
			t.Parallel()
			got := replacementToken(tt.label)
			if got != tt.want {
				t.Errorf("replacementToken(%q) = %q, want %q", tt.label, got, tt.want)
			}
		})
	}
}
