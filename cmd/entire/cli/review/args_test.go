package review

import (
	"strings"
	"testing"

	"github.com/pelletier/go-toml/v2"
)

// codex parses each -c value as TOML; the guardrail must survive that intact.
func TestCodexGuardrailConfigIsValidTOML(t *testing.T) {
	t.Parallel()

	key, value, ok := strings.Cut(CodexGuardrailConfig(), "=")
	if !ok || key != "developer_instructions" {
		t.Fatalf("CodexGuardrailConfig() = %q, want developer_instructions=<value>", CodexGuardrailConfig())
	}
	var doc map[string]string
	if err := toml.Unmarshal([]byte("v = "+value), &doc); err != nil {
		t.Fatalf("value is not a TOML string: %v", err)
	}
	if doc["v"] != ReviewerGuardrail {
		t.Fatalf("TOML round trip = %q, want ReviewerGuardrail", doc["v"])
	}
}
