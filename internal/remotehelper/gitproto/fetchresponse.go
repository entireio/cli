package gitproto

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"

	"github.com/go-git/go-git/v6/plumbing/format/pktline"
)

// SectionPackfile is the last section of a protocol v2 fetch response.
const SectionPackfile = "packfile"

// Sections of a protocol v2 fetch response, in the order that
// gitprotocol-v2(5) requires.
const (
	sectionAcknowledgments = "acknowledgments"
	sectionShallowInfo     = "shallow-info"
	sectionWantedRefs      = "wanted-refs"
	sectionPackfileURIs    = "packfile-uris"
)

var fetchSections = []string{
	sectionAcknowledgments,
	sectionShallowInfo,
	sectionWantedRefs,
	sectionPackfileURIs,
	SectionPackfile,
}

// Side-band channels (bands) of side-band-64k.
const (
	BandData     byte = 1
	BandProgress byte = 2
	BandError    byte = 3
)

// FetchPacketKind classifies a packet of a v2 fetch response.
type FetchPacketKind int

const (
	// FetchHeader is a section header.
	FetchHeader FetchPacketKind = iota + 1
	// FetchLine is a line of a section other than the packfile section.
	FetchLine
	// FetchPackData is a band-1 packet of the packfile section.
	FetchPackData
	// FetchProgress is a band-2 packet.
	FetchProgress
	// FetchError is a band-3 packet or an "ERR" packet. Next returns a
	// RemoteError after it.
	FetchError
	// FetchDelim ends a section that another section follows.
	FetchDelim
	// FetchFlush ends the response.
	FetchFlush
)

var fetchPacketKinds = [...]string{
	FetchHeader:   "header",
	FetchLine:     "line",
	FetchPackData: "pack data",
	FetchProgress: "progress",
	FetchError:    "error",
	FetchDelim:    "delim",
	FetchFlush:    "flush",
}

func (k FetchPacketKind) String() string {
	if k > 0 && int(k) < len(fetchPacketKinds) {
		return fetchPacketKinds[k]
	}
	return "FetchPacketKind(" + strconv.Itoa(int(k)) + ")"
}

// FetchPacket is one packet of a v2 fetch response.
type FetchPacket struct {
	Kind FetchPacketKind
	// Section is the section that the packet is in. It is empty before the
	// first header.
	Section string
	// Wire is the packet as it was on the wire, with its length prefix.
	Wire []byte
	// Payload is the content of the packet: a line without its band byte,
	// the pack data, or the message of a progress or error packet.
	Payload []byte
}

// RemoteError is a fatal error that the server sent in a fetch response.
type RemoteError struct {
	// Message is the server's text after AppendSanitized.
	Message string
}

func (e *RemoteError) Error() string { return "remote error: " + e.Message }

func newRemoteError(msg []byte) *RemoteError {
	return &RemoteError{Message: string(AppendSanitized(nil, bytes.TrimRight(msg, "\n")))}
}

type fetchState int

const (
	fetchExpectHeader fetchState = iota
	fetchInSection
	fetchEnded
	fetchDone
)

// FetchResponseReader reads a protocol v2 fetch response one packet at a
// time and checks its framing: the order of the sections, the delim and
// flush packets between them, and the side-band packets of the packfile
// section. It does not check the lines of a section; git does.
//
// A server sends the whole response in side-band packets when the request
// asked for sideband-all. Then a progress or error packet can come between
// any two packets of the response.
type FetchResponseReader struct {
	r           io.Reader
	sidebandAll bool
	buf         [pktline.MaxSize]byte
	state       fetchState
	// section indexes fetchSections. It is -1 before the first header.
	section int
	err     error
}

// NewFetchResponseReader returns a reader of the fetch response in r.
// sidebandAll is true when the request sent the sideband-all argument.
func NewFetchResponseReader(r io.Reader, sidebandAll bool) *FetchResponseReader {
	return &FetchResponseReader{r: r, sidebandAll: sidebandAll, section: -1}
}

// Next returns the next packet. The packet's Wire and Payload are valid
// until the next call. Next returns io.EOF after the flush that ends the
// response, when the stream ends there too. It returns an error for bytes
// after that flush, for a malformed response, and after a FetchError
// packet.
func (f *FetchResponseReader) Next() (FetchPacket, error) {
	if f.err != nil {
		return FetchPacket{}, f.err
	}
	if f.state == fetchDone {
		return FetchPacket{}, io.EOF
	}
	if f.state == fetchEnded {
		f.state = fetchDone
		n, err := f.r.Read(f.buf[:1])
		if n > 0 {
			return FetchPacket{}, f.fail(errors.New("fetch response: data after the final flush"))
		}
		if err != nil && !errors.Is(err, io.EOF) {
			return FetchPacket{}, f.fail(fmt.Errorf("fetch response: %w", err))
		}
		return FetchPacket{}, io.EOF
	}
	pkt, err := f.next()
	if err != nil {
		return FetchPacket{}, f.fail(err)
	}
	return pkt, nil
}

func (f *FetchResponseReader) fail(err error) error {
	f.err = err
	return err
}

func (f *FetchResponseReader) next() (FetchPacket, error) {
	raw, length, err := f.read()
	if err != nil {
		return FetchPacket{}, err
	}
	pkt := FetchPacket{Wire: raw}
	if f.section >= 0 {
		pkt.Section = fetchSections[f.section]
	}
	switch length {
	case pktline.Flush:
		return f.flush(pkt)
	case pktline.Delim:
		return f.delim(pkt)
	case pktline.ResponseEnd:
		return FetchPacket{}, errors.New("fetch response: unexpected response-end packet")
	}
	payload := raw[pktline.LenSize:]
	inPack := f.state == fetchInSection && pkt.Section == SectionPackfile
	if !inPack && bytes.HasPrefix(payload, []byte("ERR ")) {
		pkt.Kind, pkt.Payload = FetchError, payload[len("ERR "):]
		f.err = newRemoteError(pkt.Payload)
		return pkt, nil
	}
	if !inPack && !f.sidebandAll {
		return f.line(pkt, payload)
	}
	if len(payload) == 0 {
		return FetchPacket{}, errors.New("fetch response: missing side-band designator")
	}
	pkt.Payload = payload[1:]
	switch payload[0] {
	case BandData:
		if inPack {
			pkt.Kind = FetchPackData
			return pkt, nil
		}
		return f.line(pkt, payload[1:])
	case BandProgress:
		pkt.Kind = FetchProgress
		return pkt, nil
	case BandError:
		pkt.Kind = FetchError
		f.err = newRemoteError(pkt.Payload)
		return pkt, nil
	default:
		return FetchPacket{}, fmt.Errorf("fetch response: bad side-band %d", payload[0])
	}
}

// read reads one packet into f.buf and returns it with its length field.
func (f *FetchResponseReader) read() ([]byte, int, error) {
	if _, err := io.ReadFull(f.r, f.buf[:pktline.LenSize]); err != nil {
		return nil, 0, fmt.Errorf("fetch response: read packet length: %w", unexpectedEOF(err))
	}
	length, err := pktline.ParseLength(f.buf[:pktline.LenSize])
	if err != nil {
		return nil, 0, fmt.Errorf("fetch response: %w", err)
	}
	if length < pktline.LenSize {
		return f.buf[:pktline.LenSize], length, nil
	}
	if _, err := io.ReadFull(f.r, f.buf[pktline.LenSize:length]); err != nil {
		return nil, 0, fmt.Errorf("fetch response: read packet: %w", unexpectedEOF(err))
	}
	return f.buf[:length], length, nil
}

func unexpectedEOF(err error) error {
	if errors.Is(err, io.EOF) {
		return io.ErrUnexpectedEOF
	}
	return err
}

// flush handles a flush packet. Only the packfile section, or an
// acknowledgments section with no section after it, ends with a flush.
func (f *FetchResponseReader) flush(pkt FetchPacket) (FetchPacket, error) {
	if f.state != fetchInSection || (pkt.Section != sectionAcknowledgments && pkt.Section != SectionPackfile) {
		return FetchPacket{}, fmt.Errorf("fetch response: unexpected flush after %q", pkt.Section)
	}
	f.state = fetchEnded
	pkt.Kind = FetchFlush
	return pkt, nil
}

// delim handles a delim packet: the end of a section other than packfile.
func (f *FetchResponseReader) delim(pkt FetchPacket) (FetchPacket, error) {
	if f.state != fetchInSection || pkt.Section == SectionPackfile {
		return FetchPacket{}, fmt.Errorf("fetch response: unexpected delim after %q", pkt.Section)
	}
	f.state = fetchExpectHeader
	pkt.Kind = FetchDelim
	return pkt, nil
}

// line handles a line outside the packfile section: a header or a line of
// the current section.
func (f *FetchResponseReader) line(pkt FetchPacket, line []byte) (FetchPacket, error) {
	pkt.Payload = line
	if f.state == fetchInSection {
		pkt.Kind = FetchLine
		return pkt, nil
	}
	name := string(bytes.TrimSuffix(line, []byte("\n")))
	i := slices.Index(fetchSections, name)
	if i <= f.section {
		return FetchPacket{}, fmt.Errorf("fetch response: expected a section header after %q, got %.40q", pkt.Section, line)
	}
	f.section, f.state = i, fetchInSection
	pkt.Kind, pkt.Section = FetchHeader, name
	return pkt, nil
}

// CopyFetchResponse copies the v2 fetch response in src to dst. It passes
// every packet through unchanged except these:
//
//   - pack reads the band-1 data of the packfile section from data, and
//     writes the pack that git gets to out.
//   - The text of a progress or error packet goes through
//     AppendSanitized, in a new packet of the same band.
//
// Progress passes through as it arrives. When pack returns, data must be at
// the end of the pack data. Then CopyFetchResponse writes the flush that ends
// the response. It writes no flush after an error, so git sees a truncated
// response.
func CopyFetchResponse(dst io.Writer, src io.Reader, sidebandAll bool, pack func(data io.Reader, out *SidebandWriter) error) error {
	f := NewFetchResponseReader(src, sidebandAll)
	out := NewSidebandWriter(dst)
	for {
		pkt, err := f.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := relay(dst, out, pkt); err != nil {
			return err
		}
		if pkt.Kind == FetchHeader && pkt.Section == SectionPackfile {
			if err := copyPackfileSection(dst, f, out, pack); err != nil {
				return err
			}
		}
	}
}

// relay writes a packet other than pack data to dst.
func relay(dst io.Writer, out *SidebandWriter, pkt FetchPacket) error {
	switch {
	case pkt.Kind == FetchProgress:
		return out.Progress(appendSanitized(nil, pkt.Payload, true))
	case pkt.Kind == FetchError && pkt.Wire[pktline.LenSize] == BandError:
		return out.Error(AppendSanitized(nil, pkt.Payload))
	case pkt.Kind == FetchError:
		msg := AppendSanitized([]byte("ERR "), pkt.Payload)
		if _, err := pktline.Write(dst, msg[:min(len(msg), pktline.MaxPayloadSize)]); err != nil {
			return fmt.Errorf("fetch response: write: %w", err)
		}
		return nil
	}
	if _, err := dst.Write(pkt.Wire); err != nil {
		return fmt.Errorf("fetch response: write: %w", err)
	}
	return nil
}

func copyPackfileSection(dst io.Writer, f *FetchResponseReader, out *SidebandWriter, pack func(data io.Reader, out *SidebandWriter) error) error {
	data := &packDataReader{f: f, out: out, dst: dst}
	if err := pack(data, out); err != nil {
		return err
	}
	if !data.done {
		var b [1]byte
		n, err := data.Read(b[:])
		if n > 0 {
			return errors.New("fetch response: pack data after the end of the pack")
		}
		if !errors.Is(err, io.EOF) {
			return err
		}
	}
	if err := out.Flush(); err != nil {
		return err
	}
	if _, err := dst.Write(data.flush); err != nil {
		return fmt.Errorf("fetch response: write: %w", err)
	}
	return nil
}

// packDataReader reads the band-1 data of the packfile section. It relays
// the packets of other bands as they arrive.
type packDataReader struct {
	f    *FetchResponseReader
	out  *SidebandWriter
	dst  io.Writer
	data []byte
	done bool
	// flush is the packet that ended the section.
	flush []byte
}

func (p *packDataReader) Read(b []byte) (int, error) {
	for len(p.data) == 0 {
		if p.done {
			return 0, io.EOF
		}
		if err := p.fill(); err != nil {
			return 0, err
		}
	}
	n := copy(b, p.data)
	p.data = p.data[n:]
	return n, nil
}

// fill reads the next packet of the section. It sends the pack bytes that
// out holds before it waits for the server, so git gets them as they
// arrive.
func (p *packDataReader) fill() error {
	if err := p.out.Flush(); err != nil {
		return err
	}
	pkt, err := p.f.Next()
	if err != nil {
		return err
	}
	switch pkt.Kind {
	case FetchPackData:
		p.data = pkt.Payload
	case FetchFlush:
		p.done = true
		p.flush = slices.Clone(pkt.Wire)
	case FetchProgress, FetchError:
		return relay(p.dst, p.out, pkt)
	case FetchHeader, FetchLine, FetchDelim:
		return fmt.Errorf("fetch response: a %s packet in the packfile section", pkt.Kind)
	}
	return nil
}
