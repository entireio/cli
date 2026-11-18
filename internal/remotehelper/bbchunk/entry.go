package bbchunk

import (
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/jmwoliver/xet-go/xethash"
	"github.com/jmwoliver/xet-go/xorb"
)

// Pack-entry constants of a client chunked entry. They copy entiredb's
// internal/go-git/plumbing/format/packfile/external.go and the client-chunked
// push spec.
const (
	// ExternalObject is the pack entry type of an external entry.
	ExternalObject = 5
	// MetaClientChunked is the meta byte of a chunked entry that a client
	// sends. The server converts it to its stored kind.
	MetaClientChunked byte = 0x03
	// PackVersionExternal is the version of a pack that holds at least one
	// type-5 entry.
	PackVersionExternal uint32 = 0xe002
	// MaxPayloadSize is the largest payload of an entry: the blob OID and
	// the recipe.
	MaxPayloadSize = 1 << 20
	// MaxObjectSize is the largest logical size of a chunked blob.
	MaxObjectSize = 1 << 30
)

// Term is one run of chunks of a recipe: chunks [ChunkStart, ChunkEnd) of the
// xorb Recipe.Xorbs[Xorb].
type Term struct {
	Xorb          int
	ChunkStart    int
	ChunkEnd      int
	UnpackedBytes uint64
}

// Recipe lists the terms that rebuild one blob in order.
type Recipe struct {
	// Size is the length of the blob.
	Size int64
	// FileHash is the Xet file hash of the blob.
	FileHash xethash.Hash
	// Xorbs lists each xorb that the terms name once, in the order of
	// first use.
	Xorbs []xethash.Hash
	Terms []Term
}

// AppendEntry appends the type-5 pack entry with meta MetaClientChunked for
// the blob oid with the recipe r to dst. oid is the raw object ID: 20 bytes
// for SHA-1, 32 for SHA-256. The entry carries no inline xorbs.
//
// AppendEntry refuses a recipe that entiredb refuses, except for the
// threshold check: the caller compares r.Size with the advertised threshold.
// The server also checks each term against the footer of its xorb, which
// AppendEntry cannot see.
func AppendEntry(dst []byte, oid []byte, r *Recipe) ([]byte, error) {
	if len(oid) != 20 && len(oid) != 32 {
		return dst, fmt.Errorf("bbchunk: object ID of %d bytes, want 20 or 32", len(oid))
	}
	if err := checkRecipe(r); err != nil {
		return dst, err
	}
	payload := make([]byte, 0, len(oid)+xethash.Size+binary.MaxVarintLen64+len(r.Xorbs)*xethash.Size+len(r.Terms)*4*binary.MaxVarintLen32+2*binary.MaxVarintLen64)
	payload = append(payload, oid...)
	payload = append(payload, r.FileHash[:]...)
	payload = binary.AppendUvarint(payload, uint64(len(r.Xorbs)))
	for _, h := range r.Xorbs {
		payload = append(payload, h[:]...)
	}
	payload = binary.AppendUvarint(payload, uint64(len(r.Terms)))
	for _, t := range r.Terms {
		payload = binary.AppendUvarint(payload, uint64(t.Xorb))       //nolint:gosec // checkRecipe bounds it to the xorb table
		payload = binary.AppendUvarint(payload, uint64(t.ChunkStart)) //nolint:gosec // checkRecipe refuses a negative start
		payload = binary.AppendUvarint(payload, uint64(t.ChunkEnd))   //nolint:gosec // checkRecipe bounds it to xorb.MaxChunks
		payload = binary.AppendUvarint(payload, t.UnpackedBytes)
	}
	// The server refuses inline xorbs in v1 (ENT-4063).
	payload = binary.AppendUvarint(payload, 0)
	if len(payload) > MaxPayloadSize {
		return dst, fmt.Errorf("bbchunk: entry payload is %d bytes, over the %d-byte limit", len(payload), MaxPayloadSize)
	}
	dst = appendObjectHeader(dst, ExternalObject, r.Size)
	dst = append(dst, MetaClientChunked)
	dst = binary.AppendUvarint(dst, uint64(len(payload)))
	return append(dst, payload...), nil
}

// checkRecipe applies the server's checks of a client entry that need no
// footer.
func checkRecipe(r *Recipe) error {
	if r == nil {
		return errors.New("bbchunk: nil recipe")
	}
	if r.Size <= 0 || r.Size > MaxObjectSize {
		return fmt.Errorf("bbchunk: blob size %d is outside (0, %d]", r.Size, MaxObjectSize)
	}
	if len(r.Xorbs) == 0 || len(r.Terms) == 0 {
		return fmt.Errorf("bbchunk: recipe has %d xorbs and %d terms, want at least 1 of each", len(r.Xorbs), len(r.Terms))
	}
	seen := make(map[xethash.Hash]struct{}, len(r.Xorbs))
	for _, h := range r.Xorbs {
		if _, dup := seen[h]; dup {
			return fmt.Errorf("bbchunk: recipe lists xorb %s more than once", h)
		}
		seen[h] = struct{}{}
	}
	used := make([]bool, len(r.Xorbs))
	var total uint64
	for i, t := range r.Terms {
		switch {
		case t.Xorb < 0 || t.Xorb >= len(r.Xorbs):
			return fmt.Errorf("bbchunk: term %d: xorb index %d, table has %d", i, t.Xorb, len(r.Xorbs))
		case t.ChunkStart < 0 || t.ChunkStart >= t.ChunkEnd || t.ChunkEnd > xorb.MaxChunks:
			return fmt.Errorf("bbchunk: term %d: chunk range [%d, %d) is not in [0, %d]", i, t.ChunkStart, t.ChunkEnd, xorb.MaxChunks)
		case t.UnpackedBytes == 0 || t.UnpackedBytes > xorb.MaxRawSize:
			return fmt.Errorf("bbchunk: term %d: unpacked bytes %d is outside (0, %d]", i, t.UnpackedBytes, xorb.MaxRawSize)
		}
		used[t.Xorb] = true
		total += t.UnpackedBytes
	}
	if total != uint64(r.Size) {
		return fmt.Errorf("bbchunk: terms unpack to %d bytes, blob size is %d", total, r.Size)
	}
	for i, u := range used {
		if !u {
			return fmt.Errorf("bbchunk: no term uses xorb %d", i)
		}
	}
	return nil
}

// appendObjectHeader appends the type+size varint of a pack entry to dst.
func appendObjectHeader(dst []byte, typ byte, size int64) []byte {
	c := (typ&7)<<4 | byte(size&0x0f)
	size >>= 4
	for size > 0 {
		dst = append(dst, c|0x80)
		c = byte(size & 0x7f)
		size >>= 7
	}
	return append(dst, c)
}
