package imageextract

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"unicode"
)

// The 1x1 images a dropped image is replaced with, as standard base64. Each
// must stay a valid image of its type: a restored transcript is replayed to
// the model provider, which rejects an image entry it cannot decode.
const (
	placeholderPNG  = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg=="
	placeholderGIF  = "R0lGODlhAQABAIAAAAAAAP///yH5BAEAAAAALAAAAAABAAEAAAIBRAA7"
	placeholderWEBP = "UklGRhoAAABXRUJQVlA4TA0AAAAvAAAAEAcQERGIiP4HAA=="
	placeholderJPEG = "/9j/2wCEAP////////////////////////////////////////////////////////////////////////////////////8B///////////////////////////////////////////////////////////////////////////////////////AAAsIAAEAAQEBEQD/xADSAAABBQEBAQEBAQAAAAAAAAAAAQIDBAUGBwgJCgsQAAIBAwMCBAMFBQQEAAABfQECAwAEEQUSITFBBhNRYQcicRQygZGhCCNCscEVUtHwJDNicoIJChYXGBkaJSYnKCkqNDU2Nzg5OkNERUZHSElKU1RVVldYWVpjZGVmZ2hpanN0dXZ3eHl6g4SFhoeIiYqSk5SVlpeYmZqio6Slpqeoqaqys7S1tre4ubrCw8TFxsfIycrS09TV1tfY2drh4uPk5ebn6Onq8fLz9PX29/j5+v/aAAgBAQAAPwCOv//Z"
)

// DropImages replaces every inline image in a transcript with a 1x1 image of
// the same type, so the stored copy holds no image content. A transcript with
// no images is returned unchanged.
//
// It looks for every image shape any codec knows, whatever agent wrote the
// transcript, and unlike ExtractImages it has no size limit and does not
// require the value to be base64: an image externalization would leave inline
// must still not be stored.
//
// An image is replaced in the decoded JSON, in the slot that holds it, and
// the JSON that held it is written back. Searching the raw bytes for the
// value instead would miss an image a serializer escaped differently and
// would rewrite unrelated text that repeats it. A line without an image keeps
// its bytes, a line with one comes back compact with sorted keys.
//
// Each line's output depends only on that line, which the redaction prefix
// cache relies on, and dropping twice gives the same bytes as dropping once.
func DropImages(transcript []byte) []byte {
	if !mayHoldImage(transcript) {
		return transcript
	}
	// One JSON value spread over many lines, as OpenCode stores it, has no
	// line that parses on its own.
	if out, isJSON := dropImagesInJSON(transcript); isJSON {
		return out
	}

	lines := bytes.Split(transcript, []byte("\n"))
	changed := false
	for i, line := range lines {
		if !mayHoldImage(line) {
			continue
		}
		if out, isJSON := dropImagesInJSON(line); isJSON && !bytes.Equal(out, line) {
			lines[i] = out
			changed = true
		}
	}
	if !changed {
		return transcript
	}
	return bytes.Join(lines, []byte("\n"))
}

// dropImagesInJSON drops the images in content when it is exactly one JSON
// value, keeping the whitespace around it. isJSON is false when it is not,
// and content is returned as is when it holds no image. If the value cannot
// be written back, only that whitespace is returned.
func dropImagesInJSON(content []byte) (out []byte, isJSON bool) {
	trimmed := bytes.TrimSpace(content)
	if len(trimmed) == 0 {
		return content, false
	}
	// Numbers stay as written, so an integer beyond float64 precision
	// survives the round trip.
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	dec.UseNumber()
	var value any
	if err := dec.Decode(&value); err != nil {
		return content, false
	}
	var rest json.RawMessage
	if err := dec.Decode(&rest); !errors.Is(err, io.EOF) {
		return content, false
	}
	dropped, changed := dropImagesInValue(value)
	if !changed {
		return content, true
	}

	lead := content[:len(content)-len(bytes.TrimLeftFunc(content, unicode.IsSpace))]
	trail := content[len(bytes.TrimRightFunc(content, unicode.IsSpace)):]
	out = append(out, lead...)

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(dropped); err != nil {
		// Content known to hold an image is not stored as it was: its line is
		// left blank, which JSONL readers skip.
		return append(out, trail...), true
	}
	out = append(out, bytes.TrimSuffix(buf.Bytes(), []byte("\n"))...)
	return append(out, trail...), true
}

// mayHoldImage reports whether raw JSON can contain an image. Every shape
// dropImagesInValue matches spells "image" in a type, a media type or a data
// URI, and the only way JSON can write those letters differently is a \u00XX
// escape. Skipping everything else spares decoding most of a large transcript.
func mayHoldImage(raw []byte) bool {
	return bytes.Contains(raw, []byte(blockTypeImage)) || bytes.Contains(raw, []byte(`\u00`))
}

// dropImagesInValue replaces every image in a decoded JSON value, in place,
// and returns the value to store in its stead.
func dropImagesInValue(v any) (any, bool) {
	switch t := v.(type) {
	case map[string]any:
		changed := dropImageSlots(t)
		for key, child := range t {
			if dropped, ok := dropImagesInValue(child); ok {
				t[key] = dropped
				changed = true
			}
		}
		return t, changed
	case []any:
		changed := false
		for i, child := range t {
			if dropped, ok := dropImagesInValue(child); ok {
				t[i] = dropped
				changed = true
			}
		}
		return t, changed
	case string:
		// Codex and OpenCode 1 write an image as a data URI, alone or inside
		// a longer string.
		dropped := codexDataURIRe.ReplaceAllStringFunc(t, func(uri string) string {
			m := codexDataURIRe.FindStringSubmatch(uri)
			return strings.TrimSuffix(uri, m[2]) + dropPlaceholder(m[2], "image/"+m[1])
		})
		return dropped, dropped != t
	default:
		return v, false
	}
}

// dropImageSlots replaces the image an object holds directly: a Claude Code
// image block, a Pi image item, the file part OpenCode 2 attaches an image
// as, or an image Codex generated.
func dropImageSlots(obj map[string]any) bool {
	switch obj["type"] {
	case blockTypeImage:
		changed := false
		if source, ok := obj["source"].(map[string]any); ok && source["type"] == "base64" {
			changed = dropSlot(source, "data", "media_type")
		}
		return dropSlot(obj, "data", "mimeType") || changed
	case "file":
		if mime, ok := obj["mime"].(string); ok && strings.HasPrefix(mime, "image/") {
			return dropSlot(obj, "data", "mime")
		}
	case "image_generation_call":
		// Codex stores an image it generated as bare base64 with no declared
		// type; redaction skips this object because its type starts with "image".
		return dropSlot(obj, "result", "")
	}
	return false
}

// dropSlot replaces obj's dataKey string with the placeholder for it, reading
// the declared media type from typeKey when there is one.
func dropSlot(obj map[string]any, dataKey, typeKey string) bool {
	data, ok := obj[dataKey].(string)
	if !ok || data == "" {
		return false
	}
	declared := ""
	if mediaType, ok := obj[typeKey].(string); ok {
		declared = mediaType
	}
	placeholder := dropPlaceholder(data, declared)
	if placeholder == data {
		return false
	}
	obj[dataKey] = placeholder
	return true
}

// dropPlaceholder picks the 1x1 image for an image value: the type its bytes
// show, else the type it declares, else PNG. Keeping the type the provider
// already accepted for this entry is what keeps the entry valid.
func dropPlaceholder(data, declared string) string {
	// 16 base64 characters decode to the 12 bytes the longest signature needs,
	// without decoding a multi-megabyte image or requiring valid padding.
	// A shorter value is cut to whole 4-character groups.
	mediaType := ""
	if raw, err := base64.StdEncoding.DecodeString(data[:min(len(data), 16)&^3]); err == nil {
		mediaType = detectMediaType(raw)
	}
	if mediaType == "" {
		mediaType = strings.ToLower(declared)
	}
	switch mediaType {
	case mediaTypeJPEG, "image/jpg":
		return placeholderJPEG
	case mediaTypeGIF:
		return placeholderGIF
	case mediaTypeWEBP:
		return placeholderWEBP
	default:
		return placeholderPNG
	}
}
