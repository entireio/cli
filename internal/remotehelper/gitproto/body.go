package gitproto

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"sort"

	"github.com/go-git/go-git/v6/plumbing/format/pktline"
)

// bodyChunkSize is the block size RequestBody.ReadFrom reads raw pack
// data in. It is close to the largest pkt-line payload (65516 bytes), the
// most send-pack's stateless-rpc framing puts in one chunk, so both push
// paths hold the pack in blocks of about the same size.
const bodyChunkSize = 64 << 10

// RequestBody is a receive-pack request held as the chunks it was read
// in rather than one contiguous buffer. A push holds its pack in memory
// once: appending to a growing buffer would hold up to two copies while
// the buffer grows, and amending the agent capability would need a third.
//
// The body is replayable — Reader returns a fresh io.ReadSeeker each
// time — so the transport's 401 retry and replica failover can resend it.
type RequestBody struct {
	chunks [][]byte
	// ends[i] is the offset one past the last byte of chunks[i].
	ends []int64
}

// Len returns the body's size in bytes.
func (b *RequestBody) Len() int64 {
	if len(b.ends) == 0 {
		return 0
	}
	return b.ends[len(b.ends)-1]
}

// appendChunk takes ownership of p and adds it to the end of the body.
func (b *RequestBody) appendChunk(p []byte) {
	if len(p) == 0 {
		return
	}
	b.chunks = append(b.chunks, p)
	b.ends = append(b.ends, b.Len()+int64(len(p)))
}

// replaceRange replaces bytes [start, end) with p. The chunks on either
// side are re-sliced rather than copied, so the pack stays where it is.
func (b *RequestBody) replaceRange(start, end int64, p []byte) {
	chunks, ends, total := b.chunks, b.ends, b.Len()
	b.chunks, b.ends = nil, nil
	keep := func(from, to int64) {
		for i, c := range chunks {
			cStart := ends[i] - int64(len(c))
			if lo, hi := max(cStart, from), min(ends[i], to); lo < hi {
				b.appendChunk(c[lo-cStart : hi-cStart])
			}
		}
	}
	keep(0, start)
	b.appendChunk(p)
	keep(end, total)
}

// ReadFrom appends everything r yields until io.EOF, in bodyChunkSize
// blocks. Like io.Copy, it treats only io.EOF itself as the end: any other
// error, io.ErrUnexpectedEOF and wrapped EOFs included, means the body is
// incomplete and is returned, even after a full block.
func (b *RequestBody) ReadFrom(r io.Reader) (int64, error) {
	var total int64
	for {
		buf := make([]byte, bodyChunkSize)
		n := 0
		var err error
		for n < len(buf) && err == nil {
			var m int
			m, err = r.Read(buf[n:])
			n += m
		}
		b.appendChunk(buf[:n])
		total += int64(n)
		// io.Reader returns EOF unwrapped; a wrapped one is a failure, as in io.Copy.
		if err == io.EOF {
			return total, nil
		}
		if err != nil {
			return total, err //nolint:wrapcheck // io.ReaderFrom passes the reader's error through
		}
	}
}

// ReadAt implements io.ReaderAt over the chunks.
func (b *RequestBody) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, fmt.Errorf("request body: negative offset %d", off)
	}
	if off >= b.Len() {
		return 0, io.EOF
	}
	i := sort.Search(len(b.ends), func(i int) bool { return b.ends[i] > off })
	n := 0
	for ; i < len(b.chunks) && n < len(p); i++ {
		start := b.ends[i] - int64(len(b.chunks[i]))
		n += copy(p[n:], b.chunks[i][off+int64(n)-start:])
	}
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

// Reader returns a new reader positioned at the start of the body.
func (b *RequestBody) Reader() *io.SectionReader {
	return io.NewSectionReader(b, 0, b.Len())
}

// ApplyTo declares the body's size on req and lets net/http rewind it.
// http.NewRequest infers both only for bytes and strings readers;
// without them the POST goes out chunked, and a redirect that must
// resend the body can't.
func (b *RequestBody) ApplyTo(req *http.Request) {
	req.ContentLength = b.Len()
	req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(b.Reader()), nil }
}

// AppendAgentToReceivePackBody appends helperAgent to the agent
// capability on the request's first command line: the first pkt-line
// carrying a NUL before the flush. Shallow lines can precede it, and
// send-pack's stateless-rpc framing can split it across chunks, so the
// lines are read through the body. Only that line is copied. It leaves
// requests without agent= unchanged.
func AppendAgentToReceivePackBody(body *RequestBody, helperAgent string) error {
	if helperAgent == "" {
		return nil
	}
	lenBuf := make([]byte, pktline.LenSize)
	for off := int64(0); off < body.Len(); {
		if _, err := body.ReadAt(lenBuf, off); err != nil {
			return fmt.Errorf("pkt-line length at offset %d: %w", off, io.ErrUnexpectedEOF)
		}
		pktLen, err := pktline.ParseLength(lenBuf)
		if err != nil {
			return fmt.Errorf("parsing pkt-line length at offset %d: %w", off, err)
		}
		switch pktLen {
		case pktline.Flush, pktline.Delim, pktline.ResponseEnd:
			return nil
		}
		if pktLen < pktline.LenSize {
			return fmt.Errorf("invalid pkt-line length %d at offset %d", pktLen, off)
		}
		line := make([]byte, pktLen)
		if _, err := body.ReadAt(line, off); err != nil {
			return fmt.Errorf("pkt-line length %d at offset %d exceeds request length", pktLen, off)
		}
		if bytes.IndexByte(line[pktline.LenSize:], 0) < 0 {
			off += int64(pktLen)
			continue
		}
		amended, err := AppendAgentToReceivePackRequest(line, helperAgent)
		if err != nil {
			return err
		}
		body.replaceRange(off, off+int64(pktLen), amended)
		return nil
	}
	return nil
}
