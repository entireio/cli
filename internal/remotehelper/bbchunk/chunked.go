package bbchunk

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"slices"

	"github.com/jmwoliver/xet-go/xethash"
	"github.com/jmwoliver/xet-go/xorb"
)

// MetaChunked is the meta byte of a chunked entry as entiredb stores it. A
// server sends such entries as they are in a fetch that negotiated them.
const MetaChunked byte = 0x02

// ErrMalformedChunked reports a stored chunked entry that does not match the
// layout or the checks of entiredb.
var ErrMalformedChunked = errors.New("bbchunk: malformed chunked entry")

// minChunkedTermSize is the smallest encoding of a stored term: six one-byte
// uvarints. The parser bounds a declared term count with it before it
// allocates the terms.
const minChunkedTermSize = 6

// ChunkedPayload is the decoded payload of a MetaChunked entry. The payload
// is:
//
//	oid         20 bytes (SHA-1) or 32 bytes (SHA-256)
//	file hash   32 bytes
//	xorb count  uvarint, then 32 bytes for each xorb hash
//	term count  uvarint, then for each term six uvarints:
//	            xorb index, chunk start, chunk end,
//	            unpacked bytes, first byte, last byte
//
// The layout and every check copy ChunkedPayload and ParseChunkedPayload in
// entiredb's internal/go-git/plumbing/format/packfile/external.go.
type ChunkedPayload struct {
	// OID is the raw git object ID of the whole blob.
	OID      []byte
	FileHash xethash.Hash
	// Xorbs lists each xorb that a term uses, once.
	Xorbs []xethash.Hash
	// Terms list the blob's data in file order.
	Terms []ChunkedTerm
}

// ChunkedTerm is one run of chunks in one xorb.
type ChunkedTerm struct {
	// Xorb is an index into ChunkedPayload.Xorbs.
	Xorb uint32
	// ChunkStart and ChunkEnd are the half-open range of chunk indexes
	// [ChunkStart, ChunkEnd) in the xorb.
	ChunkStart, ChunkEnd uint32
	UnpackedBytes        uint64
	// FirstByte and LastByte are the inclusive range of stored bytes in the
	// xorb. The range starts at the header of chunk ChunkStart.
	FirstByte, LastByte uint64
}

// CheckChunkedFrame checks the meta byte and the declared payload size of a
// type-5 entry in a fetch, before the reader reads the payload. A fetch
// carries MetaChunked entries only.
func CheckChunkedFrame(meta byte, declared uint64, hashSize int) error {
	if hashSize != 20 && hashSize != 32 {
		return fmt.Errorf("%w: unsupported hash size %d", ErrMalformedChunked, hashSize)
	}
	if meta != MetaChunked {
		return fmt.Errorf("%w: kind %#x, want %#x", ErrMalformedChunked, meta, MetaChunked)
	}
	if declared > MaxPayloadSize {
		return fmt.Errorf("%w: payload of %d bytes is over %d", ErrMalformedChunked, declared, MaxPayloadSize)
	}
	if declared < uint64(hashSize) { //nolint:gosec // hashSize is 20 or 32
		return fmt.Errorf("%w: payload of %d bytes is shorter than the %d-byte object ID", ErrMalformedChunked, declared, hashSize)
	}
	return nil
}

// ParseChunkedPayload decodes and checks the payload of a MetaChunked entry
// whose header gives the logical size size. payload starts with the blob
// OID of hashSize bytes. The result does not refer to payload.
//
// The payload comes from the network, so the parser checks each declared
// count against the bytes that remain before it allocates.
func ParseChunkedPayload(payload []byte, hashSize int, size int64) (*ChunkedPayload, error) {
	if err := CheckChunkedFrame(MetaChunked, uint64(len(payload)), hashSize); err != nil {
		return nil, err
	}
	if size < 0 {
		return nil, fmt.Errorf("%w: negative size %d", ErrMalformedChunked, size)
	}
	d := chunkedDecoder{b: payload[hashSize:]}
	c := &ChunkedPayload{OID: slices.Clone(payload[:hashSize])}
	if !d.hash(&c.FileHash) {
		return nil, fmt.Errorf("%w: truncated file hash", ErrMalformedChunked)
	}
	xorbs, err := d.count("xorb", xethash.Size)
	if err != nil {
		return nil, err
	}
	c.Xorbs = make([]xethash.Hash, xorbs)
	for i := range c.Xorbs {
		d.hash(&c.Xorbs[i])
	}
	if err := checkDistinctXorbs(c.Xorbs); err != nil {
		return nil, err
	}
	terms, err := d.count("term", minChunkedTermSize)
	if err != nil {
		return nil, err
	}
	c.Terms = make([]ChunkedTerm, terms)
	remaining := uint64(size)
	used := make([]bool, len(c.Xorbs))
	for i := range c.Terms {
		t, err := d.term(xorbs)
		if err == nil && t.UnpackedBytes > remaining {
			err = fmt.Errorf("terms unpack to more than the logical size %d", size)
		}
		if err != nil {
			return nil, fmt.Errorf("%w: term %d: %w", ErrMalformedChunked, i, err)
		}
		remaining -= t.UnpackedBytes
		used[t.Xorb] = true
		c.Terms[i] = t
	}
	if len(d.b) != 0 {
		return nil, fmt.Errorf("%w: %d bytes after the last term", ErrMalformedChunked, len(d.b))
	}
	if remaining != 0 {
		return nil, fmt.Errorf("%w: terms unpack to %d bytes, logical size is %d", ErrMalformedChunked, uint64(size)-remaining, size)
	}
	if i := slices.Index(used, false); i >= 0 {
		return nil, fmt.Errorf("%w: no term uses xorb %d", ErrMalformedChunked, i)
	}
	return c, nil
}

// checkDistinctXorbs checks that the xorb table lists each hash once.
func checkDistinctXorbs(xorbs []xethash.Hash) error {
	if len(xorbs) < 2 {
		return nil
	}
	sorted := slices.Clone(xorbs)
	slices.SortFunc(sorted, func(a, b xethash.Hash) int { return bytes.Compare(a[:], b[:]) })
	for i := 1; i < len(sorted); i++ {
		if sorted[i] == sorted[i-1] {
			return fmt.Errorf("%w: xorb %x is listed more than once", ErrMalformedChunked, sorted[i][:])
		}
	}
	return nil
}

// chunkedDecoder reads the fields of a chunked payload from the front of b.
type chunkedDecoder struct {
	b []byte
}

func (d *chunkedDecoder) hash(h *xethash.Hash) bool {
	if len(d.b) < len(h) {
		return false
	}
	d.b = d.b[copy(h[:], d.b):]
	return true
}

func (d *chunkedDecoder) uvarint() (uint64, bool) {
	v, n := binary.Uvarint(d.b)
	if n <= 0 {
		return 0, false
	}
	d.b = d.b[n:]
	return v, true
}

// count reads a count of items and checks that it is at least 1 and that the
// remaining bytes can hold that many items of at least itemSize bytes.
func (d *chunkedDecoder) count(item string, itemSize uint64) (uint64, error) {
	v, ok := d.uvarint()
	if !ok {
		return 0, fmt.Errorf("%w: invalid %s count", ErrMalformedChunked, item)
	}
	if v == 0 {
		return 0, fmt.Errorf("%w: no %ss", ErrMalformedChunked, item)
	}
	if v > uint64(len(d.b))/itemSize {
		return 0, fmt.Errorf("%w: %d %ss do not fit in %d bytes", ErrMalformedChunked, v, item, len(d.b))
	}
	return v, nil
}

// term reads one stored term and checks it against Xet's limits for one
// xorb. The xorb table holds xorbs entries.
func (d *chunkedDecoder) term(xorbs uint64) (ChunkedTerm, error) {
	var v [6]uint64
	for i := range v {
		var ok bool
		if v[i], ok = d.uvarint(); !ok {
			return ChunkedTerm{}, errors.New("truncated or invalid field")
		}
	}
	index, start, end, unpacked, first, last := v[0], v[1], v[2], v[3], v[4], v[5]
	switch {
	case index >= xorbs:
		return ChunkedTerm{}, fmt.Errorf("xorb index %d, table has %d", index, xorbs)
	case start >= end:
		return ChunkedTerm{}, fmt.Errorf("chunk range [%d, %d) is empty", start, end)
	case end > xorb.MaxChunks:
		return ChunkedTerm{}, fmt.Errorf("chunk end %d is over the %d chunks of a xorb", end, xorb.MaxChunks)
	case unpacked == 0:
		return ChunkedTerm{}, errors.New("unpacked bytes is 0")
	case unpacked > xorb.MaxRawSize:
		return ChunkedTerm{}, fmt.Errorf("unpacked bytes %d is over the %d bytes of a xorb", unpacked, xorb.MaxRawSize)
	case first > last:
		return ChunkedTerm{}, fmt.Errorf("first byte %d is after last byte %d", first, last)
	case last >= xorb.MaxNetworkSize:
		return ChunkedTerm{}, fmt.Errorf("last byte %d is past the %d stored bytes of a xorb", last, xorb.MaxNetworkSize)
	}
	return ChunkedTerm{
		Xorb:          uint32(index), //nolint:gosec // index is below the xorb count, which the payload size bounds
		ChunkStart:    uint32(start), //nolint:gosec // start is below end, which is at most xorb.MaxChunks
		ChunkEnd:      uint32(end),
		UnpackedBytes: unpacked,
		FirstByte:     first,
		LastByte:      last,
	}, nil
}
