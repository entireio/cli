package imageextract

import (
	"encoding/base64"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	transcriptlib "github.com/entireio/cli/transcript"
)

// piUserImageLine returns a real-format Pi session line: a user message whose
// content holds text and one inline image item.
func piUserImageLine(b64, mimeType string) string {
	return `{"type":"message","id":"61c6d42a","parentId":"d7240266","timestamp":"2026-10-20T17:02:53.673Z",` +
		`"message":{"role":"user","content":[{"type":"text","text":"what is in this screenshot?"},` +
		`{"type":"image","data":"` + b64 + `","mimeType":"` + mimeType + `"}],"timestamp":1792515773671}}`
}

// piToolResultImageLine embeds an image in a tool result, the way Pi records a
// read of an image file.
func piToolResultImageLine(b64 string) string {
	return `{"type":"message","id":"7e89face","parentId":"f052b6ad","timestamp":"2026-10-20T17:02:57.365Z",` +
		`"message":{"role":"toolResult","toolCallId":"call_1","toolName":"read",` +
		`"content":[{"type":"text","text":"Read image file [image/png]"},{"type":"image","data":"` + b64 + `","mimeType":"image/png"}],` +
		`"isError":false,"timestamp":1792515777365}}`
}

func piPNG(payload string) string {
	return base64.StdEncoding.EncodeToString([]byte("\x89PNG\r\n\x1a\n" + payload + strings.Repeat("-pi-image-bytes", 3)))
}

// The core contract for Pi: extract then reinject reproduces the bytes exactly,
// for images in user messages and in tool results alike.
func TestPiCodec_RoundTripByteExact(t *testing.T) {
	t.Parallel()
	c := CodecFor(agent.AgentTypePi)
	if c == nil {
		t.Fatal("expected a codec for Pi")
	}
	user, tool := piPNG("user"), piPNG("tool")
	orig := piUserImageLine(user, "image/png") + "\n" +
		`{"type":"message","id":"a1","parentId":"61c6d42a","message":{"role":"assistant","content":[{"type":"text","text":"a cat"}]}}` + "\n" +
		piToolResultImageLine(tool) + "\n"

	rewritten, assets, err := c.ExtractImages([]byte(orig))
	if err != nil {
		t.Fatalf("ExtractImages: %v", err)
	}
	if len(assets) != 2 {
		t.Fatalf("expected 2 assets, got %d", len(assets))
	}
	for _, b64 := range []string{user, tool} {
		if strings.Contains(string(rewritten), b64) {
			t.Error("base64 must be gone from the rewritten transcript")
		}
	}
	// Only the data value is swapped; the item keeps its shape and mimeType.
	if !strings.Contains(string(rewritten), `{"type":"image","data":"`+placeholderPrefix) {
		t.Errorf("expected the placeholder in the image item's data:\n%s", rewritten)
	}

	restored, err := c.ReinjectImages(rewritten, lookupFrom(assets))
	if err != nil {
		t.Fatalf("ReinjectImages: %v", err)
	}
	if string(restored) != orig {
		t.Fatalf("round trip not byte-exact:\n got: %s\nwant: %s", restored, orig)
	}
}

// The asset's media type comes from the bytes, so a mislabeled mimeType does not
// mislabel the stored asset, and the transcript keeps the declared type.
func TestPiCodec_DetectsMediaTypeFromBytes(t *testing.T) {
	t.Parallel()
	c := CodecFor(agent.AgentTypePi)
	b64 := piPNG("mislabeled")
	orig := piUserImageLine(b64, "image/jpeg") + "\n"

	rewritten, assets, err := c.ExtractImages([]byte(orig))
	if err != nil {
		t.Fatalf("ExtractImages: %v", err)
	}
	if len(assets) != 1 || assets[0].MediaType != mediaTypePNG || !strings.HasSuffix(assets[0].Name, ".png") {
		t.Fatalf("assets = %+v, want one image/png asset", assets)
	}
	if !strings.Contains(string(rewritten), `"mimeType":"image/jpeg"`) {
		t.Error("the declared mimeType must stay as written")
	}
}

// Items that are not Pi images are left alone: Claude-shaped image blocks (no
// top-level data), image items without data, and text that merely contains
// base64.
func TestPiCodec_IgnoresNonPiImages(t *testing.T) {
	t.Parallel()
	c := CodecFor(agent.AgentTypePi)
	b64 := piPNG("not-an-item")
	orig := `{"type":"message","message":{"role":"user","content":[` +
		`{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + b64 + `"}},` +
		`{"type":"image","mimeType":"image/png"},` +
		`{"type":"text","text":"` + b64 + `"}]}}` + "\n"

	rewritten, assets, err := c.ExtractImages([]byte(orig))
	if err != nil {
		t.Fatalf("ExtractImages: %v", err)
	}
	if len(assets) != 0 || string(rewritten) != orig {
		t.Fatalf("expected no change, got %d assets:\n%s", len(assets), rewritten)
	}
}

// With externalization on, the stored Pi transcript carries placeholders, and
// the converted transcript.jsonl built from it keeps the image item with the
// placeholder rather than the base64.
func TestPiCodec_ConvertedTranscriptKeepsPlaceholder(t *testing.T) {
	t.Parallel()
	b64 := piPNG("converted")
	orig := `{"type":"session","version":3,"id":"s1","timestamp":"2026-10-20T17:02:50.000Z","cwd":"/repo"}` + "\n" +
		`{"type":"message","id":"61c6d42a","parentId":null,"timestamp":"2026-10-20T17:02:53.673Z",` +
		`"message":{"role":"user","content":[{"type":"image","data":"` + b64 + `","mimeType":"image/png"}],"timestamp":1792515773671}}` + "\n"

	rewritten, assets, err := CodecFor(agent.AgentTypePi).ExtractImages([]byte(orig))
	if err != nil || len(assets) != 1 {
		t.Fatalf("ExtractImages: %d assets, err %v", len(assets), err)
	}
	converted, err := transcriptlib.Convert(rewritten, transcriptlib.Options{Agent: "pi"})
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	if strings.Contains(string(converted), b64) {
		t.Errorf("converted transcript still carries the base64:\n%s", converted)
	}
	if !strings.Contains(string(converted), `"data":"`+placeholderPrefix+assets[0].Name+`"`) {
		t.Errorf("converted transcript lost the image item's placeholder:\n%s", converted)
	}
}
