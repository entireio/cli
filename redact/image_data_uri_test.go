package redact

import (
	"encoding/base64"
	"math/rand/v2"
	"slices"
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
	png := fakeImageBase64(pngMagic)
	// Drop the final quartet, which carries the = padding, so the base64 ends
	// on a quartet boundary and a base64-shaped next line decodes as a
	// continuation. The decoder skips \r and \n even in strict mode.
	noPadding := jpeg[:len(jpeg)-4]
	const base64ShapedSecret = "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"
	tests := []struct {
		name string
		s    string
		want bool
	}{
		{"jpeg", "data:image/jpeg;base64," + jpeg, true},
		{"jpg alias", "data:image/jpg;base64," + jpeg, true},
		{"png", "data:image/png;base64," + png, true},
		{"upper-case subtype", "data:image/PNG;base64," + png, true},
		{"gif87a", "data:image/gif;base64," + fakeImageBase64([]byte("GIF87a")), true},
		{"gif89a", "data:image/gif;base64," + fakeImageBase64([]byte("GIF89a")), true},
		{"webp", "data:image/webp;base64," + fakeImageBase64([]byte("RIFF\x00\x00\x00\x00WEBP")), true},
		{"png bytes under a jpeg label", "data:image/jpeg;base64," + png, true},
		{"jpeg bytes under a png label", "data:image/png;base64," + jpeg, true},
		{"image bytes under an unsupported subtype", "data:image/avif;base64," + png, false},
		{"subtype with parameters", "data:image/png;name=x;base64," + png, false},
		{"bytes that are no supported image", "data:image/png;base64," + fakeImageBase64(nil), false},
		{"base64 text under an image label", "data:image/png;base64," + base64.StdEncoding.EncodeToString([]byte(highEntropySecret)), false},
		{"svg is text", "data:image/svg+xml;base64," + base64.StdEncoding.EncodeToString([]byte("<svg/>")), false},
		{"not base64", "data:image/jpeg," + jpeg, false},
		{"invalid base64", "data:image/jpeg;base64," + jpeg + "!", false},
		{"empty data", "data:image/jpeg;base64,", false},
		{"embedded in a longer string", "screenshot: data:image/jpeg;base64," + jpeg, false},
		{"secret on the next line", "data:image/jpeg;base64," + noPadding + "\n" + base64ShapedSecret, false},
		{"secret on the next CRLF line", "data:image/jpeg;base64," + noPadding + "\r\n" + base64ShapedSecret, false},
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
	mislabeled := "data:image/jpeg;base64," + fakeImageBase64(pngMagic)
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
			// The label can be wrong: older Codex releases took it from the
			// file extension, and MCP tools declare their own.
			name: "png bytes under a jpeg label",
			line: `{"type":"input_image","image_url":"` + mislabeled + `"}`,
			want: `{"type":"input_image","image_url":"` + mislabeled + `"}`,
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
// bytes are no supported image is scanned like any other string.
func TestJSONLContent_ScansDataURIThatIsNotAnImage(t *testing.T) {
	t.Parallel()
	uri := "data:image/png;base64," + fakeImageBase64(nil)
	got, err := JSONLContent(`{"image_url":"` + uri + `"}`)
	require.NoError(t, err)
	assert.NotContains(t, got, uri)
	assert.Contains(t, got, "REDACTED")
}

// A credential key keeps its placeholder even when its value is shaped like an
// image: the image rule exists for agent image fields, not for secrets.
func TestJSONLContent_CredentialKeyOverridesImageDataURI(t *testing.T) {
	t.Parallel()
	const secret = "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"
	header := base64.StdEncoding.EncodeToString(append(slices.Clone(pngMagic), 'X'))
	got, err := JSONLContent(`{"aws_secret_access_key":"data:image/png;base64,` + header + secret + `"}`)
	require.NoError(t, err)
	assert.NotContains(t, got, secret)
	assert.Contains(t, got, "REDACTED")
}

// Duplicate keys make the raw-text splice ambiguous, so the line goes through
// the structural re-encode, the third consumer of the shared walk. The image
// must survive it byte-for-byte while the secret beside it is redacted.
func TestJSONLContent_ReencodeFallbackPreservesImageDataURI(t *testing.T) {
	t.Parallel()
	uri := "data:image/jpeg;base64," + fakeImageBase64(jpegMagic)
	got, err := JSONLContent(`{"text":"` + highEntropySecret + `","text":"` + highEntropySecret + `","image_url":"` + uri + `"}`)
	require.NoError(t, err)
	assert.Contains(t, got, `"image_url":"`+uri+`"`)
	assert.NotContains(t, got, highEntropySecret)
}
