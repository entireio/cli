package imageextract

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"sort"
	"strings"
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
// require the value to be canonical base64: an image externalization would
// leave inline must still not be stored. The output depends only on the
// input, which the redaction prefix cache relies on, and dropping twice gives
// the same bytes as dropping once.
func DropImages(transcript []byte) []byte {
	replacements := map[string]string{}
	for _, line := range bytes.Split(transcript, []byte("\n")) {
		trimmed := bytes.TrimSpace(line)
		if len(trimmed) == 0 || (trimmed[0] != '{' && trimmed[0] != '[') {
			continue
		}
		var v any
		if err := json.Unmarshal(trimmed, &v); err != nil {
			continue
		}
		var hits []imgHit
		collectClaudeImages(v, &hits)
		collectPiImages(v, &hits)
		collectCodexImages(v, &hits)
		for _, h := range hits {
			// Shorter values are never real images, and replacing one everywhere
			// it occurs could rewrite unrelated text.
			if len(h.data) < minExternalizedBase64Len {
				continue
			}
			if placeholder := dropPlaceholder(h); placeholder != h.data {
				replacements[h.data] = placeholder
			}
		}
	}
	if len(replacements) == 0 {
		return transcript
	}

	// Longest first, so a value that contains another is replaced whole.
	values := make([]string, 0, len(replacements))
	for value := range replacements {
		values = append(values, value)
	}
	sort.Slice(values, func(i, j int) bool {
		if len(values[i]) != len(values[j]) {
			return len(values[i]) > len(values[j])
		}
		return values[i] < values[j]
	})

	out := transcript
	for _, value := range values {
		placeholder := []byte(replacements[value])
		for _, written := range jsonSpellings(value) {
			out = bytes.ReplaceAll(out, written, placeholder)
		}
	}
	return out
}

// dropPlaceholder picks the 1x1 image for a hit: the type its bytes show,
// else the type it declares, else PNG. Keeping the type the provider already
// accepted for this entry is what keeps the entry valid.
func dropPlaceholder(h imgHit) string {
	// 16 base64 characters decode to the 12 bytes the longest signature needs,
	// without decoding a multi-megabyte image or requiring valid padding.
	mediaType := ""
	if raw, err := base64.StdEncoding.DecodeString(h.data[:16]); err == nil {
		mediaType = detectMediaType(raw)
	}
	if mediaType == "" {
		mediaType = strings.ToLower(h.mediaType)
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

// jsonSpellings returns the ways a string value can appear inside raw JSON:
// as is, with Go's default escaping, without HTML escaping, and with the
// optional "\/" escape some serializers use for the slashes in base64.
func jsonSpellings(value string) [][]byte {
	spellings := [][]byte{[]byte(value)}
	add := func(b []byte) {
		for _, seen := range spellings {
			if bytes.Equal(seen, b) {
				return
			}
		}
		spellings = append(spellings, b)
	}

	if quoted, err := json.Marshal(value); err == nil {
		add(quoted[1 : len(quoted)-1])
	}
	var plain bytes.Buffer
	enc := json.NewEncoder(&plain)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(value); err == nil {
		quoted := bytes.TrimSuffix(plain.Bytes(), []byte("\n"))
		add(quoted[1 : len(quoted)-1])
	}
	add([]byte(strings.ReplaceAll(value, "/", `\/`)))
	return spellings
}
