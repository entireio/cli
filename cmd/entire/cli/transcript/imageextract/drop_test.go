package imageextract

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image/gif"
	"image/jpeg"
	"image/png"
	"reflect"
	"strings"
	"testing"
)

// fakeImage returns base64 for bytes that start with the given magic number
// and are long enough to clear the minimum image length.
func fakeImage(magic, tag string) string {
	return base64.StdEncoding.EncodeToString([]byte(magic + strings.Repeat(tag+"-payload-", 12)))
}

const (
	magicPNG  = "\x89PNG\r\n\x1a\n"
	magicJPEG = "\xff\xd8\xff\xe0"
	magicGIF  = "GIF89a"
	magicWEBP = "RIFF\x00\x00\x00\x00WEBP"
)

func requireValidJSONL(t *testing.T, transcript []byte) {
	t.Helper()
	for i, line := range bytes.Split(bytes.TrimSpace(transcript), []byte("\n")) {
		if !json.Valid(line) {
			t.Fatalf("line %d is not valid JSON after the drop: %s", i, line)
		}
	}
}

func TestDropImages_ReplacesEveryKnownShape(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		line func(b64 string) string
	}{
		{
			name: "claude image block",
			line: func(b64 string) string {
				return `{"type":"user","message":{"content":[{"type":"text","text":"look"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + b64 + `"}}]}}`
			},
		},
		{
			name: "claude image inside a tool result",
			line: func(b64 string) string {
				return `{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t1","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + b64 + `"}}]}]}}`
			},
		},
		{
			name: "pi image item",
			line: func(b64 string) string {
				return `{"type":"message","message":{"role":"user","content":[{"type":"image","data":"` + b64 + `","mimeType":"image/png"}]}}`
			},
		},
		{
			name: "codex input_image data uri",
			line: func(b64 string) string {
				return `{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_image","image_url":"data:image/png;base64,` + b64 + `"}]}}`
			},
		},
		{
			name: "data uri embedded in a longer string",
			line: func(b64 string) string {
				return `{"type":"response_item","payload":{"type":"function_call_output","output":"screenshot: data:image/png;base64,` + b64 + ` (end)"}}`
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			b64 := fakeImage(magicPNG, tt.name)
			in := []byte(`{"type":"user","message":{"content":"hello"}}` + "\n" + tt.line(b64) + "\n")

			out := DropImages(in)

			if bytes.Contains(out, []byte(b64)) {
				t.Fatal("the image is still in the transcript")
			}
			if !bytes.Contains(out, []byte(placeholderPNG)) {
				t.Errorf("expected the 1x1 PNG placeholder in the output: %s", out)
			}
			if bytes.Contains(out, []byte(placeholderPrefix)) {
				t.Error("a dropped image must not leave an asset placeholder")
			}
			if !bytes.Contains(out, []byte(`"hello"`)) {
				t.Error("lines without images must be kept")
			}
			requireValidJSONL(t, out)
		})
	}
}

func TestDropImages_PlaceholderMatchesTheImageType(t *testing.T) {
	t.Parallel()

	undecodable := strings.Repeat("not base64 at all! ", 6)

	tests := []struct {
		name     string
		data     string
		declared string
		want     string
	}{
		{"png bytes", fakeImage(magicPNG, "png"), "image/png", placeholderPNG},
		{"jpeg bytes", fakeImage(magicJPEG, "jpeg"), "image/jpeg", placeholderJPEG},
		{"gif bytes", fakeImage(magicGIF, "gif"), "image/gif", placeholderGIF},
		{"webp bytes", fakeImage(magicWEBP, "webp"), "image/webp", placeholderWEBP},
		{"bytes win over a wrong declared type", fakeImage(magicPNG, "mislabelled"), "image/jpeg", placeholderPNG},
		{"unknown bytes fall back to the declared type", fakeImage("????", "unknown"), "image/jpeg", placeholderJPEG},
		{"unknown bytes and unknown type fall back to png", fakeImage("????", "heic"), "image/heic", placeholderPNG},
		{"undecodable data falls back to the declared type", undecodable, "image/gif", placeholderGIF},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			data, err := json.Marshal(tt.data)
			if err != nil {
				t.Fatal(err)
			}
			in := []byte(`{"type":"image","source":{"type":"base64","media_type":"` + tt.declared + `","data":` + string(data) + `}}` + "\n")

			out := DropImages(in)

			if bytes.Contains(out, data[1:len(data)-1]) {
				t.Fatal("the image is still in the transcript")
			}
			if !bytes.Contains(out, []byte(`"data":"`+tt.want+`"`)) {
				t.Errorf("wrong placeholder: %s", out)
			}
			if !bytes.Contains(out, []byte(`"media_type":"`+tt.declared+`"`)) {
				t.Error("the declared media type must be left as written")
			}
			requireValidJSONL(t, out)
		})
	}
}

func TestDropImages_DropsImagesExternalizationLeavesInline(t *testing.T) {
	t.Parallel()

	t.Run("escaped slashes", func(t *testing.T) {
		t.Parallel()
		// Bytes chosen so the base64 contains '/', which some serializers write as "\/".
		b64 := base64.StdEncoding.EncodeToString([]byte(magicPNG + strings.Repeat("\xff\xff\xfe", 30)))
		if !strings.Contains(b64, "/") {
			t.Fatal("fixture must contain a slash")
		}
		escaped := strings.ReplaceAll(b64, "/", `\/`)
		in := []byte(`{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + escaped + `"}}` + "\n")

		out := DropImages(in)

		if bytes.Contains(out, []byte(escaped)) {
			t.Fatal("an image written with escaped slashes was not dropped")
		}
		requireValidJSONL(t, out)
	})

	t.Run("non-canonical base64", func(t *testing.T) {
		t.Parallel()
		// Unpadded base64 does not round-trip through the standard encoding, so
		// externalization leaves it inline. Drop must still remove it.
		b64 := strings.TrimRight(base64.StdEncoding.EncodeToString([]byte(magicPNG+strings.Repeat("unpadded-payload", 8)+"x")), "=")
		in := []byte(`{"type":"image","data":"` + b64 + `","mimeType":"image/png"}` + "\n")

		out := DropImages(in)

		if bytes.Contains(out, []byte(b64)) {
			t.Fatal("an image with non-canonical base64 was not dropped")
		}
	})
}

func TestDropImages_IsIdempotent(t *testing.T) {
	t.Parallel()

	in := []byte(strings.Join([]string{
		`{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + fakeImage(magicPNG, "a") + `"}}`,
		`{"type":"image","source":{"type":"base64","media_type":"image/jpeg","data":"` + fakeImage(magicJPEG, "b") + `"}}`,
		`{"type":"image","data":"` + fakeImage(magicGIF, "c") + `","mimeType":"image/gif"}`,
		`{"image_url":"data:image/webp;base64,` + fakeImage(magicWEBP, "d") + `"}`,
	}, "\n") + "\n")

	once := DropImages(in)
	twice := DropImages(once)

	if !bytes.Equal(once, twice) {
		t.Errorf("dropping twice changed the transcript:\nonce:  %s\ntwice: %s", once, twice)
	}
}

func TestDropImages_SameInputGivesSameOutput(t *testing.T) {
	t.Parallel()

	// The redaction prefix cache reuses earlier output only while the input
	// prefix is byte-identical, so the drop must not vary between runs.
	in := []byte(`{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + fakeImage(magicPNG, "stable") + `"}}` + "\n")

	if !bytes.Equal(DropImages(in), DropImages(in)) {
		t.Error("two drops of the same transcript differ")
	}
}

func TestDropImages_LeavesTranscriptsWithoutImagesUntouched(t *testing.T) {
	t.Parallel()

	tests := map[string]string{
		"empty":           "",
		"text only":       `{"type":"user","message":{"content":"no images here"}}` + "\n",
		"non-json line":   "not json at all\n",
		"image file ref":  `{"type":"ImageView","path":"/w/shot.png"}` + "\n",
		"non-image file":  `{"type":"file","mime":"application/pdf","data":"` + fakeImage("%PDF", "pdf") + `"}` + "\n",
		"odd spacing":     " {\"type\" : \"user\", \"text\":\"the word image, <b> & a big number\", \"n\":12345678901234567890123}  \r\n",
		"pretty document": "{\n  \"info\": {\"id\": \"ses_1\"},\n  \"messages\": [\n    {\"parts\": [{\"type\": \"text\", \"text\": \"no image\"}]}\n  ]\n}\n",
	}
	for name, in := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if out := DropImages([]byte(in)); string(out) != in {
				t.Errorf("transcript changed:\nin:  %s\nout: %s", in, out)
			}
		})
	}
}

func TestDropPlaceholders_AreValidImages(t *testing.T) {
	t.Parallel()

	decode := func(t *testing.T, b64 string) []byte {
		t.Helper()
		raw, err := base64.StdEncoding.DecodeString(b64)
		if err != nil {
			t.Fatalf("placeholder is not valid base64: %v", err)
		}
		return raw
	}

	t.Run("png", func(t *testing.T) {
		t.Parallel()
		cfg, err := png.DecodeConfig(bytes.NewReader(decode(t, placeholderPNG)))
		if err != nil || cfg.Width != 1 || cfg.Height != 1 {
			t.Errorf("not a 1x1 PNG: %+v, %v", cfg, err)
		}
	})
	t.Run("jpeg", func(t *testing.T) {
		t.Parallel()
		cfg, err := jpeg.DecodeConfig(bytes.NewReader(decode(t, placeholderJPEG)))
		if err != nil || cfg.Width != 1 || cfg.Height != 1 {
			t.Errorf("not a 1x1 JPEG: %+v, %v", cfg, err)
		}
	})
	t.Run("gif", func(t *testing.T) {
		t.Parallel()
		cfg, err := gif.DecodeConfig(bytes.NewReader(decode(t, placeholderGIF)))
		if err != nil || cfg.Width != 1 || cfg.Height != 1 {
			t.Errorf("not a 1x1 GIF: %+v, %v", cfg, err)
		}
	})
	t.Run("webp", func(t *testing.T) {
		t.Parallel()
		// The standard library has no WebP decoder; check the container signature.
		if got := detectMediaType(decode(t, placeholderWEBP)); got != mediaTypeWEBP {
			t.Errorf("placeholder is detected as %q, want %q", got, mediaTypeWEBP)
		}
	})

	for name, b64 := range map[string]string{
		mediaTypePNG: placeholderPNG, mediaTypeJPEG: placeholderJPEG,
		mediaTypeGIF: placeholderGIF, mediaTypeWEBP: placeholderWEBP,
	} {
		if got := detectMediaType(decode(t, b64)); got != name {
			t.Errorf("placeholder for %s is detected as %q", name, got)
		}
	}
}

// decodeJSON parses one JSON value, keeping numbers as written.
func decodeJSON(t *testing.T, data []byte) any {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		t.Fatalf("not valid JSON: %v\n%s", err, data)
	}
	return v
}

// A value in an image slot is replaced there and nowhere else, so text that
// happens to repeat it is not rewritten.
func TestDropImages_ReplacesOnlyTheImageSlot(t *testing.T) {
	t.Parallel()

	tests := map[string]string{
		"base64 image":     fakeImage(magicPNG, "quoted"),
		"non-image string": "https://example.com/screenshots/" + strings.Repeat("a-long-path-segment/", 5) + "shot.png",
	}
	for name, value := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			before := `{"type":"user","message":{"content":"earlier I pasted ` + value + `"}}`
			image := `{"type":"image","data":"` + value + `","mimeType":"image/png"}`
			after := `{"type":"tool_result","output":"` + value + `"}`
			in := []byte(before + "\n" + image + "\n" + after + "\n")

			lines := bytes.Split(DropImages(in), []byte("\n"))

			if len(lines) != 4 {
				t.Fatalf("line count changed: %d lines", len(lines))
			}
			if string(lines[0]) != before || string(lines[2]) != after {
				t.Errorf("text outside the image slot was rewritten:\n%s\n%s", lines[0], lines[2])
			}
			if bytes.Contains(lines[1], []byte(value)) {
				t.Errorf("the image slot still holds its value: %s", lines[1])
			}
			if !bytes.Contains(lines[1], []byte(`"data":"`+placeholderPNG+`"`)) {
				t.Errorf("the image slot should hold the placeholder: %s", lines[1])
			}
		})
	}
}

func TestDropImages_DropsImagesOfAnyLength(t *testing.T) {
	t.Parallel()

	// A complete 1x1 GIF, shorter than the smallest value externalization takes.
	const smallGIF = "R0lGODlhAQABAAAAACwAAAAAAQABAAA="

	tests := []struct {
		name string
		in   string
		gone string
		want string
	}{
		{"small gif in a claude block", `{"type":"image","source":{"type":"base64","media_type":"image/gif","data":"` + smallGIF + `"}}`, smallGIF, placeholderGIF},
		{"small gif in a pi item", `{"type":"image","data":"` + smallGIF + `","mimeType":"image/gif"}`, smallGIF, placeholderGIF},
		{"small gif in a data uri", `{"image_url":"data:image/gif;base64,` + smallGIF + `"}`, smallGIF, placeholderGIF},
		{"four characters", `{"type":"image","data":"aGk=","mimeType":"image/png"}`, `"aGk="`, placeholderPNG},
		{"one character", `{"type":"image","data":"x","mimeType":"image/jpeg"}`, `"x"`, placeholderJPEG},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			out := DropImages([]byte(tt.in + "\n"))

			if bytes.Contains(out, []byte(tt.gone)) {
				t.Fatalf("the image is still in the transcript: %s", out)
			}
			if !bytes.Contains(out, []byte(tt.want)) {
				t.Errorf("wrong placeholder: %s", out)
			}
			requireValidJSONL(t, out)
		})
	}
}

// JSON lets a serializer escape any character, so the same image can be
// written many ways. Each one decodes to the same value and must be dropped.
func TestDropImages_DropsImagesHoweverTheyAreEscaped(t *testing.T) {
	t.Parallel()

	b64 := base64.StdEncoding.EncodeToString([]byte(magicPNG + strings.Repeat("\xff\xff\xfe", 30)))
	if strings.Count(b64, "/") < 2 {
		t.Fatal("fixture must contain two slashes")
	}
	unicodeEscaped := fmt.Sprintf(`\u%04x`, b64[0]) + b64[1:]
	mixedSlashes := strings.Replace(b64, "/", `\/`, 1)

	tests := map[string]string{
		"unicode escape in the data": `{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + unicodeEscaped + `"}}`,
		"mixed slash escapes":        `{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + mixedSlashes + `"}}`,
		"unicode escape in the type": `{"type":"\u0069mage","data":"` + b64 + `","mimeType":"image/png"}`,
		"unicode escape in the key":  `{"type":"image","d\u0061ta":"` + b64 + `","mimeType":"image/png"}`,
		"escaped data uri":           `{"image_url":"d\u0061ta:image\/png;base64,` + mixedSlashes + `"}`,
	}
	for name, line := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			out := DropImages([]byte(line + "\n"))

			canonical, err := json.Marshal(decodeJSON(t, out))
			if err != nil {
				t.Fatal(err)
			}
			if bytes.Contains(canonical, []byte(b64)) {
				t.Fatalf("the image is still in the transcript: %s", out)
			}
			if !bytes.Contains(canonical, []byte(placeholderPNG)) {
				t.Errorf("expected the 1x1 PNG placeholder: %s", out)
			}
		})
	}
}

// OpenCode stores one JSON document, pretty-printed over many lines by
// OpenCode 1 and compact after conversion from OpenCode 2, so no single line
// of it need be a complete JSON value.
func TestDropImages_DropsImagesInASingleJSONDocument(t *testing.T) {
	t.Parallel()

	b64 := fakeImage(magicPNG, "opencode")
	document := func(image string) string {
		return `{
  "info": {
    "id": "ses_1",
    "time": {"created": 1792558670933}
  },
  "messages": [
    {
      "info": {"id": "msg_1", "role": "user"},
      "parts": [
        {"type": "text", "text": "what is in this <screenshot> & why"},
        {
          "type": "file",
          "mime": "image/png",
          "filename": "shot.png",
          "url": "data:image/png;base64,` + image + `"
        },
        {"type": "file", "mime": "image/png", "name": "shot.png", "data": "` + image + `"}
      ]
    }
  ]
}
`
	}

	out := DropImages([]byte(document(b64)))

	if bytes.Contains(out, []byte(b64)) {
		t.Fatal("the image is still in the document")
	}
	if !reflect.DeepEqual(decodeJSON(t, out), decodeJSON(t, []byte(document(placeholderPNG)))) {
		t.Errorf("the document should differ only in its images:\n%s", out)
	}
	if !bytes.HasSuffix(out, []byte("}\n")) {
		t.Error("the trailing newline must be kept")
	}
	if !bytes.Equal(DropImages(out), out) {
		t.Error("dropping twice changed the document")
	}
}

// A line that held an image is written back as JSON, and must keep what a
// careless round trip loses.
func TestDropImages_RewrittenLineKeepsItsContent(t *testing.T) {
	t.Parallel()

	const bigNumber = "12345678901234567890123"
	line := `{"zeta":` + bigNumber + `,"text":"a <b> & c","type":"image","data":"` + fakeImage(magicPNG, "content") + `","mimeType":"image/png"}`

	out := DropImages([]byte(line + "\r\n"))

	for _, want := range []string{`"zeta":` + bigNumber, `"text":"a <b> & c"`, `"data":"` + placeholderPNG + `"`} {
		if !bytes.Contains(out, []byte(want)) {
			t.Errorf("the rewritten line lost %s: %s", want, out)
		}
	}
	if !bytes.HasSuffix(out, []byte("}\r\n")) {
		t.Errorf("the line ending must be kept: %q", out)
	}
}

// The redaction prefix cache redacts a transcript's earlier lines once and
// only the appended ones afterwards, so dropping the two parts separately
// must give the bytes that dropping the whole transcript gives.
func TestDropImages_AppendedLinesDoNotChangeEarlierOutput(t *testing.T) {
	t.Parallel()

	image := func(tag string) string {
		return `{"zeta":1,"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + fakeImage(magicPNG, tag) + `"},"alpha":2}` + "\n"
	}
	text := `{"type":"user","message":{"content":"hello"}}` + "\n"

	tests := map[string][2]string{
		"one earlier line":      {image("a"), text + image("b")},
		"several earlier lines": {text + image("a") + text, image("b") + text},
	}
	for name, parts := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			whole := DropImages([]byte(parts[0] + parts[1]))
			separate := append(DropImages([]byte(parts[0])), DropImages([]byte(parts[1]))...)

			if !bytes.Equal(whole, separate) {
				t.Errorf("dropping in two parts differs:\nwhole:    %s\nseparate: %s", whole, separate)
			}
		})
	}
}
