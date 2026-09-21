package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"slices"
	"strings"
	"unicode"
)

// Pre-RFD-026 cells answer trail, review and discussion reads in camelCase;
// current cells answer in snake_case. A released CLI meets both — a cell
// deploy is not atomic with a CLI release, and an installed binary is not
// upgraded on the operator's schedule — so responses are reconciled here
// instead of duplicating every wire struct.
//
// Scope is deliberate: only the trail/review/discussion reads go through
// DecodeTrailJSON. Those destinations are snake_case-tagged throughout (the
// trail package's Author/Reviewer use single-word keys), so renaming is safe
// for them and only for them. The checkpoint and session APIs are still
// camelCase on both ends and keep using DecodeJSON untouched.
//
// Keys are renamed, never aliased. Keeping both spellings would leave the twin
// sharing its child's pointer, so json.Marshal would serialize each subtree
// once per alias and the encoded document would double per level of camelCase
// nesting. TestNestedCamelCaseDoesNotInflate pins that.
//
// Delete this file once the oldest supported cell speaks RFD-026.

// wireRenames covers the fields whose NAME changed, not just their case;
// snakeWireKey handles everything else mechanically.
//
// The review-comment page is deliberately absent. hasMore/nextOffset ->
// next_cursor is an offset-to-cursor model change, not a rename: the two carry
// different meanings and drive different request parameters, so the paging
// loop has to branch on which one it got. Aliasing them here would hand that
// loop a cursor built from an offset.
var wireRenames = map[string]string{
	"nextPageToken":      "next_cursor",
	"repositoryId":       "repo_id",
	"thread":             "discussion",
	"threadId":           "discussion_id",
	"threadMessageCount": "discussion_message_count",
}

// compatWalkDepthLimit bounds the walk. Trail payloads nest a handful deep;
// anything past this is either hostile or not ours, and is passed through.
const compatWalkDepthLimit = 32

// DecodeTrailJSON is DecodeJSON for the trail/review/discussion routes: it
// reconciles a pre-RFD-026 cell's camelCase keys before decoding. The caller
// is responsible for closing resp.Body.
func DecodeTrailJSON(resp *http.Response, dest any) error {
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return fmt.Errorf("read response body: %w", err)
	}
	if err := json.Unmarshal(reconcileWireKeys(body), dest); err != nil {
		return fmt.Errorf("decode JSON response: %w", err)
	}
	return nil
}

// UnmarshalTrailWire is DecodeTrailJSON for callers holding bytes rather than
// a response, such as the SSE frames in `trail watch`.
func UnmarshalTrailWire(raw []byte, dest any) error {
	if err := json.Unmarshal(reconcileWireKeys(raw), dest); err != nil {
		return fmt.Errorf("decode JSON: %w", err)
	}
	return nil
}

// reconcileWireKeys returns raw with every camelCase key renamed to its
// snake_case spelling. On malformed input it returns raw unchanged and lets
// the caller's own decode report the error, so this never converts a decode
// failure into a different one.
func reconcileWireKeys(raw []byte) []byte {
	if !hasCamelCaseKey(raw) {
		return raw
	}
	// UseNumber keeps every number as its original literal. Decoding into
	// `any` would otherwise route them through float64, and re-encoding a
	// value past 2^53 (or one written as 1e3) would not round-trip.
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var doc any
	if err := dec.Decode(&doc); err != nil {
		return raw
	}
	out, err := json.Marshal(renameCompatKeys(doc, 0))
	if err != nil {
		return raw
	}
	return out
}

// hasCamelCaseKey reports whether raw contains an object KEY with a
// lower-to-upper transition. A current cell answers entirely in snake_case, so
// this keeps the common path at one linear scan with no decode and no
// re-encode.
//
// It must check keys rather than the whole document: RFC 3339 timestamps
// ("2026-09-21T00:00:00Z") carry a digit-to-upper transition, and every trail
// response has one, so a document-wide scan would never take the fast path.
func hasCamelCaseKey(raw []byte) bool {
	for i := 0; i < len(raw); i++ {
		if raw[i] != '"' {
			continue
		}
		start := i + 1
		camel := false
		for i++; i < len(raw) && raw[i] != '"'; i++ {
			if raw[i] == '\\' {
				i++ // skip the escaped byte; the loop's i++ moves past it
				continue
			}
			if i > start && raw[i] >= 'A' && raw[i] <= 'Z' {
				if prev := raw[i-1]; (prev >= 'a' && prev <= 'z') || (prev >= '0' && prev <= '9') {
					camel = true
				}
			}
		}
		if !camel {
			continue
		}
		// A string is a key only when the next non-space byte is a colon.
		for j := i + 1; j < len(raw); j++ {
			if c := raw[j]; c == ' ' || c == '\t' || c == '\n' || c == '\r' {
				continue
			} else if c == ':' {
				return true
			}
			break
		}
	}
	return false
}

// renameCompatKeys walks a decoded document renaming each camelCase key to its
// snake_case spelling. Keys are snapshotted before the walk: mutating a map
// while ranging over it leaves the new entries' visitation undefined. The
// result is order-independent — a document carrying both spellings keeps the
// current one either way.
func renameCompatKeys(v any, depth int) any {
	if depth > compatWalkDepthLimit {
		return v
	}
	switch node := v.(type) {
	case map[string]any:
		for _, key := range slices.Sorted(maps.Keys(node)) {
			value := renameCompatKeys(node[key], depth+1)
			twin, renamed := wireRenames[key]
			if !renamed {
				twin = snakeWireKey(key)
			}
			if twin == key {
				node[key] = value
				continue
			}
			delete(node, key)
			// Never clobber: a document carrying both spellings already
			// states which value belongs to the current name.
			if _, exists := node[twin]; !exists {
				node[twin] = value
			}
		}
		return node
	case []any:
		for i := range node {
			node[i] = renameCompatKeys(node[i], depth+1)
		}
		return node
	default:
		return v
	}
}

// snakeWireKey lowercases a camelCase wire key, preserving acronym runs
// (ghPrId -> gh_pr_id, headSha -> head_sha). It mirrors entire-api's own
// eventSnakeCase so both ends agree on the spelling of a given field.
func snakeWireKey(key string) string {
	if !strings.ContainsFunc(key, unicode.IsUpper) {
		return key
	}
	runes := []rune(key)
	var out strings.Builder
	out.Grow(len(key) + 4)
	for i, r := range runes {
		if unicode.IsUpper(r) {
			prevSplits := i > 0 && (unicode.IsLower(runes[i-1]) || unicode.IsDigit(runes[i-1]))
			acronymEnds := i > 0 && unicode.IsUpper(runes[i-1]) && i+1 < len(runes) && unicode.IsLower(runes[i+1])
			if prevSplits || acronymEnds {
				out.WriteByte('_')
			}
			r = unicode.ToLower(r)
		}
		out.WriteRune(r)
	}
	return out.String()
}
