package bbfetch

import (
	"errors"
	"fmt"
	"hash"
	"io"
)

// packReader reads a pack from r through one buffer. It is an io.ByteReader,
// so zlib reads exactly the bytes of each stream and no more.
//
// Bytes that the reader has consumed reach the sinks at sink, and before
// every refill of the buffer, in blocks. So the forwarded bytes go on before
// the reader waits for more input. A caller that must not forward the next
// bytes calls sink first, then changes forward.
type packReader struct {
	r   io.Reader
	buf []byte
	// buf[pos:end] is unread. buf[mark:pos] is consumed and has not reached
	// the sinks yet.
	pos, end, mark int
	// inHash hashes every consumed byte, for the check of the trailer.
	inHash hash.Hash
	// forward takes every consumed byte when it is not nil.
	forward func([]byte) error
	// readErr is the sticky error of r: io.ErrUnexpectedEOF at the end of
	// the input, else a *StreamError. Every read inside a pack expects more
	// bytes, so the end of the input is unexpected.
	readErr error
	// sinkErr is the sticky error of forward.
	sinkErr error
}

// sink passes the consumed bytes to the sinks.
func (p *packReader) sink() error {
	if p.sinkErr != nil {
		return p.sinkErr
	}
	b := p.buf[p.mark:p.pos]
	p.mark = p.pos
	if len(b) == 0 {
		return nil
	}
	if p.inHash != nil {
		p.inHash.Write(b)
	}
	if p.forward != nil {
		p.sinkErr = p.forward(b)
	}
	return p.sinkErr
}

// fill sinks the consumed bytes, moves the unread bytes to the front of the
// buffer and reads more. It returns an error when no byte arrived.
func (p *packReader) fill() error {
	if err := p.sink(); err != nil {
		return err
	}
	if p.readErr != nil {
		return p.readErr
	}
	p.end = copy(p.buf, p.buf[p.pos:p.end])
	p.pos, p.mark = 0, 0
	for {
		n, err := p.r.Read(p.buf[p.end:])
		p.end += n
		switch {
		case err == nil:
		case errors.Is(err, io.EOF):
			p.readErr = io.ErrUnexpectedEOF
		default:
			p.readErr = &StreamError{Err: fmt.Errorf("bbfetch: read pack: %w", err)}
		}
		if n > 0 {
			return nil
		}
		if p.readErr != nil {
			return p.readErr
		}
	}
}

// ensure reads until n bytes are unread or the input ends, and returns the
// number of unread bytes. n is at most the buffer size.
func (p *packReader) ensure(n int) int {
	for p.end-p.pos < n {
		if p.fill() != nil {
			break
		}
	}
	return p.end - p.pos
}

// failure returns the *StreamError of a sink or of r, or nil. The end of the
// input is not a failure of the stream.
func (p *packReader) failure() error {
	if p.sinkErr != nil {
		return p.sinkErr
	}
	var se *StreamError
	if errors.As(p.readErr, &se) {
		return p.readErr
	}
	return nil
}

func (p *packReader) ReadByte() (byte, error) {
	if p.pos == p.end {
		if err := p.fill(); err != nil {
			return 0, err
		}
	}
	c := p.buf[p.pos]
	p.pos++
	return c, nil
}

func (p *packReader) Read(b []byte) (int, error) {
	if len(b) == 0 {
		return 0, nil
	}
	if p.pos == p.end {
		if err := p.fill(); err != nil {
			return 0, err
		}
	}
	n := copy(b, p.buf[p.pos:p.end])
	p.pos += n
	return n, nil
}
