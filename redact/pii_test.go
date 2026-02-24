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
	if regions[0].label != labelEmail {
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
	if regions[0].label != labelPhone {
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
	if regions[0].label != labelAddress {
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
		if r.label == labelPhone {
			t.Errorf("phone should not be detected when category is disabled")
		}
	}
	hasEmail := false
	for _, r := range regions {
		if r.label == labelEmail {
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
		{labelEmail, "[REDACTED_EMAIL]"},
		{labelPhone, "[REDACTED_PHONE]"},
		{labelAddress, "[REDACTED_ADDRESS]"},
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

// =============================================================================
// Level 2 additions: edge cases and contracts
// =============================================================================

func TestDetectPII_EmptyString(t *testing.T) {
	t.Parallel()
	cfg := &PIIConfig{
		Enabled:    true,
		Categories: map[PIICategory]bool{PIIEmail: true, PIIPhone: true},
	}
	regions := detectPII(cfg, "")
	if len(regions) != 0 {
		t.Errorf("expected nil regions for empty string, got %d", len(regions))
	}
}

func TestDetectPII_InvalidAndValidCustomPatterns(t *testing.T) {
	t.Parallel()
	cfg := &PIIConfig{
		Enabled:    true,
		Categories: map[PIICategory]bool{},
		CustomPatterns: map[string]string{
			"bad":         "[invalid",
			"employee_id": `EMP-\d{6}`,
		},
	}
	regions := detectPII(cfg, "employee EMP-123456 joined")
	if len(regions) != 1 {
		t.Fatalf("expected 1 region (valid pattern should still fire despite invalid sibling), got %d", len(regions))
	}
	if regions[0].label != "EMPLOYEE_ID" {
		t.Errorf("expected label EMPLOYEE_ID, got %q", regions[0].label)
	}
}

// =============================================================================
// Level 2: Email allowlist tests (TDD — these FAIL until allowlist is implemented)
// =============================================================================

func TestDetectPII_AllowlistedEmails(t *testing.T) {
	t.Parallel()
	cfg := &PIIConfig{
		Enabled:    true,
		Categories: map[PIICategory]bool{PIIEmail: true},
	}
	// These emails appear constantly in coding transcripts and are non-sensitive.
	allowlisted := []string{
		"noreply@github.com",
		"user@users.noreply.github.com",
		"dependabot@users.noreply.github.com",
		"actions@github.com",
		"someone@noreply.github.com",
		"Noreply@GitHub.com", // case-insensitive
	}
	for _, email := range allowlisted {
		input := "from " + email + " to"
		regions := detectPII(cfg, input)
		for _, r := range regions {
			matched := input[r.start:r.end]
			if r.label == labelEmail && matched == email {
				t.Errorf("allowlisted email %q should NOT be detected as PII", email)
			}
		}
	}

	// Regular emails should still be detected.
	regions := detectPII(cfg, "contact user@example.com for info")
	hasEmail := false
	for _, r := range regions {
		if r.label == labelEmail {
			hasEmail = true
		}
	}
	if !hasEmail {
		t.Error("non-allowlisted email should still be detected")
	}
}

func TestDetectPII_GitAuthorNoreplyNotRedacted(t *testing.T) {
	t.Parallel()
	cfg := &PIIConfig{
		Enabled:    true,
		Categories: map[PIICategory]bool{PIIEmail: true},
	}
	// Simulates git log output — noreply addresses should not be redacted
	input := "Author: Bot <noreply@github.com>\nCo-Authored-By: User <user@users.noreply.github.com>"
	regions := detectPII(cfg, input)
	for _, r := range regions {
		if r.label == labelEmail {
			t.Errorf("noreply email in git author line should NOT be detected, but matched %q", input[r.start:r.end])
		}
	}
}

// =============================================================================
// Level 3 additions: #471 regression safety WITH PII enabled
// =============================================================================

func TestPIIEnabled_FilePathsStillPreserved(t *testing.T) {
	ConfigurePII(PIIConfig{
		Enabled:    true,
		Categories: map[PIICategory]bool{PIIEmail: true, PIIPhone: true},
	})
	t.Cleanup(resetPIIConfig)

	paths := []string{
		"/tmp/TestE2E_Something3407889464/001/controller.go",
		"/private/var/folders/v4/31cd3cg52_sfrpb1mbtr7q7r0000gn/T/TestE2E_Something/controller",
		"/Users/peytonmontei/.claude/projects/something.jsonl",
		"/tmp/test/controller.go\n/tmp/test/model.go\n/tmp/test/view.go",
	}
	for _, p := range paths {
		got := String(p)
		if got != p {
			t.Errorf("file path should NOT be redacted with PII enabled\n  input: %q\n  got:   %q", p, got)
		}
	}
}

func TestPIIEnabled_JSONEscapesStillPreserved(t *testing.T) {
	ConfigurePII(PIIConfig{
		Enabled:    true,
		Categories: map[PIICategory]bool{PIIEmail: true, PIIPhone: true},
	})
	t.Cleanup(resetPIIConfig)

	tests := []string{
		`controller.go\nmodel.go\nview.go`,
		`something.go\tanother.go`,
		`C:\\Users\\test\\file.go`,
	}
	for _, input := range tests {
		got := String(input)
		if got != input {
			t.Errorf("JSON escape should NOT be corrupted with PII enabled\n  input: %q\n  got:   %q", input, got)
		}
	}
}

func TestPIIEnabled_JSONLPathFieldsStillSkipped(t *testing.T) {
	ConfigurePII(PIIConfig{
		Enabled:    true,
		Categories: map[PIICategory]bool{PIIEmail: true},
	})
	t.Cleanup(resetPIIConfig)

	input := `{"file_path":"/private/var/folders/v4/31cd3cg52_sfrpb1mbtr7q7r0000gn/T/test/controller.go","cwd":"/private/var/folders/v4/31cd3cg52_sfrpb1mbtr7q7r0000gn/T/test","content":"normal text here"}`
	got, err := JSONLContent(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(got, "REDACTED") {
		t.Errorf("JSONL path fields should NOT be redacted with PII enabled, got: %s", got)
	}
}

func TestPIIEnabled_SecretPatternExcludesSlash(t *testing.T) {
	ConfigurePII(PIIConfig{
		Enabled:    true,
		Categories: map[PIICategory]bool{PIIEmail: true, PIIPhone: true},
	})
	t.Cleanup(resetPIIConfig)

	// This path was being redacted when / was in secretPattern
	input := "/private/var/folders/v4/31cd3cg52_sfrpb1mbtr7q7r0000gn/T/TestE2E_Something/controller"
	got := String(input)
	if got != input {
		t.Errorf("path with slashes should NOT be redacted\n  input: %q\n  got:   %q", input, got)
	}
}

// =============================================================================
// Level 3 additions: overlap and JSONL interaction
// =============================================================================

func TestPIIIntegration_OverlappingSecretAndPII(t *testing.T) {
	ConfigurePII(PIIConfig{
		Enabled:    true,
		Categories: map[PIICategory]bool{PIIEmail: true},
	})
	t.Cleanup(resetPIIConfig)

	// Secret and email adjacent (not overlapping) — both should be redacted
	input := "key=" + highEntropySecret + " user@example.com"
	got := String(input)
	if strings.Contains(got, highEntropySecret) {
		t.Error("secret should be redacted")
	}
	if strings.Contains(got, "user@example.com") {
		t.Error("email should be redacted")
	}
	if !strings.Contains(got, "REDACTED") {
		t.Error("expected at least one REDACTED token")
	}
	if !strings.Contains(got, "[REDACTED_EMAIL]") {
		t.Error("expected [REDACTED_EMAIL] token")
	}
}

func TestPIIIntegration_JSONLSkippedFieldWithEmail(t *testing.T) {
	ConfigurePII(PIIConfig{
		Enabled:    true,
		Categories: map[PIICategory]bool{PIIEmail: true},
	})
	t.Cleanup(resetPIIConfig)

	// Email in file_path field should NOT be redacted (field is skipped).
	// Email in content field SHOULD be redacted.
	input := `{"file_path":"user@example.com/project/file.go","content":"contact admin@test.org"}`
	got, err := JSONLContent(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(got, "user@example.com") {
		t.Errorf("email in file_path should NOT be redacted, got: %s", got)
	}
	if strings.Contains(got, "admin@test.org") {
		t.Errorf("email in content should be redacted, got: %s", got)
	}
}
