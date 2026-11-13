package redact

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"image"
	"image/gif"
	"image/jpeg"
	"image/png"
	"math/rand/v2"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeImageBase64 returns the base64 of fakeImage(magic).
func fakeImageBase64(magic []byte) string {
	return base64.StdEncoding.EncodeToString(fakeImage(magic))
}

// fakeImage returns a whole image, in the format whose magic number is magic,
// of deterministic noise. Compressed noise looks random to the scanners, so
// without the data URI rule the entropy layer redacts it. A nil magic returns
// noise bytes that are no image.
func fakeImage(magic []byte) []byte {
	r := rand.New(rand.NewChaCha8([32]byte{1}))
	if magic == nil {
		raw := make([]byte, 4096)
		for i := range raw {
			raw[i] = byte(r.Uint32())
		}
		return raw
	}
	if bytes.HasPrefix(magic, riffTag) {
		return fakeWebP(r)
	}
	img := image.NewNRGBA(image.Rect(0, 0, 48, 48))
	for i := range img.Pix {
		img.Pix[i] = byte(r.Uint32())
	}
	var buf bytes.Buffer
	var err error
	switch {
	case bytes.Equal(magic, jpegMagic):
		err = jpeg.Encode(&buf, img, &jpeg.Options{Quality: 90})
	case bytes.Equal(magic, pngMagic):
		err = png.Encode(&buf, img)
	default: // GIF87a or GIF89a; the encoder writes GIF89a
		err = gif.Encode(&buf, img, nil)
	}
	if err != nil {
		panic(err)
	}
	raw := buf.Bytes()
	copy(raw, magic)
	return raw
}

// fakeWebP returns a lossless WebP container around noise: no decoder would
// accept the bitstream, but its framing is whole.
func fakeWebP(r *rand.Rand) []byte {
	data := make([]byte, 4096)
	for i := range data {
		data[i] = byte(r.Uint32())
	}
	data[0] = 0x2F
	raw := append([]byte("RIFF\x00\x00\x00\x00WEBPVP8L"), binary.LittleEndian.AppendUint32(nil, uint32(len(data)))...)
	raw = append(raw, data...)
	binary.LittleEndian.PutUint32(raw[4:8], uint32(len(raw)-8))
	return raw
}

// withTokenAppended returns the base64 of raw with a token's bytes after it.
func withTokenAppended(raw []byte) string {
	return base64.StdEncoding.EncodeToString(append(slices.Clone(raw), highEntropySecret...))
}

var (
	jpegMagic = []byte{0xFF, 0xD8, 0xFF}
	pngMagic  = []byte("\x89PNG\r\n\x1a\n")
)

// jpegWithFrame returns the base64 of a JPEG whose frame segment has the
// given marker and sample precision, with one scan of a few bytes. Go's
// decoder reads only baseline and progressive 8-bit frames.
func jpegWithFrame(marker, precision byte) string {
	// Precision, height 16, width 16, one component with sampling 1x1 and
	// quantization table 0.
	frame := []byte{0xFF, marker, 0x00, 0x0B, precision, 0x00, 0x10, 0x00, 0x10, 0x01, 0x01, 0x11, 0x00}
	// One component with tables 0, then spectral selection and approximation.
	scan := []byte{0xFF, 0xDA, 0x00, 0x08, 0x01, 0x01, 0x00, 0x01, 0x00, 0x00}
	return jpegWithHeaders(frame, scan)
}

// jpegWithHeaders returns the base64 of a JPEG holding the given frame and
// scan segments, a few bytes of scan data and the end marker.
func jpegWithHeaders(frame, scan []byte) string {
	raw := []byte{0xFF, 0xD8}
	raw = append(raw, 0xFF, 0xE0, 0x00, 0x10, 'J', 'F', 'I', 'F', 0, 1, 1, 0, 0, 1, 0, 1, 0, 0)
	raw = append(raw, frame...)
	raw = append(raw, scan...)
	raw = append(raw, 0x12, 0x34, 0x56, 0x78, 0xFF, 0xD9)
	return base64.StdEncoding.EncodeToString(raw)
}

func TestIsInlineImageDataURI(t *testing.T) {
	t.Parallel()
	jpeg := fakeImageBase64(jpegMagic)
	png := fakeImageBase64(pngMagic)
	// Drop the final quartet, which carries the = padding, so the base64 ends
	// on a quartet boundary and a base64-shaped next line decodes as a
	// continuation. The decoder skips \r and \n even in strict mode.
	noPadding := jpeg[:len(jpeg)-4]
	const base64ShapedSecret = "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"
	pngSignatureThenToken := base64.StdEncoding.EncodeToString(append(slices.Clone(pngMagic), 'X')) + base64ShapedSecret
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
		{"token after a jpeg signature", "data:image/jpeg;base64,/9j/" + base64ShapedSecret, false},
		{"token after a png signature", "data:image/png;base64," + pngSignatureThenToken, false},
		{"token appended to a png", "data:image/png;base64," + withTokenAppended(fakeImage(pngMagic)), false},
		{"token appended to a jpeg", "data:image/jpeg;base64," + withTokenAppended(fakeImage(jpegMagic)), false},
		{"token appended to a gif", "data:image/gif;base64," + withTokenAppended(fakeImage([]byte("GIF89a"))), false},
		{"token appended to a webp", "data:image/webp;base64," + withTokenAppended(fakeImage(riffTag)), false},
		{"png without its end chunk", "data:image/png;base64," + base64.StdEncoding.EncodeToString(fakeImage(pngMagic)[:len(fakeImage(pngMagic))-len(pngEnd)]), false},
		{"lossless jpeg", "data:image/jpeg;base64," + jpegWithFrame(0xC3, 8), true},
		{"12-bit jpeg", "data:image/jpeg;base64," + jpegWithFrame(0xC1, 12), true},
		{"arithmetic-coded jpeg", "data:image/jpeg;base64," + jpegWithFrame(0xC9, 8), true},
		{"jpeg markers without a frame header", "data:image/jpeg;base64," + jpegWithFrame(0xC4, 8), false},
		{"jpeg frame header without its component", "data:image/jpeg;base64," + jpegWithHeaders(
			[]byte{0xFF, 0xC0, 0x00, 0x08, 0x08, 0x00, 0x10, 0x00, 0x10, 0x01},
			[]byte{0xFF, 0xDA, 0x00, 0x08, 0x01, 0x01, 0x00, 0x01, 0x00, 0x00}), false},
		{"jpeg scan header cut short", "data:image/jpeg;base64," + jpegWithHeaders(
			[]byte{0xFF, 0xC0, 0x00, 0x0B, 0x08, 0x00, 0x10, 0x00, 0x10, 0x01, 0x01, 0x11, 0x00},
			[]byte{0xFF, 0xDA, 0x00, 0x02}), false},
		{"jpeg scan header without components", "data:image/jpeg;base64," + jpegWithHeaders(
			[]byte{0xFF, 0xC0, 0x00, 0x0B, 0x08, 0x00, 0x10, 0x00, 0x10, 0x01, 0x01, 0x11, 0x00},
			[]byte{0xFF, 0xDA, 0x00, 0x06, 0x00, 0x00, 0x01, 0x00}), false},
		{"jpeg without its end marker", "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(fakeImage(jpegMagic)[:len(fakeImage(jpegMagic))-len(jpegEnd)]), false},
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

// A credential key keeps its value scanned even when it is a whole image: the
// image rule exists for agent image fields, not for secrets.
func TestJSONLContent_CredentialKeyOverridesImageDataURI(t *testing.T) {
	t.Parallel()
	uri := "data:image/png;base64," + fakeImageBase64(pngMagic)
	got, err := JSONLContent(`{"aws_secret_access_key":"` + uri + `"}`)
	require.NoError(t, err)
	assert.NotContains(t, got, uri)
	assert.Contains(t, got, "REDACTED")
}

// Outside a connection object a bare password, secret or token key is not a
// credential key, but it still names a secret, so its value is scanned even
// when it is a whole image, and so is one under a key with a password stem.
// Failing closed has a cost: the image's compressed bytes look like a secret
// to the entropy layer. Under a key that names no secret, the same image is
// preserved.
func TestJSONLContent_SecretKeyOverridesImageDataURI(t *testing.T) {
	t.Parallel()
	uri := "data:image/jpeg;base64," + fakeImageBase64(jpegMagic)
	for _, key := range []string{"password", "passwd", "pwd", "pass", "secret", "token", "userpassword", "db_pw"} {
		t.Run(key, func(t *testing.T) {
			t.Parallel()
			got, err := JSONLContent(`{"` + key + `":"` + uri + `"}`)
			require.NoError(t, err)
			assert.NotContains(t, got, uri)
			assert.Contains(t, got, "REDACTED")
		})
	}
	tests := []jsonlFieldCase{
		{
			name: "image under a key with a credential word but no secret segment",
			line: `{"author_avatar":"` + uri + `"}`,
			want: `{"author_avatar":"` + uri + `"}`,
		},
	}
	assertJSONLFieldCases(t, tests)
}

// The image rule reads only the key, so a preserved image is never scanned to
// decide it: keyContextFindsMore over a 1 MiB image took seconds.
func TestImageKeyRevealsSecret(t *testing.T) {
	t.Parallel()
	tests := []struct {
		key  string
		want bool
	}{
		{"password", true},
		{"passwd", true},
		{"pwd", true},
		{"pass", true},
		{"secret", true},
		{"token", true},
		{"key", true},
		{"userpassword", true},
		{"userPassword", true},
		{"mypasswd", true},
		{"user_psw", true},
		{"db_pw", true},
		{"api_key", true},
		{"image_url", false},
		{"url", false},
		{"screenshot", false},
		{"author_avatar", false},
		{"keys", false},
		{"passport_photo", false},
		{"upward", false},
	}
	for _, tt := range tests {
		t.Run(tt.key, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, imageKeyRevealsSecret(tt.key, false))
		})
	}
}

// skipLeaf rejects a string that is no image at its prefix, before it reads
// the key. Most leaves in a transcript are text, and reading every key made
// about a fifth more allocations on a real one. AllocsPerRun sets GOMAXPROCS,
// so this test does not run in parallel.
func TestSkipLeaf_ReadsKeyOnlyForImages(t *testing.T) {
	uri := "data:image/jpeg;base64," + fakeImageBase64(jpegMagic)
	assert.False(t, skipLeaf(false, "userPassword", false, uri))
	assert.True(t, skipLeaf(false, "authorAvatar", false, uri))
	allocs := testing.AllocsPerRun(100, func() {
		skipLeaf(false, "authorName", false, "Ada Lovelace")
	})
	assert.Zero(t, allocs)
}

// customRuleReadsValue stops at a rule's first match past the key, so a rule
// that matches every byte of an image costs no more than one that matches
// once, and a match in the key alone reads nothing of the value. Custom rules
// are process-wide, so this test and the next do not run in parallel.
func TestCustomRuleReadsValue(t *testing.T) {
	resetCustomRulesForTest(t)
	uri := "data:image/jpeg;base64," + fakeImageBase64(jpegMagic)
	ConfigureCustomRules(CustomRulesConfig{Inline: map[string]string{"letters": `[A-Za-z]`}})
	assert.True(t, customRuleReadsValue("image_url", uri))
	ConfigureCustomRules(CustomRulesConfig{Inline: map[string]string{"key": `image_url`}})
	assert.False(t, customRuleReadsValue("image_url", uri))
}

// A custom rule written for one field still redacts an image under it.
func TestJSONLContent_CustomRuleOverridesImageDataURI(t *testing.T) {
	resetCustomRulesForTest(t)
	ConfigureCustomRules(CustomRulesConfig{Inline: map[string]string{"blob": `"auth_blob":"[^"]+"`}})
	uri := "data:image/jpeg;base64," + fakeImageBase64(jpegMagic)
	got, err := JSONLContent(`{"auth_blob":"` + uri + `","image_url":"` + uri + `"}`)
	require.NoError(t, err)
	assert.Equal(t, `{"auth_blob":"REDACTED","image_url":"`+uri+`"}`, got)
}

// A token after an image signature is no image, so it is scanned under any
// key and redacts as it did before the image rule: with the key-context
// placeholder under a password key, and the entropy layer's elsewhere.
func TestJSONLContent_ScansTokenAfterImageSignature(t *testing.T) {
	t.Parallel()
	const jpegShaped = "data:image/jpeg;base64,/9j/wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"
	uri := "data:image/jpeg;base64," + fakeImageBase64(jpegMagic)
	tests := []jsonlFieldCase{
		{
			name: "password",
			line: `{"password":"` + jpegShaped + `"}`,
			want: `{"password":"REDACTED"}`,
		},
		{
			name: "passwd beside a preserved image",
			line: `{"passwd":"` + jpegShaped + `","image_url":"` + uri + `"}`,
			want: `{"passwd":"REDACTED","image_url":"` + uri + `"}`,
		},
		{
			name: "secret",
			line: `{"secret":"` + jpegShaped + `"}`,
			want: `{"secret":"data:image/jpeg;base64,REDACTED"}`,
		},
		{
			name: "image field",
			line: `{"image_url":"` + jpegShaped + `"}`,
			want: `{"image_url":"data:image/jpeg;base64,REDACTED"}`,
		},
		{
			name: "text",
			line: `{"text":"` + jpegShaped + `"}`,
			want: `{"text":"data:image/jpeg;base64,REDACTED"}`,
		},
	}
	assertJSONLFieldCases(t, tests)
}

// A token appended to a real image breaks its end marker, so the whole string
// is scanned. The token redacts, and so does the image, as it did before the
// image rule.
func TestJSONLContent_ScansTokenAppendedToImage(t *testing.T) {
	t.Parallel()
	line := `{"image_url":"data:image/png;base64,` + withTokenAppended(fakeImage(pngMagic)) + `"}`
	got, err := JSONLContent(line)
	require.NoError(t, err)
	assert.NotEqual(t, line, got)
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
