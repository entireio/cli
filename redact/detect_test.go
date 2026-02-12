package redact

import (
	"strings"
	"testing"
)

// --- AWS Keys ---

func TestPatternDetection_AWSAccessKey(t *testing.T) {
	t.Parallel()
	// AKIAIOSFODNN7EXAMPLE has entropy ~3.7, below the 4.5 threshold.
	// Must be caught by pattern detection.
	input := "aws_access_key_id = AKIAIOSFODNN7EXAMPLE"
	result := String(input)
	if strings.Contains(result, "AKIAIOSFODNN7EXAMPLE") {
		t.Errorf("AWS access key should have been redacted, got %q", result)
	}
	if !strings.Contains(result, "REDACTED") {
		t.Errorf("expected REDACTED in output, got %q", result)
	}
}

func TestPatternDetection_AWSAccessKeyASIA(t *testing.T) {
	t.Parallel()
	// ASIA prefix + 16 uppercase/digit chars = 20 total (temporary credentials)
	input := "key=ASIAJEXAMPLEKEY12345"
	result := String(input)
	if strings.Contains(result, "ASIAJEXAMPLEKEY12345") {
		t.Errorf("AWS ASIA key should have been redacted, got %q", result)
	}
}

func TestPatternDetection_AWSAccessKeyABIA(t *testing.T) {
	t.Parallel()
	// ABIA prefix (STS service)
	input := "ABIA1234567890ABCDEF"
	result := String(input)
	if strings.Contains(result, "ABIA1234567890ABCDEF") {
		t.Errorf("AWS ABIA key should have been redacted, got %q", result)
	}
}

func TestPatternDetection_AWSAccessKeyACCA(t *testing.T) {
	t.Parallel()
	// ACCA prefix (Content delivery)
	input := "access_key: ACCA1234567890ABCDEF"
	result := String(input)
	if strings.Contains(result, "ACCA1234567890ABCDEF") {
		t.Errorf("AWS ACCA key should have been redacted, got %q", result)
	}
}

func TestPatternDetection_AWSAccessKeyInJSON(t *testing.T) {
	t.Parallel()
	// AWS key embedded in JSON — tests the JSONL path
	input := []byte(`{"type":"text","content":"aws_access_key_id=AKIAIOSFODNN7EXAMPLE"}`)
	result, err := JSONLBytes(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(string(result), "AKIAIOSFODNN7EXAMPLE") {
		t.Errorf("AWS key in JSONL should have been redacted, got %q", result)
	}
}

func TestPatternDetection_AWSAccessKeyBareContext(t *testing.T) {
	t.Parallel()
	// AWS key without any surrounding context — no "key=" prefix, just the key
	input := "AKIAIOSFODNN7EXAMPLE"
	result := String(input)
	if result != "REDACTED" {
		t.Errorf("bare AWS key should be redacted, got %q", result)
	}
}

// --- GitHub Tokens ---

func TestPatternDetection_GitHubPAT(t *testing.T) {
	t.Parallel()
	input := "token: ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZabcdef1234"
	result := String(input)
	if strings.Contains(result, "ghp_") {
		t.Errorf("GitHub PAT should have been redacted, got %q", result)
	}
	if !strings.Contains(result, "REDACTED") {
		t.Errorf("expected REDACTED in output, got %q", result)
	}
}

func TestPatternDetection_GitHubFineGrainedPAT(t *testing.T) {
	t.Parallel()
	input := "GITHUB_TOKEN=github_pat_11ABCDEFG0HIJKLMNOP1234567890abcdefghijklmnopqrstuvwxyz0123456789ABCDEFGHIJKLMNOPQRS"
	result := String(input)
	if strings.Contains(result, "github_pat_") {
		t.Errorf("GitHub fine-grained PAT should have been redacted, got %q", result)
	}
}

func TestPatternDetection_GitHubOAuth(t *testing.T) {
	t.Parallel()
	input := "gho_16C7e42F292c6912E7710c838347Ae178B4a"
	result := String(input)
	if strings.Contains(result, "gho_") {
		t.Errorf("GitHub OAuth token should have been redacted, got %q", result)
	}
}

func TestPatternDetection_GitHubAppToken(t *testing.T) {
	t.Parallel()
	input := "ghu_16C7e42F292c6912E7710c838347Ae178B4a"
	result := String(input)
	if strings.Contains(result, "ghu_") {
		t.Errorf("GitHub app token should have been redacted, got %q", result)
	}
}

// --- Anthropic ---

func TestPatternDetection_AnthropicAPIKey(t *testing.T) {
	t.Parallel()
	input := "ANTHROPIC_API_KEY=sk-ant-api03-xK9mZ2vL8nQ5rT1wY4bC7dF0gH3jE6pA"
	result := String(input)
	if strings.Contains(result, "sk-ant-api03") {
		t.Errorf("Anthropic API key should have been redacted, got %q", result)
	}
	if !strings.Contains(result, "REDACTED") {
		t.Errorf("expected REDACTED in output, got %q", result)
	}
}

// --- Slack Tokens ---

func TestPatternDetection_SlackBotToken(t *testing.T) {
	t.Parallel()
	// xoxb-{10-13 digits}-{10-13 digits}-{alphanum}
	input := "SLACK_TOKEN=" + "xoxb-" + "1234567890-9876543210-AbCdEfGhIjKlMnOpQrStUv"
	result := String(input)
	if strings.Contains(result, "xoxb-") {
		t.Errorf("Slack bot token should have been redacted, got %q", result)
	}
}

func TestPatternDetection_SlackUserToken(t *testing.T) {
	t.Parallel()
	// xoxp-{digits}-{digits}-{digits}-{alphanum 28-34}
	input := "xoxp-" + "1234567890-1234567890-1234567890-abcdef1234567890abcdef1234567890"
	result := String(input)
	if strings.Contains(result, "xoxp-") {
		t.Errorf("Slack user token should have been redacted, got %q", result)
	}
}

// --- Stripe ---

func TestPatternDetection_StripeLiveKey(t *testing.T) {
	t.Parallel()
	input := "stripe_key=" + "sk_live_" + "4eC39HqLyjWDarjtT1zdp7dc"
	result := String(input)
	if strings.Contains(result, "sk_live_") {
		t.Errorf("Stripe live key should have been redacted, got %q", result)
	}
}

func TestPatternDetection_StripeTestKey(t *testing.T) {
	t.Parallel()
	input := "STRIPE_KEY=" + "sk_test_" + "4eC39HqLyjWDarjtT1zdp7dc"
	result := String(input)
	if strings.Contains(result, "sk_test_") {
		t.Errorf("Stripe test key should have been redacted, got %q", result)
	}
}

func TestPatternDetection_StripeRestrictedKey(t *testing.T) {
	t.Parallel()
	input := "rk_live_" + "4eC39HqLyjWDarjtT1zdp7dc"
	result := String(input)
	if strings.Contains(result, "rk_live_") {
		t.Errorf("Stripe restricted key should have been redacted, got %q", result)
	}
}

// --- SendGrid ---

func TestPatternDetection_SendGridAPIKey(t *testing.T) {
	t.Parallel()
	// SG.{22 base64}.{43 base64}  = 66 chars after "SG."
	input := "SENDGRID_API_KEY=" + "SG.ngeVfQFYQlKU0ufo8x5d1A" + ".TwL2iGABf9DHoTf-09kqeF8tAmbihYzrnopKc-1s5cr"
	result := String(input)
	if strings.Contains(result, "SG.ngeVfQ") {
		t.Errorf("SendGrid API key should have been redacted, got %q", result)
	}
}

// --- npm ---

func TestPatternDetection_NpmToken(t *testing.T) {
	t.Parallel()
	// npm_{36 alphanum}
	input := "//registry.npmjs.org/:_authToken=npm_1234567890abcdefABCDEF1234567890abcd"
	result := String(input)
	if strings.Contains(result, "npm_1234567890") {
		t.Errorf("npm token should have been redacted, got %q", result)
	}
}

// --- PyPI ---

func TestPatternDetection_PyPIToken(t *testing.T) {
	t.Parallel()
	// pypi-AgEIcHlwaS5vcmc{50+ chars}
	input := "TWINE_PASSWORD=pypi-AgEIcHlwaS5vcmcCJGI3YjczNTEwLTgwZWEtNDQ5NS1iMDNmLWE0ZjRlYmViNjhkYgACKlszLCIyYmE"
	result := String(input)
	if strings.Contains(result, "pypi-AgEIcHlwaS5vcmc") {
		t.Errorf("PyPI token should have been redacted, got %q", result)
	}
}

// --- Private Keys ---

func TestPatternDetection_RSAPrivateKey(t *testing.T) {
	t.Parallel()
	input := `-----BEGIN RSA PRIVATE KEY-----
MIIEpAIBAAKCAQEA0Z3VS5JJcds3xfn/ygWyF/PbnGy0AHB7MhgHcTz6sE2I2iFK
-----END RSA PRIVATE KEY-----`
	result := String(input)
	if strings.Contains(result, "MIIEpAIBAAKCAQEA0Z3VS5JJcds3xfn") {
		t.Errorf("RSA private key content should have been redacted, got %q", result)
	}
}

func TestPatternDetection_ECPrivateKey(t *testing.T) {
	t.Parallel()
	input := `-----BEGIN EC PRIVATE KEY-----
MHQCAQEEIBkg4LVWM9nuwNSk3yByxZpYRTBnVJk5GkMxMXnBbCxMoAcGBSuBBAAi
-----END EC PRIVATE KEY-----`
	result := String(input)
	if strings.Contains(result, "MHQCAQEEIBkg4LVWM9nuwNSk3yBy") {
		t.Errorf("EC private key content should have been redacted, got %q", result)
	}
}

func TestPatternDetection_GenericPrivateKey(t *testing.T) {
	t.Parallel()
	input := `-----BEGIN PRIVATE KEY-----
MIIEvgIBADANBgkqhkiG9w0BAQEFAASCBKgwggSkAgEAAoIBAQC7a1eZkLio26Dj
-----END PRIVATE KEY-----`
	result := String(input)
	if strings.Contains(result, "MIIEvgIBADANBgkqhkiG9w0BAQEF") {
		t.Errorf("generic private key should have been redacted, got %q", result)
	}
}

// --- DigitalOcean ---

func TestPatternDetection_DigitalOceanPAT(t *testing.T) {
	t.Parallel()
	// dop_v1_{64 hex}
	input := "DO_TOKEN=dop_v1_" + strings.Repeat("a1b2c3d4", 8)
	result := String(input)
	if strings.Contains(result, "dop_v1_") {
		t.Errorf("DigitalOcean PAT should have been redacted, got %q", result)
	}
}

// --- Twilio ---

func TestPatternDetection_TwilioAPIKey(t *testing.T) {
	t.Parallel()
	// SK{32 hex}
	input := "TWILIO_API_KEY=" + "SK" + "1234567890abcdef1234567890abcdef"
	result := String(input)
	if strings.Contains(result, "SK1234567890abcdef") {
		t.Errorf("Twilio API key should have been redacted, got %q", result)
	}
}

// --- Heroku ---

func TestPatternDetection_HerokuAPIKey(t *testing.T) {
	t.Parallel()
	// heroku context with 36-char UUID-like key
	input := `heroku_api_key = "a1b2c3d4-e5f6-7890-abcd-ef1234567890"`
	result := String(input)
	if strings.Contains(result, "a1b2c3d4-e5f6-7890-abcd-ef1234567890") {
		t.Errorf("Heroku API key should have been redacted, got %q", result)
	}
}

// --- Gitleaks Issue #1933: Kubernetes YAML secrets ---
// https://github.com/gitleaks/gitleaks/issues/1933
// Tests that base64-encoded secrets in Kubernetes YAML are caught
// regardless of whether kind: Secret appears before or after data.

func TestPatternDetection_KubernetesSecret_KindFirst(t *testing.T) {
	t.Parallel()
	// Standard order: kind before data — gitleaks catches this
	input := `apiVersion: v1
kind: Secret
data:
  key.pem: LS0tLS1CRUdJTiBQUklWQVRFIEtFWS0tLS0tXG5BM` //nolint:misspell
	result := String(input)
	if strings.Contains(result, "LS0tLS1CRUdJTiBQUklWQVRFIEtFWS0tLS0tXG5BM") {
		t.Errorf("k8s secret (kind first) should have been redacted, got %q", result)
	}
}

func TestPatternDetection_KubernetesSecret_KindLast(t *testing.T) {
	t.Parallel()
	// Reversed order: data before kind — gitleaks issue #1933 fails on this.
	// Our entropy detection should catch the base64 content even if the
	// kubernetes-secret-yaml pattern rule doesn't match.
	input := `apiVersion: v1
data:
  key.pem: LS0tLS1CRUdJTiBQUklWQVRFIEtFWS0tLS0tXG5FNTBGNjBzdXBlcnNlY3JldHBlbWRhdGE
kind: Secret`
	result := String(input)
	if strings.Contains(result, "LS0tLS1CRUdJTiBQUklWQVRFIEtFWS0tLS0t") {
		t.Errorf("k8s secret (kind last) should have been redacted, got %q", result)
	}
}

// --- Generic API key pattern ---

func TestPatternDetection_GenericAPIKeyAssignment(t *testing.T) {
	t.Parallel()
	// Generic pattern: keyword (api_key, secret, token) + assignment + value
	input := `api_key = "a1b2c3d4e5f6g7h8i9j0k1l2m3n4o5p6"`
	result := String(input)
	if strings.Contains(result, "a1b2c3d4e5f6g7h8i9j0k1l2m3n4o5p6") {
		t.Errorf("generic API key should have been redacted, got %q", result)
	}
}

func TestPatternDetection_GenericSecretEnvVar(t *testing.T) {
	t.Parallel()
	input := `SECRET_TOKEN=abcdef1234567890abcdef1234567890`
	result := String(input)
	if strings.Contains(result, "abcdef1234567890abcdef1234567890") {
		t.Errorf("generic secret env var should have been redacted, got %q", result)
	}
}

// --- Entropy-only detection (safety net) ---

func TestEntropyDetection_StillWorks(t *testing.T) {
	t.Parallel()
	// High-entropy string that doesn't match any known pattern prefix.
	input := "secret=xQ9kR7mW2vL5nT8pY1bC4dF0gH3jE6aS"
	result := String(input)
	if strings.Contains(result, "xQ9kR7mW2vL5nT8pY1bC4dF0gH3jE6aS") {
		t.Errorf("high-entropy string should have been redacted, got %q", result)
	}
}

func TestEntropyDetection_UnknownProviderKey(t *testing.T) {
	t.Parallel()
	// A made-up provider key with high entropy — no pattern for it,
	// but entropy should catch it.
	input := "OBSCURE_SERVICE_KEY=zX8wQ3rV7mN1kJ5hG9fD2cB6aY0tP4sL"
	result := String(input)
	if strings.Contains(result, "zX8wQ3rV7mN1kJ5hG9fD2cB6aY0tP4sL") {
		t.Errorf("unknown provider high-entropy key should have been redacted, got %q", result)
	}
}

// --- No false positives ---

func TestNoFalsePositives_NormalText(t *testing.T) {
	t.Parallel()
	input := "The configuration value is simple_text_here and nothing else"
	result := String(input)
	if result != input {
		t.Errorf("normal text should not be redacted, got %q", result)
	}
}

func TestNoFalsePositives_CommonWords(t *testing.T) {
	t.Parallel()
	inputs := []string{
		"hello world this is a test",
		"function getUserProfile returns data",
		"the quick brown fox jumps over the lazy dog",
		"version 1.2.3-beta4 is released",
	}
	for _, input := range inputs {
		result := String(input)
		if result != input {
			t.Errorf("common text should not be redacted:\n  input:  %q\n  output: %q", input, result)
		}
	}
}

// --- Overlap and merge ---

func TestBothMethodsCatchSameSecret(t *testing.T) {
	t.Parallel()
	// sk-ant-api03-xK9mZ2vL8nQ5rT1wY4bC7dF0gH3jE6pA is caught by BOTH:
	// - Entropy (>4.5)
	// - Pattern (sk-ant- prefix)
	// Should produce exactly one REDACTED, not REDACTEDREDACTED.
	input := "sk-ant-api03-xK9mZ2vL8nQ5rT1wY4bC7dF0gH3jE6pA"
	result := String(input)
	if result != "REDACTED" {
		t.Errorf("expected single REDACTED, got %q", result)
	}
}

func TestPatternDetection_MultipleSecretsInOneLine(t *testing.T) {
	t.Parallel()
	input := "AWS_KEY=AKIAIOSFODNN7EXAMPLE STRIPE=" + "sk_live_" + "4eC39HqLyjWDarjtT1zdp7dc"
	result := String(input)
	if strings.Contains(result, "AKIAIOSFODNN7EXAMPLE") {
		t.Errorf("AWS key should have been redacted, got %q", result)
	}
	if strings.Contains(result, "sk_live_") {
		t.Errorf("Stripe key should have been redacted, got %q", result)
	}
	// Count REDACTEDs — should be at least 2
	count := strings.Count(result, "REDACTED")
	if count < 2 {
		t.Errorf("expected at least 2 REDACTEDs for two secrets, got %d in %q", count, result)
	}
}

// --- JSONL integration ---

func TestPatternDetection_InJSONL(t *testing.T) {
	t.Parallel()
	input := []byte(`{"type":"text","content":"key=AKIAIOSFODNN7EXAMPLE"}`)
	result, err := JSONLBytes(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(string(result), "AKIAIOSFODNN7EXAMPLE") {
		t.Errorf("AWS key in JSONL should have been redacted, got %q", result)
	}
}

func TestPatternDetection_MultipleProviders_InJSONL(t *testing.T) {
	t.Parallel()
	input := []byte(`{"aws":"AKIAIOSFODNN7EXAMPLE","stripe":"` + "sk_live_" + `4eC39HqLyjWDarjtT1zdp7dc"}`)
	result, err := JSONLBytes(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	resultStr := string(result)
	if strings.Contains(resultStr, "AKIAIOSFODNN7EXAMPLE") {
		t.Errorf("AWS key in JSONL should have been redacted, got %q", resultStr)
	}
	if strings.Contains(resultStr, "sk_live_") {
		t.Errorf("Stripe key in JSONL should have been redacted, got %q", resultStr)
	}
}

// --- Merge regions ---

func TestMergeRegions_Overlapping(t *testing.T) {
	t.Parallel()
	matches := []Match{
		{Start: 0, End: 10, Method: "entropy"},
		{Start: 5, End: 15, Method: "pattern", RuleID: "test"},
		{Start: 20, End: 30, Method: "entropy"},
	}
	merged := mergeRegions(matches)
	if len(merged) != 2 {
		t.Fatalf("expected 2 merged regions, got %d", len(merged))
	}
	if merged[0].Start != 0 || merged[0].End != 15 {
		t.Errorf("first region: got [%d,%d], want [0,15]", merged[0].Start, merged[0].End)
	}
	if merged[0].Method != "pattern+entropy" {
		t.Errorf("first region method: got %q, want %q", merged[0].Method, "pattern+entropy")
	}
	if merged[1].Start != 20 || merged[1].End != 30 {
		t.Errorf("second region: got [%d,%d], want [20,30]", merged[1].Start, merged[1].End)
	}
}

func TestMergeRegions_Adjacent(t *testing.T) {
	t.Parallel()
	matches := []Match{
		{Start: 0, End: 10, Method: "entropy"},
		{Start: 10, End: 20, Method: "pattern"},
	}
	merged := mergeRegions(matches)
	if len(merged) != 1 {
		t.Fatalf("expected 1 merged region, got %d", len(merged))
	}
	if merged[0].Start != 0 || merged[0].End != 20 {
		t.Errorf("merged region: got [%d,%d], want [0,20]", merged[0].Start, merged[0].End)
	}
}

func TestMergeRegions_Empty(t *testing.T) {
	t.Parallel()
	merged := mergeRegions(nil)
	if merged != nil {
		t.Errorf("expected nil for empty input, got %v", merged)
	}
}

// --- Utilities ---

func TestMaskedPreview(t *testing.T) {
	t.Parallel()
	tests := []struct {
		input string
		want  string
	}{
		{"short", "****"},
		{"exactly12ch", "****"},
		{"AKIAIOSFODNN7EXAMPLE", "AKIA...MPLE"},
		{"sk-ant-api03-xK9mZ2vL8nQ5rT1wY4bC7dF0gH3jE6pA", "sk-a...E6pA"},
	}
	for _, tt := range tests {
		got := maskedPreview(tt.input)
		if got != tt.want {
			t.Errorf("maskedPreview(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

// --- Benchmarks ---

func BenchmarkString_NoSecrets(b *testing.B) {
	input := "This is a normal string with no secrets whatsoever, just regular text content."
	for b.Loop() {
		String(input)
	}
}

func BenchmarkString_WithEntropySecret(b *testing.B) {
	input := "my key is sk-ant-api03-xK9mZ2vL8nQ5rT1wY4bC7dF0gH3jE6pA ok"
	for b.Loop() {
		String(input)
	}
}

func BenchmarkString_WithPatternSecret(b *testing.B) {
	input := "aws_access_key_id = AKIAIOSFODNN7EXAMPLE"
	for b.Loop() {
		String(input)
	}
}
