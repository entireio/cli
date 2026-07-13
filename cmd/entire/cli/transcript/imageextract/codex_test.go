package imageextract

import (
	"encoding/base64"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
)

// codexImageLine returns a real-format Codex user message embedding one inline
// image as a data-URI in an input_image content block (compact serialization,
// matching how the Codex rollout JSONL is written).
func codexImageLine(b64 string) string {
	return `{"type":"response_item","payload":{"type":"message","role":"user","content":[` +
		`{"type":"input_text","text":"<image name=[Image #1]>"},` +
		`{"type":"input_image","image_url":"data:image/png;base64,` + b64 + `"},` +
		`{"type":"input_text","text":"</image>"}` +
		`]}}`
}

// codexFunctionOutputLine embeds a data-URI inside function_call_output text,
// the way a screenshot/generated-image tool result appears in the rollout.
func codexFunctionOutputLine(b64 string) string {
	return `{"type":"response_item","payload":{"type":"function_call_output","call_id":"call_1",` +
		`"output":"here is the render: data:image/png;base64,` + b64 + ` done"}}`
}

func codexPNG(payload string) string {
	return base64.StdEncoding.EncodeToString([]byte("\x89PNG\r\n\x1a\n" + payload + strings.Repeat("-codex-image-bytes", 3)))
}

func codexJPEG(payload string) string {
	return base64.StdEncoding.EncodeToString([]byte("\xFF\xD8\xFF" + payload + strings.Repeat("-codex-image-bytes", 3)))
}

// The core contract for Codex: extract then reinject reproduces the bytes exactly.
func TestCodexCodec_RoundTripByteExact(t *testing.T) {
	t.Parallel()
	c := CodecFor(agent.AgentTypeCodex)
	if c == nil {
		t.Fatal("expected a codec for Codex")
	}
	b64 := codexPNG("round-trip")
	orig := codexImageLine(b64) + "\n" +
		`{"type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}}` + "\n"

	rewritten, assets, err := c.ExtractImages([]byte(orig))
	if err != nil {
		t.Fatalf("ExtractImages: %v", err)
	}
	if len(assets) != 1 {
		t.Fatalf("expected 1 asset, got %d", len(assets))
	}
	if assets[0].MediaType != mediaTypePNG {
		t.Errorf("media type = %q, want image/png", assets[0].MediaType)
	}
	if strings.Contains(string(rewritten), b64) {
		t.Error("base64 must be gone from the rewritten transcript")
	}
	// The data-URI prefix stays inline; only the base64 value became a placeholder.
	if !strings.Contains(string(rewritten), "data:image/png;base64,"+placeholderPrefix) {
		t.Error("expected the placeholder to sit inside the data-URI, prefix preserved")
	}

	restored, err := c.ReinjectImages(rewritten, lookupFrom(assets))
	if err != nil {
		t.Fatalf("ReinjectImages: %v", err)
	}
	if string(restored) != orig {
		t.Fatalf("round trip not byte-exact:\n got: %s\nwant: %s", restored, orig)
	}
}

// A data-URI embedded in function_call_output text round-trips too.
func TestCodexCodec_FunctionOutputDataURIRoundTrips(t *testing.T) {
	t.Parallel()
	c := CodecFor(agent.AgentTypeCodex)
	b64 := codexPNG("tool-output")
	orig := codexFunctionOutputLine(b64) + "\n"

	rewritten, assets, err := c.ExtractImages([]byte(orig))
	if err != nil {
		t.Fatalf("ExtractImages: %v", err)
	}
	if len(assets) != 1 {
		t.Fatalf("expected 1 asset, got %d", len(assets))
	}
	if strings.Contains(string(rewritten), b64) {
		t.Error("base64 in tool output should be externalized")
	}
	restored, err := c.ReinjectImages(rewritten, lookupFrom(assets))
	if err != nil {
		t.Fatalf("ReinjectImages: %v", err)
	}
	if string(restored) != orig {
		t.Fatal("function_call_output round trip not byte-exact")
	}
}

// A single message with many images (the real Codex case) round-trips, each a
// distinct asset.
func TestCodexCodec_MultipleImagesOneMessage(t *testing.T) {
	t.Parallel()
	c := CodecFor(agent.AgentTypeCodex)
	b1, b2, b3 := codexPNG("one"), codexJPEG("two"), codexPNG("three")
	orig := `{"type":"response_item","payload":{"type":"message","role":"user","content":[` +
		`{"type":"input_image","image_url":"data:image/png;base64,` + b1 + `"},` +
		`{"type":"input_image","image_url":"data:image/jpeg;base64,` + b2 + `"},` +
		`{"type":"input_image","image_url":"data:image/png;base64,` + b3 + `"}` +
		`]}}` + "\n"

	rewritten, assets, err := c.ExtractImages([]byte(orig))
	if err != nil {
		t.Fatalf("ExtractImages: %v", err)
	}
	if len(assets) != 3 {
		t.Fatalf("expected 3 assets, got %d", len(assets))
	}
	// jpeg maps to .jpg extension.
	var sawJPG bool
	for _, a := range assets {
		if strings.HasSuffix(a.Name, ".jpg") {
			sawJPG = true
		}
	}
	if !sawJPG {
		t.Error("expected a .jpg asset from the image/jpeg data-URI")
	}
	restored, err := c.ReinjectImages(rewritten, lookupFrom(assets))
	if err != nil {
		t.Fatalf("ReinjectImages: %v", err)
	}
	if string(restored) != orig {
		t.Fatal("multi-image round trip not byte-exact")
	}
}

// Identical images dedupe to one asset but round-trip both occurrences.
func TestCodexCodec_DedupesIdenticalImages(t *testing.T) {
	t.Parallel()
	c := CodecFor(agent.AgentTypeCodex)
	b64 := codexPNG("same")
	orig := codexImageLine(b64) + "\n" + codexImageLine(b64) + "\n"
	rewritten, assets, err := c.ExtractImages([]byte(orig))
	if err != nil {
		t.Fatalf("ExtractImages: %v", err)
	}
	if len(assets) != 1 {
		t.Fatalf("identical images should dedupe to 1 asset, got %d", len(assets))
	}
	restored, err := c.ReinjectImages(rewritten, lookupFrom(assets))
	if err != nil {
		t.Fatalf("ReinjectImages: %v", err)
	}
	if string(restored) != orig {
		t.Fatal("dedup round trip not byte-exact")
	}
}

// A text-only Codex transcript is a no-op.
func TestCodexCodec_NoImagesIsNoOp(t *testing.T) {
	t.Parallel()
	c := CodecFor(agent.AgentTypeCodex)
	orig := `{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}}` + "\n"
	rewritten, assets, err := c.ExtractImages([]byte(orig))
	if err != nil {
		t.Fatalf("ExtractImages: %v", err)
	}
	if assets != nil {
		t.Errorf("expected no assets, got %d", len(assets))
	}
	if string(rewritten) != orig {
		t.Error("text-only transcript should be unchanged")
	}
}

// A tiny data-URI (below the externalize threshold) is left inline.
func TestCodexCodec_LeavesTinyDataURIInline(t *testing.T) {
	t.Parallel()
	c := CodecFor(agent.AgentTypeCodex)
	tiny := base64.StdEncoding.EncodeToString([]byte("tiny"))
	if len(tiny) >= minExternalizedBase64Len {
		t.Fatalf("fixture too long: %d", len(tiny))
	}
	orig := codexImageLine(tiny) + "\n"
	rewritten, assets, err := c.ExtractImages([]byte(orig))
	if err != nil {
		t.Fatalf("ExtractImages: %v", err)
	}
	if len(assets) != 0 || string(rewritten) != orig {
		t.Errorf("tiny data-URI must be left inline; assets=%d changed=%v", len(assets), string(rewritten) != orig)
	}
}

// Ordering: a Codex line carrying both a secret and an image data-URI —
// extraction lifts the image, leaving the secret for the redaction pass, and the
// image reinjects cleanly.
func TestCodexCodec_ExtractLeavesSecretForRedaction(t *testing.T) {
	t.Parallel()
	c := CodecFor(agent.AgentTypeCodex)
	secret := "aB3xK9mQ7pL2wR8tY4vN6cF1gH5jD0sZeW7uI2oP"
	b64 := codexPNG("secret-plus-image")
	orig := `{"type":"response_item","payload":{"type":"message","role":"user","content":[` +
		`{"type":"input_text","text":"token ` + secret + `"},` +
		`{"type":"input_image","image_url":"data:image/png;base64,` + b64 + `"}` +
		`]}}` + "\n"

	rewritten, assets, err := c.ExtractImages([]byte(orig))
	if err != nil {
		t.Fatalf("ExtractImages: %v", err)
	}
	if len(assets) != 1 {
		t.Fatalf("expected 1 asset, got %d", len(assets))
	}
	if strings.Contains(string(rewritten), b64) {
		t.Error("image should be externalized")
	}
	if !strings.Contains(string(rewritten), secret) {
		t.Error("the secret must remain for the downstream redaction pass")
	}
	restored, err := c.ReinjectImages(rewritten, lookupFrom(assets))
	if err != nil {
		t.Fatalf("ReinjectImages: %v", err)
	}
	if string(restored) != orig {
		t.Fatal("round trip not byte-exact")
	}
}
