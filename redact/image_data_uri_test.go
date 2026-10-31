package redact

import (
	"encoding/base64"
	"math/rand/v2"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeImageBase64 returns the base64 of magic followed by deterministic
// pseudo-random bytes. Compressed image data looks like this to the scanners,
// so without the data URI rule the entropy layer redacts it.
func fakeImageBase64(magic []byte) string {
	r := rand.New(rand.NewChaCha8([32]byte{1}))
	raw := make([]byte, 4096)
	for i := range raw {
		raw[i] = byte(r.Uint32())
	}
	copy(raw, magic)
	return base64.StdEncoding.EncodeToString(raw)
}

var (
	jpegMagic = []byte{0xFF, 0xD8, 0xFF}
	pngMagic  = []byte("\x89PNG\r\n\x1a\n")
)

func TestIsInlineImageDataURI(t *testing.T) {
	t.Parallel()
	jpeg := fakeImageBase64(jpegMagic)
	tests := []struct {
		name string
		s    string
		want bool
	}{
		{"jpeg", "data:image/jpeg;base64," + jpeg, true},
		{"jpg alias", "data:image/jpg;base64," + jpeg, true},
		{"png", "data:image/png;base64," + fakeImageBase64(pngMagic), true},
		{"gif", "data:image/gif;base64," + fakeImageBase64([]byte("GIF89a")), true},
		{"webp", "data:image/webp;base64," + fakeImageBase64([]byte("RIFF\x00\x00\x00\x00WEBP")), true},
		{"subtype does not match bytes", "data:image/png;base64," + jpeg, false},
		{"base64 text under an image label", "data:image/png;base64," + base64.StdEncoding.EncodeToString([]byte(highEntropySecret)), false},
		{"svg is text", "data:image/svg+xml;base64," + base64.StdEncoding.EncodeToString([]byte("<svg/>")), false},
		{"not base64", "data:image/jpeg," + jpeg, false},
		{"invalid base64", "data:image/jpeg;base64," + jpeg + "!", false},
		{"empty data", "data:image/jpeg;base64,", false},
		{"embedded in a longer string", "screenshot: data:image/jpeg;base64," + jpeg, false},
		{"non-image data URI", "data:text/plain;base64," + jpeg, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, isInlineImageDataURI(tt.s))
		})
	}
}

// Codex writes images as data URIs in places shouldSkipJSONLObject does not
// match. Redacting their base64 corrupts the image, so these must survive
// byte-for-byte while everything around them is still scanned.
func TestJSONLContent_PreservesCodexImageDataURIs(t *testing.T) {
	t.Parallel()
	uri := "data:image/jpeg;base64," + fakeImageBase64(jpegMagic)
	tests := []jsonlFieldCase{
		{
			name: "user message input_image",
			line: `{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_image","image_url":"` + uri + `"}]}}`,
			want: `{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_image","image_url":"` + uri + `"}]}}`,
		},
		{
			name: "function_call_output input_image",
			line: `{"type":"response_item","payload":{"type":"function_call_output","call_id":"c1","output":[{"type":"input_image","image_url":"` + uri + `"}]}}`,
			want: `{"type":"response_item","payload":{"type":"function_call_output","call_id":"c1","output":[{"type":"input_image","image_url":"` + uri + `"}]}}`,
		},
		{
			name: "bare screenshot url in metadata",
			line: `{"metadata":{"codex/toolSurface":{"screenshot":{"url":"` + uri + `"}}}}`,
			want: `{"metadata":{"codex/toolSurface":{"screenshot":{"url":"` + uri + `"}}}}`,
		},
		{
			name: "sibling secret is still redacted",
			line: `{"content":[{"type":"input_text","text":"` + highEntropySecret + `"},{"type":"input_image","image_url":"` + uri + `","detail":"` + highEntropySecret + `"}]}`,
			want: `{"content":[{"type":"input_text","text":"REDACTED"},{"type":"input_image","image_url":"` + uri + `","detail":"REDACTED"}]}`,
		},
	}
	assertJSONLFieldCases(t, tests)
}

// The rule must not turn an image label into a hiding place: a data URI whose
// bytes are not the image it names is scanned like any other string.
func TestJSONLContent_ScansDataURIThatIsNotAnImage(t *testing.T) {
	t.Parallel()
	line := `{"image_url":"data:image/png;base64,` + fakeImageBase64(jpegMagic) + `"}`
	got, err := JSONLContent(line)
	require.NoError(t, err)
	assert.Contains(t, got, "REDACTED")
}
