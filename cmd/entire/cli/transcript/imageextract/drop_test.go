package imageextract

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"image/gif"
	"image/jpeg"
	"image/png"
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
		"empty":          "",
		"text only":      `{"type":"user","message":{"content":"no images here"}}` + "\n",
		"non-json line":  "not json at all\n",
		"tiny image":     `{"type":"image","source":{"type":"base64","media_type":"image/png","data":"aGk="}}` + "\n",
		"image file ref": `{"type":"ImageView","path":"/w/shot.png"}` + "\n",
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
