package bbchunk

import (
	"bytes"
	"errors"
	"fmt"
	"io"

	"github.com/jmwoliver/xet-go/xethash"
	"github.com/jmwoliver/xet-go/xorb"
)

// A complete xorb is the network form followed by a V1 footer, as
// xorb.AppendFooter writes it. entiredb stores only the canonical footer,
// with a zero nonce and zero reserved bytes, and serves the stored bytes as
// they are. xet-go checks the layout, the chunks and the hashes. This
// package also requires the canonical footer.

var (
	// ErrMalformedXorb reports a complete xorb that does not match its hash
	// or the stored format: a bad chunk, a bad footer, or a wrong length.
	ErrMalformedXorb = errors.New("bbchunk: malformed complete xorb")
	// ErrCorruptXorb reports stored chunk bytes that do not match their
	// footer. The copy is damaged. Remove it and download it again.
	ErrCorruptXorb = errors.New("bbchunk: corrupt xorb")
)

// maxFooterSize is the length of the largest V1 footer, with its trailer.
var maxFooterSize = xorb.FooterSize(xorb.MaxChunks)

// Footer is the chunk table of a complete xorb, a V1 footer. Every Footer
// that this package returns hashes to its xorb hash.
//
// ChunkBoundaryOffsets[i] is the offset just past the stored bytes of chunk
// i, with its 8-byte header; chunk 0 starts at 0. UnpackedChunkOffsets[i]
// is the offset just past the unpacked data of chunk i.
type Footer xorb.Footer

// Chunks returns the number of chunks.
func (f *Footer) Chunks() int { return len(f.ChunkHashes) }

// Size returns the length of the complete xorb: its chunks and its footer.
func (f *Footer) Size() int64 {
	n := len(f.ChunkBoundaryOffsets)
	return int64(f.ChunkBoundaryOffsets[n-1]) + int64(xorb.FooterSize(n))
}

// StoredRange returns the inclusive range of stored bytes of chunks
// [start, end). The range starts at the header of chunk start. A recipe term
// records this range.
func (f *Footer) StoredRange(start, end int) (first, last uint64, err error) {
	if err := f.checkRange(start, end); err != nil {
		return 0, 0, err
	}
	return uint64(f.storedStart(start)), uint64(f.ChunkBoundaryOffsets[end-1]) - 1, nil
}

// UnpackedSize returns the length of the data of chunks [start, end).
func (f *Footer) UnpackedSize(start, end int) (uint64, error) {
	if err := f.checkRange(start, end); err != nil {
		return 0, err
	}
	return uint64(f.UnpackedChunkOffsets[end-1] - f.unpackedStart(start)), nil
}

func (f *Footer) checkRange(start, end int) error {
	if start < 0 || start >= end || end > len(f.ChunkBoundaryOffsets) {
		return fmt.Errorf("bbchunk: chunk range [%d, %d) outside a xorb of %d chunks", start, end, len(f.ChunkBoundaryOffsets))
	}
	return nil
}

func (f *Footer) storedStart(i int) uint32 {
	if i == 0 {
		return 0
	}
	return f.ChunkBoundaryOffsets[i-1]
}

func (f *Footer) unpackedStart(i int) uint32 {
	if i == 0 {
		return 0
	}
	return f.UnpackedChunkOffsets[i-1]
}

// CheckComplete reads a complete xorb from r to its end and checks it
// against want with xorb.VerifyComplete. It also requires the canonical
// footer. It decompresses and hashes every chunk, so a xorb that passes
// holds the data that want names. size is the length that the sender
// declared, or -1. CheckComplete holds a few chunks, the chunk table and the
// last bytes read in memory. An error from r comes back wrapped, and does
// not wrap ErrMalformedXorb; every other failure does.
func CheckComplete(r io.Reader, want xethash.Hash, size int64) (*Footer, error) {
	tr := &tailReader{r: r}
	f, err := xorb.VerifyComplete(tr, want, size)
	if err != nil {
		return nil, xorbError(ErrMalformedXorb, err)
	}
	// VerifyComplete reads r to its end, so the tail ends with the footer.
	if err := checkCanonical(f, tr.tail); err != nil {
		return nil, err
	}
	return (*Footer)(f), nil
}

// ReadFooter reads the footer at the end of the complete xorb of size bytes
// in ra and checks it against want with xorb.ReadFooter. It also requires
// the canonical footer. It does not read the chunks: the reader that
// NewTermReader returns checks each chunk. An error from ra comes back
// wrapped, and does not wrap ErrMalformedXorb; every other failure does.
func ReadFooter(ra io.ReaderAt, size int64, want xethash.Hash) (*Footer, error) {
	f, err := xorb.ReadFooter(ra, size, want)
	if err != nil {
		return nil, xorbError(ErrMalformedXorb, err)
	}
	raw := make([]byte, xorb.FooterSize(len(f.ChunkHashes)))
	// A ReaderAt may return io.EOF with a full read, so only a short read
	// fails.
	if n, err := ra.ReadAt(raw, size-int64(len(raw))); n < len(raw) {
		if err == nil {
			err = io.ErrUnexpectedEOF
		}
		return nil, fmt.Errorf("bbchunk: read xorb footer: %w", err)
	}
	if err := checkCanonical(f, raw); err != nil {
		return nil, err
	}
	return (*Footer)(f), nil
}

// checkCanonical checks that tail ends with the footer that entiredb stores
// for f: f with a zero nonce and zero reserved bytes. xet-go reads the nonce
// but does not check it, and it does not read the reserved bytes.
func checkCanonical(f *xorb.Footer, tail []byte) error {
	canonical := *f
	canonical.UniquenessNonce = [4]byte{}
	if !bytes.HasSuffix(tail, xorb.AppendFooter(nil, &canonical)) {
		return fmt.Errorf("%w: the footer of xorb %s is not canonical", ErrMalformedXorb, f.XorbHash)
	}
	return nil
}

// xorbError wraps err, a failure from xet-go, in kind when the bytes are at
// fault. A failure of the source of the bytes stays a read error.
func xorbError(kind, err error) error {
	var formatErr *xorb.FormatError
	if errors.As(err, &formatErr) || errors.Is(err, xorb.ErrHashMismatch) {
		return fmt.Errorf("%w: %w", kind, err)
	}
	return fmt.Errorf("bbchunk: read xorb: %w", err)
}

// tailReader passes reads to r and keeps the last bytes read: at least
// maxFooterSize of them, or all of them when there are fewer.
type tailReader struct {
	r    io.Reader
	tail []byte
}

func (t *tailReader) Read(p []byte) (int, error) {
	n, err := t.r.Read(p)
	if len(t.tail)+n > 2*maxFooterSize {
		keep := max(0, maxFooterSize-n)
		t.tail = append(t.tail[:0], t.tail[len(t.tail)-keep:]...)
	}
	t.tail = append(t.tail, p[:n]...)
	return n, err //nolint:wrapcheck // VerifyComplete wraps the error of r
}

// NewTermReader returns a reader of the data of chunks [start, end) of the
// complete xorb in ra with footer f, in order. A xorb.ChunkRangeReader reads
// the stored range of the chunks: it decompresses one chunk at a time and
// checks it against f. A chunk that fails its check wraps ErrCorruptXorb,
// and a failed read of ra does not. f must come from ReadFooter or
// CheckComplete.
func NewTermReader(ra io.ReaderAt, f *Footer, start, end int) (io.Reader, error) {
	first, last, err := f.StoredRange(start, end)
	if err != nil {
		return nil, err
	}
	sr := io.NewSectionReader(ra, int64(first), int64(last-first+1)) //nolint:gosec // xorb.MaxCompleteSize bounds the range
	r, err := xorb.NewChunkRangeReader(sr, (*xorb.Footer)(f), start, end)
	if err != nil {
		return nil, fmt.Errorf("bbchunk: %w", err)
	}
	return &termReader{r: r}, nil
}

type termReader struct {
	r *xorb.ChunkRangeReader
}

func (t *termReader) Read(p []byte) (int, error) {
	n, err := t.r.Read(p)
	if err != nil && !errors.Is(err, io.EOF) {
		return n, xorbError(ErrCorruptXorb, err)
	}
	return n, err //nolint:wrapcheck // io.EOF passes as it is
}
