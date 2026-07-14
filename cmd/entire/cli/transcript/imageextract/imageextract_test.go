package imageextract

import (
	"encoding/base64"
	"errors"
	"math"
	"regexp"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
)

var errTestRand = errors.New("simulated rand failure")

func lookupFrom(assets []Asset) func(string) (Asset, bool) {
	return func(name string) (Asset, bool) {
		for _, a := range assets {
			if a.Name == name {
				return a, true
			}
		}
		return Asset{}, false
	}
}

func claudeLine(b64 string) string {
	return `{"type":"user","message":{"role":"user","content":[` +
		`{"type":"text","text":"look at this"},` +
		`{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + b64 + `"}}` +
		`]}}`
}

// The core contract: extract then reinject reproduces the original bytes exactly.
func TestClaudeCodec_RoundTripByteExact(t *testing.T) {
	t.Parallel()
	c := CodecFor(agent.AgentTypeClaudeCode)
	if c == nil {
		t.Fatal("expected a codec for Claude Code")
	}
	b64 := base64.StdEncoding.EncodeToString([]byte("\x89PNG\r\n\x1a\nfake-png-bytes-with-enough-length-to-be-a-real-image\x00\x01\x02"))
	orig := claudeLine(b64) + "\n{\"type\":\"assistant\",\"message\":{\"content\":[{\"type\":\"text\",\"text\":\"ok\"}]}}\n"

	rewritten, assets, err := c.ExtractImages([]byte(orig))
	if err != nil {
		t.Fatalf("ExtractImages: %v", err)
	}
	if len(assets) != 1 {
		t.Fatalf("expected 1 asset, got %d", len(assets))
	}
	if strings.Contains(string(rewritten), b64) {
		t.Error("base64 must be gone from the rewritten transcript")
	}
	if !strings.Contains(string(rewritten), placeholderPrefix) {
		t.Error("rewritten transcript should carry a placeholder")
	}
	if assets[0].MediaType != mediaTypePNG {
		t.Errorf("asset media type = %q, want image/png", assets[0].MediaType)
	}

	restored, err := c.ReinjectImages(rewritten, lookupFrom(assets))
	if err != nil {
		t.Fatalf("ReinjectImages: %v", err)
	}
	if string(restored) != orig {
		t.Fatalf("round-trip not byte-exact:\n got: %s\nwant: %s", restored, orig)
	}
}

// A transcript with no images is returned unchanged with no assets.
func TestClaudeCodec_NoImagesIsNoOp(t *testing.T) {
	t.Parallel()
	c := CodecFor(agent.AgentTypeClaudeCode)
	orig := `{"type":"user","message":{"role":"user","content":[{"type":"text","text":"hi"}]}}` + "\n"
	rewritten, assets, err := c.ExtractImages([]byte(orig))
	if err != nil {
		t.Fatalf("ExtractImages: %v", err)
	}
	if assets != nil {
		t.Errorf("expected no assets, got %d", len(assets))
	}
	if string(rewritten) != orig {
		t.Errorf("no-image transcript should be unchanged")
	}
}

// Identical images dedupe to one asset but round-trip both occurrences.
func TestClaudeCodec_DedupesIdenticalImages(t *testing.T) {
	t.Parallel()
	c := CodecFor(agent.AgentTypeClaudeCode)
	b64 := base64.StdEncoding.EncodeToString([]byte("same-image-bytes-repeated-with-enough-length-to-externalize"))
	orig := claudeLine(b64) + "\n" + claudeLine(b64) + "\n"
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
		t.Fatalf("round-trip mismatch for duplicated image")
	}
}

// When one image's base64 is a substring of another's, the round trip must still
// be byte-exact (longest-first replacement guarantees this).
func TestClaudeCodec_SubstringImagesRoundTrip(t *testing.T) {
	t.Parallel()
	c := CodecFor(agent.AgentTypeClaudeCode)
	long := base64.StdEncoding.EncodeToString([]byte(
		"prefix-bytes-AAAABBBBCCCCDDDD-and-a-considerably-longer-image-tail-payload-so-a-64-char-substring-fits-xyz"))
	// A canonical base64 substring of long (>= threshold) that decodes/re-encodes
	// cleanly, taken from a non-zero offset so it is genuinely embedded.
	var short string
	for i := 4; i+64 <= len(long); i += 4 {
		cand := long[i : i+64]
		if raw, err := base64.StdEncoding.DecodeString(cand); err == nil && base64.StdEncoding.EncodeToString(raw) == cand {
			short = cand
			break
		}
	}
	if short == "" {
		t.Fatal("could not construct a canonical base64 substring")
	}
	// Shorter block first, so first-seen order would (without the sort) replace it
	// before the containing longer value.
	orig := claudeLine(short) + "\n" + claudeLine(long) + "\n"
	rewritten, assets, err := c.ExtractImages([]byte(orig))
	if err != nil {
		t.Fatalf("ExtractImages: %v", err)
	}
	if len(assets) != 2 {
		t.Fatalf("expected 2 assets, got %d", len(assets))
	}
	// Longest-first replacement means both assets have a live placeholder (neither
	// is orphaned by the other's swap).
	for _, a := range assets {
		if !strings.Contains(string(rewritten), placeholderPrefix+a.Name) {
			t.Errorf("asset %s has no placeholder in the rewritten transcript (orphaned)", a.Name)
		}
	}
	restored, err := c.ReinjectImages(rewritten, lookupFrom(assets))
	if err != nil {
		t.Fatalf("ReinjectImages: %v", err)
	}
	if string(restored) != orig {
		t.Fatalf("substring round-trip not byte-exact:\n got: %s\nwant: %s", restored, orig)
	}
}

// Even if the id source degenerates to a constant, distinct images must still get
// distinct names so the round trip stays byte-exact (no asset shadows another).
func TestClaudeCodec_DistinctNamesUnderCollidingIDSource(t *testing.T) {
	c := CodecFor(agent.AgentTypeClaudeCode)
	orig := newAssetID
	newAssetID = func() (string, error) { return "deadbeefdeadbeefdeadbeefdeadbeef", nil } // constant
	defer func() { newAssetID = orig }()

	img1 := base64.StdEncoding.EncodeToString([]byte("first-distinct-image-payload-long-enough-to-externalize"))
	img2 := base64.StdEncoding.EncodeToString([]byte("second-distinct-image-payload-long-enough-to-externalize"))
	in := claudeLine(img1) + "\n" + claudeLine(img2) + "\n"

	rewritten, assets, err := c.ExtractImages([]byte(in))
	if err != nil {
		t.Fatalf("ExtractImages: %v", err)
	}
	if len(assets) != 2 {
		t.Fatalf("want 2 assets, got %d", len(assets))
	}
	if assets[0].Name == assets[1].Name {
		t.Fatalf("distinct images got the same name %q", assets[0].Name)
	}
	restored, err := c.ReinjectImages(rewritten, lookupFrom(assets))
	if err != nil {
		t.Fatalf("ReinjectImages: %v", err)
	}
	if string(restored) != in {
		t.Fatalf("round trip broke under colliding id source:\n got: %s\nwant: %s", restored, in)
	}
}

// The same base64 appearing in both an image and a text field round-trips
// byte-exactly: the value swap is value-preserving and reversible, so every
// occurrence is restored to the identical bytes on reinject.
func TestClaudeCodec_Base64InTextRoundTrips(t *testing.T) {
	t.Parallel()
	c := CodecFor(agent.AgentTypeClaudeCode)
	b64 := base64.StdEncoding.EncodeToString([]byte("shared-image-and-text-payload-long-enough-to-externalize"))
	textLine := `{"type":"user","message":{"role":"user","content":[{"type":"text","text":"raw was ` + b64 + `"}]}}`
	in := textLine + "\n" + claudeLine(b64) + "\n"

	rewritten, assets, err := c.ExtractImages([]byte(in))
	if err != nil {
		t.Fatalf("ExtractImages: %v", err)
	}
	if len(assets) != 1 {
		t.Fatalf("want 1 asset, got %d", len(assets))
	}
	restored, err := c.ReinjectImages(rewritten, lookupFrom(assets))
	if err != nil {
		t.Fatalf("ReinjectImages: %v", err)
	}
	if string(restored) != in {
		t.Fatalf("round trip not byte-exact:\n got: %s\nwant: %s", restored, in)
	}
}

// Regression: Claude Code serializes image content blocks with a space after the
// colon ("data": "<b64>") as well as compactly ("data":"<b64>"). Both forms must
// externalize and round-trip. (A data-field-scoped swap missed the spaced form.)
func TestClaudeCodec_SpacedAndCompactDataFields(t *testing.T) {
	t.Parallel()
	c := CodecFor(agent.AgentTypeClaudeCode)
	for _, tc := range []struct {
		name, line string
	}{
		{"compact", `{"type": "image", "source": {"type": "base64", "media_type": "image/png", "data":"%s"}}`},
		{"spaced", `{"type": "image", "source": {"type": "base64", "media_type": "image/png", "data": "%s"}}`},
	} {
		b64 := base64.StdEncoding.EncodeToString([]byte("spaced-vs-compact-payload-long-enough-to-externalize-" + tc.name))
		in := strings.Replace(tc.line, "%s", b64, 1) + "\n"
		rewritten, assets, err := c.ExtractImages([]byte(in))
		if err != nil {
			t.Fatalf("[%s] ExtractImages: %v", tc.name, err)
		}
		if len(assets) != 1 {
			t.Fatalf("[%s] want 1 asset, got %d", tc.name, len(assets))
		}
		if strings.Contains(string(rewritten), b64) {
			t.Errorf("[%s] base64 not externalized", tc.name)
		}
		restored, err := c.ReinjectImages(rewritten, lookupFrom(assets))
		if err != nil {
			t.Fatalf("[%s] ReinjectImages: %v", tc.name, err)
		}
		if string(restored) != in {
			t.Fatalf("[%s] round trip not byte-exact", tc.name)
		}
	}
}

// A crypto/rand failure surfaces as an error instead of a silent all-zero id.
func TestClaudeCodec_IDGenerationErrorSurfaces(t *testing.T) {
	c := CodecFor(agent.AgentTypeClaudeCode)
	orig := newAssetID
	newAssetID = func() (string, error) { return "", errTestRand }
	defer func() { newAssetID = orig }()

	b64 := base64.StdEncoding.EncodeToString([]byte("payload-long-enough-to-externalize-and-trigger-id-gen"))
	_, _, err := c.ExtractImages([]byte(claudeLine(b64) + "\n"))
	if err == nil {
		t.Fatal("expected an error when id generation fails, got nil")
	}
}

// An array-rooted JSONL line carrying an image is walked like an object line.
func TestClaudeCodec_ArrayRootedLine(t *testing.T) {
	t.Parallel()
	c := CodecFor(agent.AgentTypeClaudeCode)
	b64 := base64.StdEncoding.EncodeToString([]byte("array-rooted-line-image-payload-long-enough-to-externalize"))
	in := `[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + b64 + `"}}]` + "\n"
	rewritten, assets, err := c.ExtractImages([]byte(in))
	if err != nil {
		t.Fatalf("ExtractImages: %v", err)
	}
	if len(assets) != 1 {
		t.Fatalf("array-rooted line: want 1 asset, got %d", len(assets))
	}
	restored, err := c.ReinjectImages(rewritten, lookupFrom(assets))
	if err != nil {
		t.Fatalf("ReinjectImages: %v", err)
	}
	if string(restored) != in {
		t.Fatalf("array-rooted round trip not byte-exact")
	}
}

// Base64 values too short to be a real image are left inline (and can therefore
// never collide with a placeholder's hex id).
func TestClaudeCodec_LeavesTinyBase64Inline(t *testing.T) {
	t.Parallel()
	c := CodecFor(agent.AgentTypeClaudeCode)
	tiny := base64.StdEncoding.EncodeToString([]byte("tiny-blob")) // < minExternalizedBase64Len
	if len(tiny) >= minExternalizedBase64Len {
		t.Fatalf("test fixture too long: %d", len(tiny))
	}
	orig := claudeLine(tiny) + "\n"
	rewritten, assets, err := c.ExtractImages([]byte(orig))
	if err != nil {
		t.Fatalf("ExtractImages: %v", err)
	}
	if len(assets) != 0 || string(rewritten) != orig {
		t.Errorf("tiny base64 must be left inline; assets=%d changed=%v", len(assets), string(rewritten) != orig)
	}
}

// Images whose decoded bytes exceed maxExternalizedImageBytes are left inline: as
// a single asset blob they could become an unpushable git object, so (like the
// Cursor sidecar path) they stay in the transcript, which is chunked to stay
// pushable. Not parallel: it lowers the shared cap to avoid a 50MB fixture.
func TestClaudeCodec_LeavesOversizedImageInline(t *testing.T) {
	c := CodecFor(agent.AgentTypeClaudeCode)

	restore := maxExternalizedImageBytes
	maxExternalizedImageBytes = 8
	t.Cleanup(func() { maxExternalizedImageBytes = restore })

	// 72 bytes: over the lowered cap, and its base64 clears minExternalizedBase64Len
	// so only the size guard (not the min-length filter) can keep it inline.
	raw := append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 64)...)
	b64 := base64.StdEncoding.EncodeToString(raw)
	if len(b64) < minExternalizedBase64Len {
		t.Fatalf("fixture too short to exercise the max guard: %d", len(b64))
	}
	orig := claudeLine(b64) + "\n"
	rewritten, assets, err := c.ExtractImages([]byte(orig))
	if err != nil {
		t.Fatalf("ExtractImages: %v", err)
	}
	if len(assets) != 0 || string(rewritten) != orig {
		t.Errorf("oversized image must be left inline; assets=%d changed=%v", len(assets), string(rewritten) != orig)
	}
}

// Non-base64 image sources (e.g. url) and non-decodable data are left inline.
func TestClaudeCodec_LeavesNonBase64Inline(t *testing.T) {
	t.Parallel()
	c := CodecFor(agent.AgentTypeClaudeCode)
	orig := `{"type":"user","message":{"content":[{"type":"image","source":{"type":"url","url":"https://x/y.png"}}]}}` + "\n"
	rewritten, assets, err := c.ExtractImages([]byte(orig))
	if err != nil {
		t.Fatalf("ExtractImages: %v", err)
	}
	if len(assets) != 0 || string(rewritten) != orig {
		t.Errorf("url image source must be left inline; assets=%d changed=%v", len(assets), string(rewritten) != orig)
	}
}

// Agents that don't inline images in the transcript have no codec (graceful
// no-op upstream). Cursor is included deliberately: its images live in a separate
// SQLite store, captured via the SidecarImageProvider path, not a transcript codec.
func TestCodecFor_NonImageAgentsAreNil(t *testing.T) {
	t.Parallel()
	for _, at := range []string{"Cursor", "Gemini CLI", "OpenCode", "Pi", "Factory AI Droid", "Copilot CLI"} {
		if CodecFor(types.AgentType(at)) != nil {
			t.Errorf("agent %q should not have an image codec yet", at)
		}
	}
}

// The placeholder must stay low-entropy so the downstream redaction pass never
// flags it. Redaction's entropy detector runs over each [A-Za-z0-9+_=-]{10,}
// RUN (threshold 4.5 bits/char), not the whole string, so mirror that here.
func TestPlaceholder_RunsAreLowEntropy(t *testing.T) {
	t.Parallel()
	c := CodecFor(agent.AgentTypeClaudeCode)
	b64 := base64.StdEncoding.EncodeToString([]byte("entropy-check-bytes-xyz-padded-to-exceed-the-externalize-threshold"))
	rewritten, _, err := c.ExtractImages([]byte(claudeLine(b64) + "\n"))
	if err != nil {
		t.Fatalf("ExtractImages: %v", err)
	}
	ph := placeholderRe.Find(rewritten)
	if ph == nil {
		t.Fatal("no placeholder produced")
	}
	runRe := regexp.MustCompile(`[A-Za-z0-9+_=-]{10,}`)
	runs := runRe.FindAll(ph, -1)
	if len(runs) == 0 {
		t.Fatalf("expected at least one detector-sized run in %s", ph)
	}
	for _, run := range runs {
		if e := shannonBitsPerChar(run); e >= 4.5 {
			t.Errorf("placeholder run %q entropy %.2f >= 4.5 — redaction could flag it", run, e)
		}
	}
}

func shannonBitsPerChar(b []byte) float64 {
	if len(b) == 0 {
		return 0
	}
	var counts [256]int
	for _, c := range b {
		counts[c]++
	}
	var e float64
	n := float64(len(b))
	for _, c := range counts {
		if c == 0 {
			continue
		}
		p := float64(c) / n
		e -= p * math.Log2(p)
	}
	return e
}
