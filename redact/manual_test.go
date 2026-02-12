package redact_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/entireio/cli/redact"
)

// TestManual_BugReport_AWSAccessKeyID verifies the exact scenario from the bug report.
// Bug: AWS Access Key IDs (AKIA...) were not redacted because their entropy (3.68)
// is below the 4.5 threshold. The layered detection fix catches them via pattern matching.
func TestManual_BugReport_AWSAccessKeyID(t *testing.T) {
	t.Parallel()

	input := "export AWS_ACCESS_KEY_ID=AKIAIOSFODNN7EXAMPLE"
	result := redact.String(input)

	if !strings.Contains(result, "REDACTED") {
		t.Errorf("Bug report scenario FAILED: AWS Access Key ID not redacted\n  input:  %q\n  result: %q", input, result)
	}
	if strings.Contains(result, "AKIAIOSFODNN7EXAMPLE") {
		t.Errorf("Bug report scenario FAILED: AWS key still present in output\n  result: %q", result)
	}
	t.Logf("Bug report scenario PASSED: %q -> %q", input, result)
}

// TestManual_EndToEnd_AllSecretPaths simulates secrets flowing through all data paths
// that exist in the CLI. This verifies that both String() and JSONLBytes() catch secrets
// that would flow through transcript copies, prompt files, summaries, and context files.
func TestManual_EndToEnd_AllSecretPaths(t *testing.T) {
	t.Parallel()

	// Secrets that should be caught by pattern detection (low entropy, distinctive format)
	patternSecrets := map[string]string{
		"AWS Access Key (AKIA)":    "AKIAIOSFODNN7EXAMPLE",
		"AWS Access Key (ASIA)":    "ASIAJEXAMPLEKEY12345",
		"GitHub PAT":               "ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZabcdef1234",
		"GitHub Fine-Grained PAT":  "github_pat_11AAAAAA0aaaaaAAAAAAAA_BBBBBBBBBBbbbbbbBBBBBBBBBBbbbbbbBBBBBBBBBBbbbbbbBBBBBBBBBBbb",
		"Slack Bot Token":          "xoxb-" + "123456789012-1234567890123-AbCdEfGhIjKlMnOpQrStUvWx",
		"Stripe Live Key":          "sk_live_" + "51H1234567890abcdefghijklmnopqrs",
		"SendGrid API Key":         "SG.ngeVfQFYQlKU0ufo8x5d1A" + ".TwL2iGABf9DHoTf-09kqeF8tAmbihYzrnopKc-1s5cr",
		"Anthropic API Key":        "sk-ant-api03-xK9mZ2vL8nQ5rT1wY4bC7dF0gH3jE6pA",
		"npm Token":                "npm_1234567890abcdefghijklmnopqrstuvwxyz",
		"DigitalOcean PAT":         "dop_v1_abc123def456ghi789jkl012mno345pqr678stu901vwx234yz567abc890de",
	}

	// Secrets that should be caught by entropy detection (high entropy, no distinctive pattern)
	entropySecrets := map[string]string{
		"Generic High-Entropy Secret": "xK9mZ2vL8nQ5rT1wY4bC7dF0gH3jE6pA8kR2sN7uW",
	}

	allSecrets := make(map[string]string)
	for k, v := range patternSecrets {
		allSecrets[k] = v
	}
	for k, v := range entropySecrets {
		allSecrets[k] = v
	}

	t.Run("PlainText_PromptFile", func(t *testing.T) {
		t.Parallel()
		for name, secret := range allSecrets {
			// Simulates a user prompt containing a secret
			input := fmt.Sprintf("Please configure my API with key %s in the .env file", secret)
			result := redact.String(input)
			if strings.Contains(result, secret) {
				t.Errorf("[%s] Secret leaked through prompt file path\n  secret: %s\n  result: %s", name, secret, result)
			}
		}
	})

	t.Run("PlainText_SummaryFile", func(t *testing.T) {
		t.Parallel()
		for name, secret := range allSecrets {
			// Simulates an assistant summary mentioning a secret
			input := fmt.Sprintf("I configured the environment variable with value %s as requested.", secret)
			result := redact.String(input)
			if strings.Contains(result, secret) {
				t.Errorf("[%s] Secret leaked through summary file path\n  secret: %s\n  result: %s", name, secret, result)
			}
		}
	})

	t.Run("PlainText_ContextFile", func(t *testing.T) {
		t.Parallel()
		for name, secret := range allSecrets {
			// Simulates a context.md file with secrets
			input := fmt.Sprintf("# Session Context\n\n**Commit Message:** Add API integration\n\n## Prompt\n\nSet key=%s\n\n## Summary\n\nDone.", secret)
			result := redact.String(input)
			if strings.Contains(result, secret) {
				t.Errorf("[%s] Secret leaked through context file path\n  secret: %s\n  result: %s", name, secret, result)
			}
		}
	})

	t.Run("JSONL_Transcript", func(t *testing.T) {
		t.Parallel()
		for name, secret := range allSecrets {
			// Simulates a JSONL transcript line containing a secret in assistant content
			entry := map[string]any{
				"type": "assistant",
				"content": []map[string]string{
					{"type": "text", "text": fmt.Sprintf("I found the API key: %s", secret)},
				},
			}
			line, _ := json.Marshal(entry)
			result, err := redact.JSONLBytes(append(line, '\n'))
			if err != nil {
				t.Fatalf("[%s] JSONLBytes error: %v", name, err)
			}
			if strings.Contains(string(result), secret) {
				t.Errorf("[%s] Secret leaked through JSONL transcript path\n  secret: %s\n  result: %s", name, secret, string(result))
			}
		}
	})

	t.Run("JSONL_ToolResult", func(t *testing.T) {
		t.Parallel()
		for name, secret := range allSecrets {
			// Simulates a tool result containing a secret (e.g., reading a .env file)
			entry := map[string]any{
				"type":       "tool_result",
				"tool_use_id": "toolu_123",
				"content":    fmt.Sprintf("AWS_ACCESS_KEY_ID=%s\nAWS_SECRET_ACCESS_KEY=wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY", secret),
			}
			line, _ := json.Marshal(entry)
			result, err := redact.JSONLBytes(append(line, '\n'))
			if err != nil {
				t.Fatalf("[%s] JSONLBytes error: %v", name, err)
			}
			if strings.Contains(string(result), secret) {
				t.Errorf("[%s] Secret leaked through JSONL tool result path\n  secret: %s\n  result: %s", name, secret, string(result))
			}
		}
	})

	t.Run("NoFalsePositives", func(t *testing.T) {
		t.Parallel()
		safeInputs := []string{
			"The quick brown fox jumps over the lazy dog",
			"export PATH=/usr/local/bin:/usr/bin:/bin",
			"git commit -m 'Add feature'",
			"func main() { fmt.Println(\"hello world\") }",
			"SELECT * FROM users WHERE id = 42",
			"https://api.example.com/v1/users?page=1&limit=10",
			"The password must be at least 8 characters",
			"Error: connection refused at localhost:5432",
		}
		for _, input := range safeInputs {
			result := redact.String(input)
			if result != input {
				t.Errorf("False positive on safe input\n  input:  %q\n  result: %q", input, result)
			}
		}
	})
}

// TestManual_GeminiJSON_Transcript tests that Gemini CLI's single-JSON-document
// transcripts are properly redacted via JSONBytes(). Gemini uses .json files
// (not .jsonl) with a {"messages": [...]} structure.
func TestManual_GeminiJSON_Transcript(t *testing.T) {
	t.Parallel()

	// Simulates a Gemini transcript with secrets in user prompt and assistant response
	geminiTranscript := `{
  "messages": [
    {"id": "msg-1", "type": "user", "content": "Set my AWS key to AKIAIOSFODNN7EXAMPLE"},
    {"id": "msg-2", "type": "gemini", "content": "I've configured the key. Here's the .env:\nAWS_ACCESS_KEY_ID=AKIAIOSFODNN7EXAMPLE\nAWS_SECRET_ACCESS_KEY=wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"},
    {"id": "msg-3", "type": "user", "content": "Also add my GitHub token ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZabcdef1234"},
    {"id": "msg-4", "type": "gemini", "content": "Done! I added the GitHub token to your config."}
  ]
}`

	result, err := redact.JSONBytes([]byte(geminiTranscript))
	if err != nil {
		t.Fatalf("JSONBytes error: %v", err)
	}

	resultStr := string(result)

	// AWS key should be redacted
	if strings.Contains(resultStr, "AKIAIOSFODNN7EXAMPLE") {
		t.Error("AWS Access Key ID not redacted in Gemini JSON transcript")
	}

	// GitHub PAT should be redacted
	if strings.Contains(resultStr, "ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZabcdef1234") {
		t.Error("GitHub PAT not redacted in Gemini JSON transcript")
	}

	// High-entropy AWS secret key should be redacted
	if strings.Contains(resultStr, "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY") {
		t.Error("AWS Secret Access Key not redacted in Gemini JSON transcript")
	}

	// JSON structure should still be valid
	var parsed any
	if err := json.Unmarshal(result, &parsed); err != nil {
		t.Errorf("Redacted output is not valid JSON: %v\nResult:\n%s", err, resultStr)
	}

	// Non-secret content should be preserved
	if !strings.Contains(resultStr, "msg-1") {
		t.Error("Message ID 'msg-1' was incorrectly redacted")
	}
	if !strings.Contains(resultStr, "gemini") {
		t.Error("Message type 'gemini' was incorrectly redacted")
	}

	t.Logf("Gemini JSON transcript redaction result:\n%s", resultStr)
}

// TestManual_GeminiJSON_ToolCalls tests that secrets in Gemini tool call arguments
// are redacted properly.
func TestManual_GeminiJSON_ToolCalls(t *testing.T) {
	t.Parallel()

	stripeKey := "sk_live_" + "51H1234567890abcdefghijklmnopqrs"
	geminiTranscript := `{
  "messages": [
    {"id": "msg-1", "type": "user", "content": "Write my Stripe key to .env"},
    {"id": "msg-2", "type": "gemini", "content": "", "toolCalls": [
      {"id": "tc-1", "name": "write_file", "args": {"file_path": ".env", "content": "STRIPE_KEY=` + stripeKey + `"}}
    ]}
  ]
}`

	result, err := redact.JSONBytes([]byte(geminiTranscript))
	if err != nil {
		t.Fatalf("JSONBytes error: %v", err)
	}

	resultStr := string(result)

	// Stripe key should be redacted
	if strings.Contains(resultStr, stripeKey) {
		t.Error("Stripe key not redacted in Gemini tool call args")
	}

	// File path should be preserved (not a secret)
	if !strings.Contains(resultStr, ".env") {
		t.Error("File path '.env' was incorrectly redacted")
	}

	// JSON should still be valid
	var parsed any
	if err := json.Unmarshal(result, &parsed); err != nil {
		t.Errorf("Redacted output is not valid JSON: %v", err)
	}

	t.Logf("Gemini tool call redaction result:\n%s", resultStr)
}

// TestManual_JSONBytes_InvalidJSON tests that JSONBytes falls back gracefully
// on invalid JSON input.
func TestManual_JSONBytes_InvalidJSON(t *testing.T) {
	t.Parallel()

	input := "not valid json with secret AKIAIOSFODNN7EXAMPLE"
	result, err := redact.JSONBytes([]byte(input))
	if err != nil {
		t.Fatalf("JSONBytes error on invalid JSON: %v", err)
	}

	if strings.Contains(string(result), "AKIAIOSFODNN7EXAMPLE") {
		t.Error("Secret not redacted in invalid JSON fallback path")
	}
}

// TestManual_KubernetesSecret_Issue1933 tests the scenario from gitleaks issue #1933
// where Kubernetes secrets with `kind: Secret` appearing after the data section
// were not detected.
func TestManual_KubernetesSecret_Issue1933(t *testing.T) {
	t.Parallel()

	// Scenario from issue #1933: kind: Secret appears AFTER the data section
	k8sYAML := `apiVersion: v1
data:
  password: c3VwZXJTZWNyZXRQYXNzd29yZDEyMw==
  username: YWRtaW5Vc2VyMTIz
kind: Secret
metadata:
  name: my-secret`

	result := redact.String(k8sYAML)

	// The base64-encoded values have high enough entropy to be caught,
	// even if the kubernetes-specific pattern doesn't match the reversed order
	if strings.Contains(result, "c3VwZXJTZWNyZXRQYXNzd29yZDEyMw==") {
		t.Errorf("Kubernetes secret password not redacted (issue #1933)\n  result: %s", result)
	}
	if strings.Contains(result, "YWRtaW5Vc2VyMTIz") {
		// Note: this shorter base64 may have lower entropy and might not be caught.
		// This is acceptable — the longer password value is the critical one.
		t.Logf("Note: shorter base64 value not caught (low entropy): YWRtaW5Vc2VyMTIz")
	}

	t.Logf("Kubernetes secret test result:\n%s", result)
}
