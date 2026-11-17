// Package bbfetch turns the pack of a fetch that negotiated entire-bb-fetch
// into a pack that git reads. Such a pack has version 0xe002, and ends with
// type-5 entries that hold blob recipes (meta 0x02) instead of blobs. The
// Rewriter forwards every other entry to git as it arrives, keeps the
// recipes, and lets the caller append each blob in place of its recipe.
// RewritePack drives a Rewriter for the helper: it gets the xorbs of the
// recipes into the local cache and rebuilds each blob from them.
//
// The pack contract is in entiredb's docs/specs/client-chunked-fetch.md.
package bbfetch

import (
	"bytes"
	"compress/zlib"
	"crypto/sha1" //nolint:gosec // the pack trailer and object IDs of a SHA-1 repository
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"hash"
	"hash/adler32"
	"io"
	"strconv"

	"github.com/entireio/cli/internal/remotehelper/bbchunk"
)

// Pack entry types that the rewriter reads.
const (
	objCommit   = 1
	objBlob     = 3
	objTag      = 4
	objOfsDelta = 6
	objRefDelta = 7
)

const (
	packHeaderSize = 12
	// maxEntryHeader bounds the type+size varint of an entry: 4 bits of
	// size in the first byte and 7 in each of 9 more.
	maxEntryHeader = 10
	// readBufferSize is the size of the input buffer. The rewriter passes
	// the forwarded bytes on in blocks of about this size.
	readBufferSize = 64 << 10
	// maxStoredBlock is the largest data length of a stored deflate block.
	maxStoredBlock = 0xffff
)

// ErrMalformedPack reports a pack that breaks the pack contract of a fetch.
var ErrMalformedPack = errors.New("bbfetch: malformed pack")

// StreamError is a failure of the stream that carries the pack: a read of
// the server's response that fails, or a write to git that fails. A band-3
// error of the server, a broken connection and a response that ends early
// are stream errors. A StreamError never wraps ErrMalformedPack: it says
// nothing about the pack or its recipes.
type StreamError struct{ Err error }

func (e *StreamError) Error() string { return e.Err.Error() }

func (e *StreamError) Unwrap() error { return e.Err }

// ChunkedEntry is a type-5 entry of a fetched pack.
type ChunkedEntry struct {
	// Size is the logical size from the entry header: the blob's length.
	Size int64
	// Payload is the raw payload of the entry: the blob OID, then the
	// recipe. bbchunk.ParseChunkedPayload decodes it. The receiver owns it.
	Payload []byte
}

type rewriterState int

const (
	stateNew rewriterState = iota
	stateAppending
	stateFinished
	stateFailed
)

// Rewriter rewrites one fetched pack. Call ReadPack, then AppendBlob once for
// each ChunkedEntry in order, then Finish. Memory does not grow with the
// size of the pack: the rewriter holds one input buffer, the payload of the
// entry it reads, and the base OID of each REF_DELTA entry.
//
// After an error the rewriter refuses every call. It never writes a trailer
// after an error, so git refuses the pack.
type Rewriter struct {
	out      io.Writer
	newHash  func() hash.Hash
	hashSize int
	// outHash hashes every byte written to out, for the new trailer.
	outHash hash.Hash
	state   rewriterState
	// externals counts the type-5 entries of the input, and appended the
	// blobs written in their place.
	externals int
	appended  int
	block     []byte
}

// NewRewriter returns a rewriter that writes the new pack to out.
// objectFormat is the repository's object format: "sha1" or "sha256". An
// empty objectFormat is SHA-1.
func NewRewriter(out io.Writer, objectFormat string) (*Rewriter, error) {
	rw := &Rewriter{out: out}
	switch objectFormat {
	case "", "sha1":
		rw.newHash, rw.hashSize = sha1.New, sha1.Size
	case "sha256":
		rw.newHash, rw.hashSize = sha256.New, sha256.Size
	default:
		return nil, fmt.Errorf("bbfetch: unknown object format %q", objectFormat)
	}
	rw.outHash = rw.newHash()
	return rw, nil
}

// ReadPack reads the whole pack from in, up to and including its trailer,
// and checks the trailer. It writes the header with version 2 and the same
// object count, and every entry before the first type-5 entry, to out as
// they arrive. It calls recipe for each type-5 entry in pack order. An
// error from recipe stops ReadPack and ReadPack returns it.
//
// ReadPack refuses a pack with an entry of another type after a type-5
// entry, a type-5 entry with a meta byte other than 0x02, a type-5 entry in
// a version-2 pack, a REF_DELTA whose base is a type-5 entry of the pack,
// a trailer that does not match, and bytes after the trailer. Such errors,
// and a pack that ends before its trailer, wrap ErrMalformedPack. It does
// not check a recipe beyond its frame.
//
// When a read of in or a write to out fails, ReadPack returns that failure
// as a *StreamError, whatever else it found.
func (rw *Rewriter) ReadPack(in io.Reader, recipe func(ChunkedEntry) error) error {
	if rw.state != stateNew {
		return errors.New("bbfetch: ReadPack called twice or after an error")
	}
	pr := &packReader{r: in, buf: make([]byte, readBufferSize), inHash: rw.newHash()}
	if err := rw.readPack(pr, recipe); err != nil {
		rw.state = stateFailed
		if failure := pr.failure(); failure != nil {
			return failure
		}
		return err
	}
	rw.state = stateAppending
	return nil
}

func (rw *Rewriter) readPack(pr *packReader, recipe func(ChunkedEntry) error) error {
	if pr.ensure(packHeaderSize) < packHeaderSize {
		return fmt.Errorf("%w: read header: %w", ErrMalformedPack, io.ErrUnexpectedEOF)
	}
	header := pr.buf[pr.pos : pr.pos+packHeaderSize]
	version := binary.BigEndian.Uint32(header[4:8])
	if string(header[:4]) != "PACK" || (version != 2 && version != bbchunk.PackVersionExternal) {
		return fmt.Errorf("%w: header %x", ErrMalformedPack, header)
	}
	count := binary.BigEndian.Uint32(header[8:])
	out := [packHeaderSize]byte{'P', 'A', 'C', 'K'}
	binary.BigEndian.PutUint32(out[4:], 2)
	binary.BigEndian.PutUint32(out[8:], count)
	pr.pos += packHeaderSize
	if err := pr.sink(); err != nil {
		return err
	}
	if err := rw.write(out[:]); err != nil {
		return err
	}
	pr.forward = rw.write

	er := &entryReader{rw: rw, pr: pr, external: version == bbchunk.PackVersionExternal, recipe: recipe, refBases: map[string]struct{}{}}
	for i := range count {
		if err := er.next(); err != nil {
			return fmt.Errorf("pack entry %d: %w", i, err)
		}
	}
	return er.trailer()
}

// write writes p to out and hashes it for the new trailer. A failure is a
// *StreamError.
func (rw *Rewriter) write(p []byte) error {
	rw.outHash.Write(p)
	if _, err := rw.out.Write(p); err != nil {
		return &StreamError{Err: fmt.Errorf("bbfetch: write pack: %w", err)}
	}
	return nil
}

// AppendBlob writes a blob entry of size bytes in place of the next type-5
// entry. It reads exactly size bytes from r and writes them as stored
// (uncompressed) deflate blocks, so it holds one block at a time. It returns
// the object ID of the blob that it wrote. The caller compares it with the
// recipe's OID before it calls Finish. A failed write to out is a
// *StreamError.
func (rw *Rewriter) AppendBlob(size int64, r io.Reader) ([]byte, error) {
	if rw.state != stateAppending {
		return nil, errors.New("bbfetch: AppendBlob called out of order or after an error")
	}
	if rw.appended == rw.externals {
		return nil, fmt.Errorf("bbfetch: the pack has %d type-5 entries, all replaced", rw.externals)
	}
	oid, err := rw.appendBlob(size, r)
	if err != nil {
		rw.state = stateFailed
		return nil, err
	}
	rw.appended++
	return oid, nil
}

func (rw *Rewriter) appendBlob(size int64, r io.Reader) ([]byte, error) {
	if size < 0 {
		return nil, fmt.Errorf("bbfetch: negative blob size %d", size)
	}
	oid := rw.newHash()
	oid.Write([]byte("blob " + strconv.FormatInt(size, 10) + "\x00"))
	var sum hash.Hash = adler32.New()
	if rw.block == nil {
		rw.block = make([]byte, 5+maxStoredBlock)
	}
	if err := rw.write(bbchunk.AppendObjectHeader(nil, objBlob, size)); err != nil {
		return nil, err
	}
	// The zlib header: deflate with a 32 KiB window, no dictionary, and the
	// check bits that make it a multiple of 31.
	if err := rw.write([]byte{0x78, 0x01}); err != nil {
		return nil, err
	}
	for remaining := size; ; {
		n := min(remaining, maxStoredBlock)
		remaining -= n
		block := rw.block[:5+n]
		block[0] = 0
		if remaining == 0 {
			block[0] = 1 // BFINAL, and BTYPE 00: stored
		}
		binary.LittleEndian.PutUint16(block[1:], uint16(n))
		binary.LittleEndian.PutUint16(block[3:], ^uint16(n))
		if _, err := io.ReadFull(r, block[5:]); err != nil {
			return nil, fmt.Errorf("bbfetch: read blob data: %w", err)
		}
		oid.Write(block[5:])
		sum.Write(block[5:])
		if err := rw.write(block); err != nil {
			return nil, err
		}
		if remaining == 0 {
			break
		}
	}
	if err := rw.write(sum.Sum(nil)); err != nil {
		return nil, err
	}
	return oid.Sum(nil), nil
}

// Finish writes the trailer of the new pack. It refuses a pack in which a
// type-5 entry has no blob in its place. A failed write to out is a
// *StreamError.
func (rw *Rewriter) Finish() error {
	if rw.state != stateAppending {
		return errors.New("bbfetch: Finish called out of order or after an error")
	}
	if rw.appended != rw.externals {
		rw.state = stateFailed
		return fmt.Errorf("bbfetch: %d of %d type-5 entries replaced", rw.appended, rw.externals)
	}
	rw.state = stateFinished
	if _, err := rw.out.Write(rw.outHash.Sum(nil)); err != nil {
		return &StreamError{Err: fmt.Errorf("bbfetch: write trailer: %w", err)}
	}
	return nil
}

// entryReader reads the entries of one pack.
type entryReader struct {
	rw *Rewriter
	pr *packReader
	// external is true for a pack of version 0xe002.
	external bool
	recipe   func(ChunkedEntry) error
	// refBases holds the base OID of each REF_DELTA entry. No type-5 entry
	// may have one of these OIDs.
	refBases map[string]struct{}
	zr       io.ReadCloser
}

// next reads one entry.
func (er *entryReader) next() error {
	pr := er.pr
	if err := pr.sink(); err != nil {
		return err
	}
	avail := pr.ensure(maxEntryHeader)
	if err := pr.failure(); err != nil {
		return err
	}
	typ, size, n, err := parseEntryHeader(pr.buf[pr.pos : pr.pos+avail])
	if err != nil {
		return err
	}
	pr.pos += n
	if typ == bbchunk.ExternalObject {
		return er.chunked(size)
	}
	if er.rw.externals > 0 {
		return fmt.Errorf("%w: an entry of type %d after a type-5 entry", ErrMalformedPack, typ)
	}
	switch typ {
	case objOfsDelta:
		if err := skipOffset(pr); err != nil {
			return err
		}
	case objRefDelta:
		base := make([]byte, er.rw.hashSize)
		if _, err := io.ReadFull(pr, base); err != nil {
			return fmt.Errorf("%w: read REF_DELTA base: %w", ErrMalformedPack, err)
		}
		er.refBases[string(base)] = struct{}{}
	default:
		if typ < objCommit || typ > objTag {
			return fmt.Errorf("%w: entry type %d", ErrMalformedPack, typ)
		}
	}
	return er.inflate(size)
}

// inflate reads the zlib stream of an entry and checks its length. It reads
// exactly the bytes of the stream: zlib reads a flate.Reader byte by byte.
func (er *entryReader) inflate(size int64) error {
	var err error
	if er.zr == nil {
		er.zr, err = zlib.NewReader(er.pr)
	} else {
		rs, ok := er.zr.(zlib.Resetter)
		if !ok {
			return errors.New("bbfetch: the zlib reader cannot reset")
		}
		err = rs.Reset(er.pr, nil)
	}
	if err != nil {
		return fmt.Errorf("%w: zlib header: %w", ErrMalformedPack, err)
	}
	n, err := io.Copy(io.Discard, er.zr)
	if err != nil {
		return fmt.Errorf("%w: inflate: %w", ErrMalformedPack, err)
	}
	if n != size {
		return fmt.Errorf("%w: entry inflates to %d bytes, header says %d", ErrMalformedPack, n, size)
	}
	return nil
}

// chunked reads a type-5 entry. Its bytes are not forwarded: the caller
// writes a blob in its place.
func (er *entryReader) chunked(size int64) error {
	pr := er.pr
	if !er.external {
		return fmt.Errorf("%w: a type-5 entry in a version-2 pack", ErrMalformedPack)
	}
	pr.forward = nil
	er.rw.externals++
	meta, err := pr.ReadByte()
	if err != nil {
		return fmt.Errorf("%w: read meta byte: %w", ErrMalformedPack, err)
	}
	declared, err := binary.ReadUvarint(pr)
	if err != nil {
		return fmt.Errorf("%w: read payload size: %w", ErrMalformedPack, err)
	}
	if err := bbchunk.CheckChunkedFrame(meta, declared, er.rw.hashSize); err != nil {
		return err //nolint:wrapcheck // the error names the entry kind
	}
	payload := make([]byte, declared)
	if _, err := io.ReadFull(pr, payload); err != nil {
		return fmt.Errorf("%w: read payload: %w", ErrMalformedPack, err)
	}
	if _, ok := er.refBases[string(payload[:er.rw.hashSize])]; ok {
		return fmt.Errorf("%w: a REF_DELTA base is the type-5 entry %x", ErrMalformedPack, payload[:er.rw.hashSize])
	}
	return er.recipe(ChunkedEntry{Size: size, Payload: payload})
}

// trailer reads and checks the trailer, and checks that no byte follows it.
func (er *entryReader) trailer() error {
	pr := er.pr
	if err := pr.sink(); err != nil {
		return err
	}
	want := pr.inHash.Sum(nil)
	pr.forward, pr.inHash = nil, nil
	got := make([]byte, len(want))
	if _, err := io.ReadFull(pr, got); err != nil {
		return fmt.Errorf("%w: read trailer: %w", ErrMalformedPack, err)
	}
	if !bytes.Equal(got, want) {
		return fmt.Errorf("%w: trailer %x, pack hashes to %x", ErrMalformedPack, got, want)
	}
	if pr.ensure(1) > 0 {
		return fmt.Errorf("%w: data after the trailer", ErrMalformedPack)
	}
	return pr.failure()
}

// parseEntryHeader decodes the type+size varint at the start of b.
func parseEntryHeader(b []byte) (typ byte, size int64, n int, err error) {
	if len(b) == 0 {
		return 0, 0, 0, fmt.Errorf("%w: truncated entry header", ErrMalformedPack)
	}
	c := b[0]
	typ = (c >> 4) & 7
	size = int64(c & 0x0f)
	shift := 4
	for n = 1; c&0x80 != 0; n++ {
		if n == len(b) {
			return 0, 0, 0, fmt.Errorf("%w: truncated entry header", ErrMalformedPack)
		}
		if shift > 56 {
			return 0, 0, 0, fmt.Errorf("%w: entry size overflows", ErrMalformedPack)
		}
		c = b[n]
		size |= int64(c&0x7f) << shift
		shift += 7
	}
	return typ, size, n, nil
}

// skipOffset reads the base offset of an OFS_DELTA entry. Its value does not
// matter here: every entry before the first type-5 entry keeps its offset,
// and an earlier entry is never type 5.
func skipOffset(pr *packReader) error {
	for i := 0; ; i++ {
		c, err := pr.ReadByte()
		if err != nil {
			return fmt.Errorf("%w: read OFS_DELTA offset: %w", ErrMalformedPack, err)
		}
		if c&0x80 == 0 {
			return nil
		}
		if i == 8 {
			return fmt.Errorf("%w: OFS_DELTA offset overflows", ErrMalformedPack)
		}
	}
}
