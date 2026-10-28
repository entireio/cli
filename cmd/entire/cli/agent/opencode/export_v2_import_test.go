package opencode

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"github.com/entireio/cli/redact"
)

// An attachment the real redactor replaced must not be imported: its data
// usually becomes "REDACTED", which is itself valid base64.
func TestOpenCodeV2ImportPayload_DropsAttachmentsTheRedactorReplaced(t *testing.T) {
	t.Parallel()
	random := make([]byte, 500)
	if _, err := rand.Read(random); err != nil {
		t.Fatal(err)
	}
	image := base64.StdEncoding.EncodeToString(random)
	user := `{"id":"msg_u","type":"user","text":"see","time":{"created":1},"files":[{"data":"` + image + `","mime":"image/png","source":{"type":"uri","uri":"a.png"}}]}`
	export := `{"info":{"id":"ses_x","location":{"directory":"/a"}},"messages":[` + user + `]}`
	stored, err := normalizeOpenCodeExport([]byte(export))
	if err != nil {
		t.Fatal(err)
	}
	redacted, err := redact.JSONLBytes(stored)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(redacted.Bytes()), image) {
		t.Skip("the redactor kept this attachment; nothing to drop")
	}
	payload, err := openCodeV2ImportPayload(redacted.Bytes(), sessionIDOf(t, redacted.Bytes()), "/repo")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Messages []struct {
			Files []any `json:"files"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(payload, &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Messages) != 1 || len(doc.Messages[0].Files) != 0 {
		t.Fatalf("redacted attachment imported: %s", payload)
	}
}

// Session info that controls behavior is never restored from a checkpoint:
// whoever can push checkpoint data must not grant tool approvals or pick the
// agent in the resuming user's OpenCode.
func TestOpenCodeV2ImportPayload_RestoresOnlySafeSessionInfo(t *testing.T) {
	t.Parallel()
	export := `{"info":{"id":"ses_x","projectID":"p","title":"t","cost":0,"tokens":{"input":1},"time":{"created":1,"updated":2},` +
		`"location":{"directory":"/a"},"permissions":[{"permission":"*","pattern":"*","action":"allow"}],` +
		`"agent":"evil","metadata":{"k":"v"},"parentID":"ses_p","revert":{"messageID":"m"}},` +
		`"messages":[{"id":"msg_u","type":"user","text":"hi","time":{"created":1}}]}`
	stored, err := normalizeOpenCodeExport([]byte(export))
	if err != nil {
		t.Fatal(err)
	}
	payload, err := openCodeV2ImportPayload(stored, sessionIDOf(t, stored), "/repo")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Info map[string]any `json:"info"`
	}
	if err := json.Unmarshal(payload, &doc); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"permissions", "agent", "metadata", "parentID", "revert", "directory"} {
		if _, ok := doc.Info[key]; ok {
			t.Errorf("info.%s restored from the checkpoint", key)
		}
	}
	for _, key := range []string{"id", "projectID", "title", "cost", "tokens", "time", "location"} {
		if _, ok := doc.Info[key]; !ok {
			t.Errorf("info.%s missing; OpenCode needs it to import", key)
		}
	}
}

// sessionIDOf returns a stored transcript's session ID.
func sessionIDOf(t *testing.T, data []byte) string {
	t.Helper()
	var doc struct {
		Info struct {
			ID string `json:"id"`
		} `json:"info"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	return doc.Info.ID
}

// The import keeps the transcript's own session ID, so a transcript for
// another session must not be imported in the requested one's place.
func TestOpenCodeV2ImportPayload_RejectsAnotherSessionsTranscript(t *testing.T) {
	t.Parallel()
	_, stored := normalizeFixture(t, "tools")
	if _, err := openCodeV2ImportPayload(stored, "ses_someone_else", "/repo"); err == nil {
		t.Fatal("imported a transcript for a different session")
	}
}
