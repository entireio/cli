package gitproto

import (
	"fmt"
	"io"

	"github.com/go-git/go-git/v6/plumbing/format/pktline"
)

// sidebandHeader is the length prefix and the band byte of a side-band
// packet.
const sidebandHeader = pktline.LenSize + 1

// SidebandWriter writes side-band-64k packets to w. Every packet is at most
// pktline.MaxSize bytes, with its length prefix and band byte. Write collects
// band-1 data into full packets. Flush sends a partial packet.
type SidebandWriter struct {
	w   io.Writer
	buf [pktline.MaxSize]byte
	// n is the number of data bytes in buf after the header.
	n int
}

// NewSidebandWriter returns a writer of side-band-64k packets to w.
func NewSidebandWriter(w io.Writer) *SidebandWriter {
	return &SidebandWriter{w: w}
}

// Write adds p to the pack data on band 1. It sends each packet that fills.
func (s *SidebandWriter) Write(p []byte) (int, error) {
	written := 0
	for len(p) > 0 {
		k := copy(s.buf[sidebandHeader+s.n:], p)
		s.n += k
		p = p[k:]
		written += k
		if sidebandHeader+s.n == len(s.buf) {
			if err := s.Flush(); err != nil {
				return written, err
			}
		}
	}
	return written, nil
}

// Flush sends the band-1 data that the writer holds.
func (s *SidebandWriter) Flush() error {
	if s.n == 0 {
		return nil
	}
	pkt := s.buf[:sidebandHeader+s.n]
	putPktLength(pkt, len(pkt))
	pkt[pktline.LenSize] = BandData
	s.n = 0
	if _, err := s.w.Write(pkt); err != nil {
		return fmt.Errorf("side-band: write: %w", err)
	}
	return nil
}

// Progress sends msg on band 2, after the band-1 data that the writer holds.
// The caller sanitizes text from the server.
func (s *SidebandWriter) Progress(msg []byte) error {
	return s.writeBand(BandProgress, msg)
}

// Error sends msg on band 3, after the band-1 data that the writer holds.
// Git stops the fetch when it reads it.
func (s *SidebandWriter) Error(msg []byte) error {
	return s.writeBand(BandError, msg)
}

func (s *SidebandWriter) writeBand(band byte, msg []byte) error {
	if err := s.Flush(); err != nil {
		return err
	}
	var pkt [pktline.MaxSize]byte
	for first := true; first || len(msg) > 0; first = false {
		k := copy(pkt[sidebandHeader:], msg)
		msg = msg[k:]
		putPktLength(pkt[:], sidebandHeader+k)
		pkt[pktline.LenSize] = band
		if _, err := s.w.Write(pkt[:sidebandHeader+k]); err != nil {
			return fmt.Errorf("side-band: write band %d: %w", band, err)
		}
	}
	return nil
}

// putPktLength writes n as the 4-digit hex length prefix of pkt.
func putPktLength(pkt []byte, n int) {
	const digits = "0123456789abcdef"
	for i := pktline.LenSize - 1; i >= 0; i-- {
		pkt[i] = digits[n&0xf]
		n >>= 4
	}
}

// AppendSanitized appends server text to dst as git shows it on a terminal:
// a control byte other than tab and newline becomes ^X (DEL becomes ^?), and
// a colour sequence (ESC [ digits m) passes. See git's sideband.c.
func AppendSanitized(dst, p []byte) []byte {
	return appendSanitized(dst, p, false)
}

// appendSanitized is AppendSanitized. With passCR a carriage return passes
// too, so progress meters still redraw their line.
func appendSanitized(dst, p []byte, passCR bool) []byte {
	for i := 0; i < len(p); i++ {
		c := p[i]
		switch {
		case c >= 0x20 && c != 0x7f, c == '\t', c == '\n', c == '\r' && passCR:
			dst = append(dst, c)
		case c == 0x1b:
			if n := colourSequence(p[i:]); n > 0 {
				dst = append(dst, p[i:i+n]...)
				i += n - 1
				continue
			}
			dst = append(dst, "^["...)
		case c == 0x7f:
			dst = append(dst, "^?"...)
		default:
			dst = append(dst, '^', 0x40+c)
		}
	}
	return dst
}

// colourSequence returns the length of the SGR sequence at the start of p,
// or 0.
func colourSequence(p []byte) int {
	if len(p) < 3 || p[0] != 0x1b || p[1] != '[' {
		return 0
	}
	for i := 2; i < len(p); i++ {
		switch c := p[i]; {
		case c == 'm':
			return i + 1
		case (c < '0' || c > '9') && c != ';' && c != ':':
			return 0
		}
	}
	return 0
}
