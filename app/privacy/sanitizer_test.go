package privacy_test

import (
	"testing"

	"github.com/entireio/cli/app/models"
	"github.com/entireio/cli/app/privacy"
)

func TestSanitizeString(t *testing.T) {
	sanitizer := privacy.NewPrivacySanitizer()

	input := "User key api_key: ghp_1234567890abcdef1234567890abcdef1234 and auth=secret12345"
	cleaned := sanitizer.SanitizeString(input)

	if cleaned == input {
		t.Errorf("Expected redaction but got original string: %s", cleaned)
	}
}

func TestSanitizeCheckpoint(t *testing.T) {
	sanitizer := privacy.NewPrivacySanitizer()

	cp := &models.Checkpoint{
		CheckpointID:  "cp-test",
		IntentContext: "Fix auth with token=secret_token_val_12345",
	}

	sanitized := sanitizer.SanitizeCheckpoint(cp)
	if sanitized.IntentContext == cp.IntentContext {
		t.Errorf("Expected redacted intent context, got: %s", sanitized.IntentContext)
	}
}
