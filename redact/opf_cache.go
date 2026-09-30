package redact

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// OPFSpanCache persists OPF results per blob, so the model call can run in a
// background scan and a later rewrite can apply the result without it.
//
// An entry maps the hash of each prose leaf in one blob to the spans OPF found
// in it. It holds no text: a leaf is identified only by its SHA-256, and a span
// only by offsets and a label. Implementations must be safe to call from one
// process at a time; a missing or unreadable entry reports ok=false.
type OPFSpanCache interface {
	LoadOPFSpans(key string) (spans map[string][]Span, ok bool)
	StoreOPFSpans(key string, spans map[string][]Span) error
}

// ErrOPFScanPending reports that at least one blob has not been scanned yet,
// so it cannot be redacted without a model call.
var ErrOPFScanPending = errors.New("OpenAI Privacy Filter has not finished scanning this content")

// ErrOPFUnavailable reports that the OPF runtime failed earlier in this
// process, so no further scan will be attempted.
var ErrOPFUnavailable = errors.New("OpenAI Privacy Filter is unavailable for the rest of this process")

// opfCacheVersion is part of every cache key. Bump it when the entry format or
// the meaning of a stored span changes, so older entries are never read.
const opfCacheVersion = "1"

// opfBlobCacheKey names one blob's entry. OPF's output depends only on the leaf
// text and the categories it was asked for, so the key is the blob's object
// hash plus the sorted category set; enabling a category rescans everything.
func opfBlobCacheKey(blobID string, cats []string) string {
	sum := sha256.Sum256([]byte(opfCacheVersion + "\x00" + strings.Join(cats, ",") + "\x00" + blobID))
	return hex.EncodeToString(sum[:])
}

func opfLeafKey(leaf string) string {
	sum := sha256.Sum256([]byte(leaf))
	return hex.EncodeToString(sum[:])
}

// ScanBlobsWithPrivacyFilter runs OPF over every blob of inputs that has no
// cache entry yet and stores one entry per such blob. Blobs already cached cost
// one cache read and no model time, so rescanning a backlog is cheap.
//
// Fail-closed like BatchBytesWithPrivacyFilter: zero enabled categories returns
// ErrOPFNoEnabledCategories and a runtime failure returns an error with nothing
// stored for the blobs of the failed pass. With OPF disabled there is nothing to
// scan and it returns nil.
func ScanBlobsWithPrivacyFilter(ctx context.Context, inputs []NamedBlob, cache OPFSpanCache) error {
	cfg := getOPFConfig()
	if cfg == nil || !cfg.Enabled {
		return nil
	}
	cats := enabledCategories(cfg)
	if len(cats) == 0 {
		return ErrOPFNoEnabledCategories
	}
	if cfg.runtime == nil {
		return errOPFNilRuntime
	}
	if opfBreakerTripped.Load() {
		return ErrOPFUnavailable
	}

	var pending []NamedBlob
	seen := make(map[string]struct{})
	for _, in := range inputs {
		if in.ID == "" {
			continue
		}
		if _, dup := seen[in.ID]; dup {
			continue
		}
		seen[in.ID] = struct{}{}
		if _, ok := cache.LoadOPFSpans(opfBlobCacheKey(in.ID, cats)); !ok {
			pending = append(pending, in)
		}
	}
	if len(pending) == 0 {
		return nil
	}

	spansByLeaf, err := scanProseLeaves(ctx, cfg, cats, uniqueProseLeaves(pending), len(pending))
	if err != nil {
		return err
	}
	for _, in := range pending {
		entry := make(map[string][]Span)
		collectLeaves(in, func(v string) {
			if isProseLeaf(v) {
				entry[opfLeafKey(v)] = spansByLeaf[v]
			}
		})
		if err := cache.StoreOPFSpans(opfBlobCacheKey(in.ID, cats), entry); err != nil {
			return fmt.Errorf("store OPF span cache entry: %w", err)
		}
	}
	return nil
}

// ApplyCachedPrivacyFilter redacts inputs using cached OPF results only; it
// never calls the model. Every blob must have an ID and a complete cache entry,
// otherwise it returns ErrOPFScanPending and no output, because the caller is
// about to stamp Entire-OPF-Applied and partial coverage would make that false.
//
// With OPF disabled it returns regex-only output, and with zero enabled
// categories ErrOPFNoEnabledCategories, matching BatchBytesWithPrivacyFilter. A
// tripped breaker does not matter here: the results were produced earlier.
func ApplyCachedPrivacyFilter(inputs []NamedBlob, cache OPFSpanCache) ([][]byte, error) {
	if len(inputs) == 0 {
		return nil, nil
	}
	cfg := getOPFConfig()
	if cfg == nil || !cfg.Enabled {
		return applyRegexLayersToBlobs(inputs), nil
	}
	cats := enabledCategories(cfg)
	if len(cats) == 0 {
		return nil, ErrOPFNoEnabledCategories
	}

	spansByLeaf := make(map[string][]Span)
	for _, in := range inputs {
		if in.ID == "" {
			return nil, ErrOPFScanPending
		}
		entry, ok := cache.LoadOPFSpans(opfBlobCacheKey(in.ID, cats))
		if !ok {
			return nil, ErrOPFScanPending
		}
		complete := true
		collectLeaves(in, func(v string) {
			if !isProseLeaf(v) {
				return
			}
			spans, found := entry[opfLeafKey(v)]
			if !found {
				complete = false
				return
			}
			spansByLeaf[v] = spans
		})
		if !complete {
			return nil, ErrOPFScanPending
		}
	}

	out := make([][]byte, len(inputs))
	for i, in := range inputs {
		out[i] = applyToBlob(in, spansByLeaf, cfg)
	}
	return out, nil
}
