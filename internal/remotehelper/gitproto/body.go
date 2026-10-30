package gitproto

import (
	"fmt"
	"io"
	"net/http"
	"sort"
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

// replaceFirstChunk swaps the first chunk for p and recomputes offsets.
func (b *RequestBody) replaceFirstChunk(p []byte) {
	b.chunks[0] = p
	var end int64
	for i, c := range b.chunks {
		end += int64(len(c))
		b.ends[i] = end
	}
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
// capability on the request's first command line, which both push paths
// keep in the first chunk: send-pack writes the command section as its
// own stateless-rpc packets ahead of the pack, and ReadReceivePackRequest
// stores the first pkt-line as a chunk of its own. Only that chunk is
// copied. Git always puts the capabilities on the first line, so a first
// line that doesn't fit in the first chunk is reported as an error.
func AppendAgentToReceivePackBody(body *RequestBody, helperAgent string) error {
	if body.Len() == 0 {
		return nil
	}
	first, err := AppendAgentToReceivePackRequest(body.chunks[0], helperAgent)
	if err != nil {
		return fmt.Errorf("first request chunk: %w", err)
	}
	body.replaceFirstChunk(first)
	return nil
}
