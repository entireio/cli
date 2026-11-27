// Package bbchunk cuts large git blobs ("BB", git big blobs) into Xet chunks
// and packs the chunks into xorbs, for a client-chunked push to entiredb.
//
// The server checks every byte that this package makes. It rehashes each
// uploaded xorb, and it rebuilds each blob from its recipe and checks the git
// object ID. A chunk boundary, a chunk hash or a xorb hash that differs from
// the server's makes chunks that do not deduplicate, or a push that the
// server refuses. So the parameters and the rules here copy entiredb exactly:
// its internal/xet and xorbstore packages are the source of truth.
// github.com/jmwoliver/xet-go implements the chunker, the hashes and the xorb
// format. Keep its version equal to the version that entiredb pins.
package bbchunk

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"github.com/jmwoliver/xet-go/chunker"
	"github.com/jmwoliver/xet-go/xethash"
)

// Chunk size bounds in bytes. They are Xet's defaults. Chunk boundaries, and
// so every chunk, xorb and file hash, depend on them.
const (
	targetChunkSize = 64 << 10
	minChunkSize    = 8 << 10
	maxChunkSize    = 128 << 10
)

// anchorModulus is the sampling rate of the anchor index: about one chunk in
// anchorModulus is an anchor.
const anchorModulus = 64

// chunkerConfig returns the chunker configuration for chunked blobs. Do not
// use chunker.DefaultConfig: any package can change it. Do not use the zero
// Config: it follows xet-go's constants, not the values pinned here.
func chunkerConfig() chunker.Config {
	return chunker.Config{
		TargetSize: targetChunkSize,
		MinSize:    minChunkSize,
		MaxSize:    maxChunkSize,
	}
}

// IsAnchor reports whether the chunk with hash h is an anchor. The server
// indexes the anchors of each stored xorb, and a client looks up its anchors
// to find xorbs that the repo holds. The rule reads the last 8 bytes of the
// hash as a little-endian integer. It must equal the server's rule, or
// lookups miss.
func IsAnchor(h xethash.Hash) bool {
	return binary.LittleEndian.Uint64(h[xethash.Size-8:])%anchorModulus == 0
}

// Chunk is one content-defined chunk of a blob.
type Chunk struct {
	// Data is owned by the receiver. Split does not reuse it.
	Data []byte
	Hash xethash.Hash
}

// Split cuts the bytes of r into chunks and calls emit for each chunk in
// order. It holds at most one chunk and one read buffer, so memory does not
// grow with the input. An error from emit stops Split and Split returns it.
func Split(ctx context.Context, r io.Reader, emit func(Chunk) error) error {
	if emit == nil {
		return errors.New("bbchunk: nil emit function")
	}
	err := chunker.SplitContext(ctx, r, chunkerConfig(), func(c chunker.Chunk) error {
		return emit(Chunk{Data: c.Data, Hash: xethash.Chunk(c.Data)})
	})
	if err != nil {
		return fmt.Errorf("bbchunk: split: %w", err)
	}
	return nil
}
