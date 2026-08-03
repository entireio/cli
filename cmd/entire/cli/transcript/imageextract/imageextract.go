// Package imageextract externalizes inline base64 images from an agent's session
// transcript into a checkpoint asset store, replacing each with a compact,
// path-bearing placeholder, and re-injects them byte-exactly on restore.
//
// The transform is per-agent because transcript formats differ: only agents that
// inline base64 images (Claude Code and Codex today) register a codec; every
// other agent resolves to nil and its transcript flows through untouched (a
// graceful no-op). All codecs share one extraction engine and reinjection routine
// and differ only in how they *find* images (their collector).
//
// Correctness contract: for any transcript x from a supported agent,
// ReinjectImages(ExtractImages(x)) == x, byte-for-byte. This is achieved by only
// ever swapping the base64 image *value* in place (never re-marshalling the JSON)
// and by refusing to externalize any image whose raw bytes don't re-encode to the
// exact original base64 string.
package imageextract

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
)

// Asset is one externalized image. It reuses the canonical agent asset model;
// Name is the stable asset filename (also the id used in the placeholder).
type Asset = agent.CompactedTranscriptAsset

// ImageCodec extracts/reinjects inline images for one agent's transcript format.
type ImageCodec interface {
	// ExtractImages lifts inline base64 images out, returning the rewritten
	// (placeholder-bearing) transcript plus the extracted assets. A transcript
	// with no externalizable images is returned unchanged with nil assets.
	ExtractImages(transcript []byte) (rewritten []byte, assets []Asset, err error)
	// ReinjectImages restores the original transcript by looking each placeholder's
	// asset up by Name. Placeholders whose asset is missing are left in place.
	ReinjectImages(transcript []byte, lookup func(name string) (Asset, bool)) ([]byte, error)
}

// placeholderPrefix leads every externalized-image reference. It is deliberately
// low-entropy so the (later) redaction pass never flags it, and it carries the
// asset's path so an agent summarizing the stored log still understands an image
// was here and where it lives.
const placeholderPrefix = "entire-asset:assets/"

// placeholderRe matches a full placeholder and captures the asset name.
var placeholderRe = regexp.MustCompile(`entire-asset:assets/(img-[0-9a-f]+\.[a-z0-9]+)`)

// newAssetID returns a random hex id (16 bytes → 32 hex chars). Hex is ~4
// bits/char, below the redaction entropy threshold, so placeholders survive
// redaction. The rand error is surfaced (never swallowed) so a broken entropy
// source can't silently yield an all-zero, collision-prone id. Injectable for
// deterministic tests.
var newAssetID = func() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate asset id: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// uniqueImageName returns an asset name not already in used, recording it. It
// guarantees the "distinct image data -> distinct asset name" invariant the
// byte-exact round trip relies on: two assets sharing a name would make
// ReinjectImages restore both placeholders to whichever the lookup returns first.
// With crypto/rand collisions never happen; the hex-counter suffix guarantees
// termination even if a caller injects a degenerate id source.
func uniqueImageName(used map[string]bool, mediaType string) (string, error) {
	ext := extForMedia(mediaType)
	id, err := newAssetID()
	if err != nil {
		return "", err
	}
	name := "img-" + id + "." + ext
	for suffix := 0; used[name]; suffix++ {
		name = "img-" + id + strconv.FormatInt(int64(suffix), 16) + "." + ext
	}
	used[name] = true
	return name, nil
}

// minExternalizedBase64Len is the shortest base64 image value we externalize.
// It must exceed the 32-char random-hex run in a placeholder so that an
// externalized value can never be a substring of any placeholder — that is the
// single condition under which the in-place value swap could corrupt a
// placeholder and break the byte-exact round trip. As a bonus it leaves tiny
// blobs (which are never real images) inline, where they cost nothing.
const minExternalizedBase64Len = 64

// maxExternalizedImageBytes is the largest decoded image we externalize. Above
// it the image stays inline: writeAssets stores each asset as a single git blob,
// so one over agent.MaxChunkSize would become an unpushable object — the same
// guard the Cursor sidecar path applies. The transcript itself is chunked under
// MaxChunkSize, so an oversized image left inline still pushes fine. It is a var
// (not a const) only so tests can lower it without allocating a 50MB fixture.
var maxExternalizedImageBytes = agent.MaxChunkSize

var codecs = map[types.AgentType]ImageCodec{
	agent.AgentTypeClaudeCode: claudeCodec{},
	agent.AgentTypeCodex:      codexCodec{},
}

// CodecFor returns the image codec for an agent type, or nil if the agent's
// transcript is not known to inline images (a no-op).
func CodecFor(t types.AgentType) ImageCodec { return codecs[t] }

// HasPlaceholders reports whether a transcript carries any externalized-image
// placeholders — so restore knows to reinject regardless of the current config.
func HasPlaceholders(transcript []byte) bool {
	return bytes.Contains(transcript, []byte(placeholderPrefix))
}

// imgHit is one image found by a collector: the bare base64 value to swap out and
// its declared media type (used only to pick the asset filename extension).
type imgHit struct{ data, mediaType string }

// extractImagesWith is the shared extraction engine. It parses each JSONL line,
// gathers image hits via the per-agent collector, dedupes, decodes and re-encodes
// to confirm the base64 is byte-exactly reversible, then swaps each value out for
// a placeholder — longest value first (so a value that is a substring of another
// can't orphan it) and only recording assets whose swap actually replaced bytes.
func extractImagesWith(transcript []byte, collect func(v any, out *[]imgHit)) ([]byte, []Asset, error) {
	if len(transcript) == 0 {
		return transcript, nil, nil
	}

	// Map each unique base64 image value → its asset (dedupes repeats within the
	// transcript; git dedupes identical blobs across checkpoints by content).
	seen := map[string]Asset{}
	var order []string             // unique base64 values, later sorted longest-first
	usedNames := map[string]bool{} // guards distinct-data -> distinct-name

	for _, line := range bytes.Split(transcript, []byte("\n")) {
		trimmed := bytes.TrimSpace(line)
		// Accept object- and array-rooted JSON lines; the collectors walk both.
		if len(trimmed) == 0 || (trimmed[0] != '{' && trimmed[0] != '[') {
			continue
		}
		var v any
		if err := json.Unmarshal(trimmed, &v); err != nil {
			continue // non-JSON line; leave untouched
		}
		var hits []imgHit
		collect(v, &hits)
		for _, h := range hits {
			if len(h.data) < minExternalizedBase64Len {
				continue // too small to be a real image; also keeps it out of placeholders
			}
			if _, ok := seen[h.data]; ok {
				continue
			}
			raw, err := base64.StdEncoding.DecodeString(h.data)
			if err != nil {
				continue // not standard base64; leave inline
			}
			// Only externalize if re-encoding reproduces the exact original string;
			// otherwise the restore round-trip could not be byte-exact.
			if base64.StdEncoding.EncodeToString(raw) != h.data {
				continue
			}
			// Leave oversized images inline. Each asset is stored as one git blob,
			// and a blob over MaxChunkSize would be unpushable; the transcript, by
			// contrast, is chunked under that limit, so keeping the image inline
			// stays pushable (mirrors the Cursor sidecar guard).
			if len(raw) > maxExternalizedImageBytes {
				continue
			}
			// Prefer the media type detected from the actual bytes over the declared
			// one: agents mislabel it (real Codex data-URIs declare image/jpeg for
			// PNG bytes), and the asset filename/manifest should reflect the content.
			// This is metadata only — the transcript's declared type is untouched, so
			// the round trip stays byte-exact.
			mediaType := detectMediaType(raw)
			if mediaType == "" {
				mediaType = h.mediaType
			}
			name, err := uniqueImageName(usedNames, mediaType)
			if err != nil {
				return transcript, nil, err
			}
			seen[h.data] = Asset{Name: name, MediaType: mediaType, Data: raw}
			order = append(order, h.data)
		}
	}

	if len(order) == 0 {
		return transcript, nil, nil
	}

	// Replace longest values first so that if one image's base64 is a substring of
	// another's, the containing (longer) value is swapped out before the shorter
	// one, keeping every asset's placeholder present. Ties broken by value for
	// determinism.
	sort.SliceStable(order, func(i, j int) bool {
		if len(order[i]) != len(order[j]) {
			return len(order[i]) > len(order[j])
		}
		return order[i] < order[j]
	})

	rewritten := transcript
	assets := make([]Asset, 0, len(order))
	for _, data := range order {
		a := seen[data]
		// Swap the base64 value itself, not a field wrapper. Agents serialize the
		// enclosing JSON differently and across versions (compact vs spaced, string
		// vs object image_url, data-URI vs bare), so the bare value is the only
		// format-agnostic anchor. This can also swap a copy of the same base64 in a
		// text field, but the round trip stays byte-exact because ReinjectImages
		// restores every occurrence to the identical value.
		swapped := bytes.ReplaceAll(rewritten, []byte(data), []byte(placeholderPrefix+a.Name))
		if bytes.Equal(swapped, rewritten) {
			// Exact bytes weren't present (e.g. JSON-escaped in the raw transcript);
			// leave the image inline rather than record an asset no placeholder
			// references.
			continue
		}
		rewritten = swapped
		assets = append(assets, a)
	}
	if len(assets) == 0 {
		return transcript, nil, nil
	}
	return rewritten, assets, nil
}

// reinjectImages restores every placeholder to its asset's base64. It is shared
// by all codecs: the placeholder token is agent-independent, so restore only
// needs the asset lookup.
func reinjectImages(transcript []byte, lookup func(name string) (Asset, bool)) ([]byte, error) {
	if !HasPlaceholders(transcript) {
		return transcript, nil
	}
	result := transcript
	done := map[string]bool{}
	for _, m := range placeholderRe.FindAllSubmatch(transcript, -1) {
		full, name := m[0], string(m[1])
		if done[name] {
			continue
		}
		done[name] = true
		a, ok := lookup(name)
		if !ok {
			continue // asset unavailable; leave the placeholder (best-effort)
		}
		result = bytes.ReplaceAll(result, full, []byte(base64.StdEncoding.EncodeToString(a.Data)))
	}
	return result, nil
}

// claudeCodec handles Claude Code (and, structurally, Cursor) JSONL transcripts,
// which embed images as {"type":"image","source":{"type":"base64","media_type":…,"data":…}}.
type claudeCodec struct{}

func (claudeCodec) ExtractImages(transcript []byte) ([]byte, []Asset, error) {
	return extractImagesWith(transcript, collectClaudeImages)
}

func (claudeCodec) ReinjectImages(transcript []byte, lookup func(name string) (Asset, bool)) ([]byte, error) {
	return reinjectImages(transcript, lookup)
}

// codexCodec handles OpenAI Codex rollout JSONL transcripts, which embed images
// as base64 data-URIs (data:image/<type>;base64,<data>) — in input_image
// image_url values, user messages, and function_call_output content alike.
type codexCodec struct{}

func (codexCodec) ExtractImages(transcript []byte) ([]byte, []Asset, error) {
	return extractImagesWith(transcript, collectCodexImages)
}

func (codexCodec) ReinjectImages(transcript []byte, lookup func(name string) (Asset, bool)) ([]byte, error) {
	return reinjectImages(transcript, lookup)
}

// collectClaudeImages walks any decoded JSON value and gathers every inline
// base64 image block, at any nesting depth (top-level content, tool_result
// content, etc.).
func collectClaudeImages(v any, out *[]imgHit) {
	switch t := v.(type) {
	case map[string]any:
		if t["type"] == "image" {
			if src, ok := t["source"].(map[string]any); ok && src["type"] == "base64" {
				if data, ok := src["data"].(string); ok && data != "" {
					var mediaType string
					if mt, mok := src["media_type"].(string); mok {
						mediaType = mt
					}
					*out = append(*out, imgHit{data: data, mediaType: mediaType})
				}
			}
		}
		for _, vv := range t {
			collectClaudeImages(vv, out)
		}
	case []any:
		for _, vv := range t {
			collectClaudeImages(vv, out)
		}
	}
}

// codexDataURIRe matches an image data-URI and captures (media subtype, base64).
// Codex serializes every inline image this way — input_image image_url values,
// user-message content, and function_call_output payloads — so keying on the
// data-URI (rather than a specific field) covers input and generated/tool images
// uniformly. The captured group is the bare base64, which is what gets swapped.
var codexDataURIRe = regexp.MustCompile(`data:image/([a-zA-Z0-9.+-]+);base64,([A-Za-z0-9+/]+={0,2})`)

// collectCodexImages walks any decoded JSON value and, for each string leaf,
// gathers every image data-URI it contains (a leaf may be the URI itself, e.g.
// input_image.image_url, or embed one inside larger tool output).
func collectCodexImages(v any, out *[]imgHit) {
	switch t := v.(type) {
	case map[string]any:
		for _, vv := range t {
			collectCodexImages(vv, out)
		}
	case []any:
		for _, vv := range t {
			collectCodexImages(vv, out)
		}
	case string:
		for _, m := range codexDataURIRe.FindAllStringSubmatch(t, -1) {
			*out = append(*out, imgHit{data: m[2], mediaType: "image/" + m[1]})
		}
	}
}

const (
	mediaTypePNG  = "image/png"
	mediaTypeJPEG = "image/jpeg"
	mediaTypeGIF  = "image/gif"
	mediaTypeWEBP = "image/webp"
)

// detectMediaType returns the image media type implied by the leading magic
// bytes, or "" if unrecognized (caller falls back to the declared type).
func detectMediaType(raw []byte) string {
	switch {
	case bytes.HasPrefix(raw, []byte("\x89PNG\r\n\x1a\n")):
		return mediaTypePNG
	case bytes.HasPrefix(raw, []byte{0xFF, 0xD8, 0xFF}):
		return mediaTypeJPEG
	case bytes.HasPrefix(raw, []byte("GIF87a")), bytes.HasPrefix(raw, []byte("GIF89a")):
		return mediaTypeGIF
	case len(raw) >= 12 && bytes.HasPrefix(raw, []byte("RIFF")) && bytes.Equal(raw[8:12], []byte("WEBP")):
		return mediaTypeWEBP
	default:
		return ""
	}
}

func extForMedia(mediaType string) string {
	switch mediaType {
	case mediaTypePNG:
		return "png"
	case mediaTypeJPEG:
		return "jpg"
	case mediaTypeGIF:
		return "gif"
	case mediaTypeWEBP:
		return "webp"
	default:
		return "bin"
	}
}
