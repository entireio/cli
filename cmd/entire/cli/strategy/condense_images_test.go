package strategy

import (
	"context"
	"encoding/base64"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/transcript/imageextract"
)

// These tests exercise the opt-in image-externalization step in the condensation
// pipeline. They use t.Chdir / t.Setenv (process-global) to control the settings
// flag, so they cannot run in parallel.

// claudeImageLine returns a Claude Code user line embedding one inline base64
// image, plus the base64 string for assertions.
func claudeImageLine(t *testing.T, payload string) (line, b64 string) {
	t.Helper()
	b64 = base64.StdEncoding.EncodeToString([]byte(payload))
	line = `{"type":"user","message":{"role":"user","content":[` +
		`{"type":"text","text":"look"},` +
		`{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + b64 + `"}}` +
		`]}}`
	return line, b64
}

func TestExternalizeSessionImages_DisabledIsNoOp(t *testing.T) {
	t.Chdir(t.TempDir()) // isolate settings; externalization defaults off
	line, b64 := claudeImageLine(t, "disabled-noop-bytes-padded-long-enough-to-externalize")
	raw := []byte(line + "\n")
	state := &SessionState{SessionID: "s1", AgentType: agent.AgentTypeClaudeCode}

	rewritten, assets := externalizeSessionImages(context.Background(), context.Background(), state, raw)
	if assets != nil {
		t.Errorf("expected no assets when flag off, got %d", len(assets))
	}
	if string(rewritten) != string(raw) {
		t.Error("transcript must be unchanged when externalization is off")
	}
	if !strings.Contains(string(rewritten), b64) {
		t.Error("base64 image should still be inline when externalization is off")
	}
}

func TestExternalizeSessionImages_EnabledExtracts(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("ENTIRE_EXTERNALIZE_IMAGES", "1")
	line, b64 := claudeImageLine(t, "enabled-extract-bytes-padded-long-enough-to-externalize")
	raw := []byte(line + "\n")
	state := &SessionState{SessionID: "s2", AgentType: agent.AgentTypeClaudeCode}

	rewritten, assets := externalizeSessionImages(context.Background(), context.Background(), state, raw)
	if len(assets) != 1 {
		t.Fatalf("expected 1 asset when flag on, got %d", len(assets))
	}
	if strings.Contains(string(rewritten), b64) {
		t.Error("base64 image should be externalized out of the transcript")
	}
	if !strings.Contains(string(rewritten), "entire-asset:assets/") {
		t.Error("transcript should carry a placeholder after externalization")
	}
	// The caller's raw transcript must be left untouched (growth-baseline / result).
	if !strings.Contains(string(raw), b64) {
		t.Error("the input transcript must not be mutated by externalization")
	}
}

func TestExternalizeSessionImages_NonImageAgentIsNoOp(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("ENTIRE_EXTERNALIZE_IMAGES", "1") // on, but agent has no codec
	line, b64 := claudeImageLine(t, "codex-noop-bytes-padded-long-enough-to-externalize")
	raw := []byte(line + "\n")
	state := &SessionState{SessionID: "s3", AgentType: types.AgentType("Codex")}

	rewritten, assets := externalizeSessionImages(context.Background(), context.Background(), state, raw)
	if assets != nil {
		t.Errorf("agent with no image codec should extract nothing, got %d assets", len(assets))
	}
	if string(rewritten) != string(raw) || !strings.Contains(string(rewritten), b64) {
		t.Error("transcript must pass through unchanged for a no-codec agent")
	}
}

// TestExtractThenRedact_ImageExternalizedSecretRedacted proves the mandatory
// ordering: on a line carrying BOTH a base64 image and a high-entropy secret,
// extracting first lifts the image into an asset (placeholder left behind), the
// redaction pass then strips the secret while leaving the low-entropy
// placeholder intact, and reinjection restores the exact image bytes. The stored
// (post-extract, post-redact) transcript therefore contains neither the raw
// image blob nor the secret.
func TestExtractThenRedact_ImageExternalizedSecretRedacted(t *testing.T) {
	t.Parallel()
	secret := "aB3xK9mQ7pL2wR8tY4vN6cF1gH5jD0sZeW7uI2oP"
	b64 := base64.StdEncoding.EncodeToString([]byte("\x89PNG\r\n\x1a\nordering-fixture-bytes-padded-long-enough-to-externalize\x00\x01\x02"))
	raw := []byte(`{"type":"user","message":{"role":"user","content":[` +
		`{"type":"text","text":"my token ` + secret + ` ok"},` +
		`{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + b64 + `"}}` +
		`]}}` + "\n")

	codec := imageextract.CodecFor(agent.AgentTypeClaudeCode)
	if codec == nil {
		t.Fatal("expected a Claude Code image codec")
	}

	// Step 1: extract images (before redaction).
	rewritten, assets, err := codec.ExtractImages(raw)
	if err != nil {
		t.Fatalf("ExtractImages() error = %v", err)
	}
	if len(assets) != 1 {
		t.Fatalf("expected 1 asset, got %d", len(assets))
	}
	if strings.Contains(string(rewritten), b64) {
		t.Error("image base64 should be gone after extraction")
	}
	if !strings.Contains(string(rewritten), secret) {
		t.Error("secret must still be present pre-redaction")
	}

	// Step 2: redact the placeholder-bearing transcript.
	redacted, err := redactSessionJSONLBytes(context.Background(), rewritten)
	if err != nil {
		t.Fatalf("redactSessionJSONLBytes() error = %v", err)
	}
	stored := string(redacted.Bytes())
	if strings.Contains(stored, secret) {
		t.Error("secret must be redacted out of the stored transcript")
	}
	if !strings.Contains(stored, "REDACTED") {
		t.Error("expected a REDACTED marker where the secret was")
	}
	if !strings.Contains(stored, "entire-asset:assets/") {
		t.Error("placeholder must survive redaction (low entropy)")
	}
	if strings.Contains(stored, b64) {
		t.Error("stored transcript must not contain the raw image blob")
	}

	// Step 3: reinject restores the exact image bytes.
	lookup := func(name string) (imageextract.Asset, bool) {
		for _, a := range assets {
			if a.Name == name {
				return a, true
			}
		}
		return imageextract.Asset{}, false
	}
	restored, err := codec.ReinjectImages(redacted.Bytes(), lookup)
	if err != nil {
		t.Fatalf("ReinjectImages() error = %v", err)
	}
	final := string(restored)
	if !strings.Contains(final, b64) {
		t.Error("image should be reinjected on restore")
	}
	if strings.Contains(final, "entire-asset:assets/") {
		t.Error("no placeholder should remain after reinjection")
	}
	if strings.Contains(final, secret) {
		t.Error("secret must stay redacted after reinjection")
	}
}
