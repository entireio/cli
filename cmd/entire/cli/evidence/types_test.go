package evidence

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestSanitizeCompleteContext(t *testing.T) {
	t.Parallel()
	evidence := Sanitize(LocalInput{CheckpointID: "cp_1", Intent: "- add collector\n- preserve privacy"})
	if evidence.ContextStatus != ContextComplete || len(evidence.Requirements) != 2 {
		t.Fatalf("unexpected sanitized context: %#v", evidence)
	}
}

func TestSanitizeIncompleteWhenIntentMissingOrRedacted(t *testing.T) {
	t.Parallel()
	for _, intent := range []string{"", "[REDACTED]", "implement it"} {
		if got := Sanitize(LocalInput{Intent: intent}).ContextStatus; got != ContextIncomplete {
			t.Fatalf("intent %q produced %s", intent, got)
		}
	}
}

func TestSanitizedEvidenceDoesNotSerializeRawLocalText(t *testing.T) {
	t.Parallel()
	const secret = "private prompt and transcript content"
	evidence := Sanitize(LocalInput{CheckpointID: "cp_1", Intent: secret, Transcript: []byte(secret), ChangedFiles: []string{"main.go"}})
	encoded, err := json.Marshal(evidence)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), secret) {
		t.Fatalf("serialized evidence leaked local text: %s", encoded)
	}
}
