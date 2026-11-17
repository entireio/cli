package bbpush

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"

	"github.com/go-git/go-git/v6/plumbing/format/pktline"
)

// commandSection returns the commands of the request as send-pack writes
// them: one pkt-line per command, the capabilities after a NUL on the first,
// a flush, and the push options and a flush when there are any.
func commandSection(cmds []command, caps string, pushOptions []string) ([]byte, error) {
	var b bytes.Buffer
	for i, c := range cmds {
		line := c.old + " " + c.new + " " + c.ref
		if i == 0 {
			line += "\x00" + caps
		}
		if _, err := pktline.WriteString(&b, line); err != nil {
			return nil, fmt.Errorf("command %s: %w", c.ref, err)
		}
	}
	_ = pktline.WriteFlush(&b)
	if len(pushOptions) == 0 {
		return b.Bytes(), nil
	}
	for _, o := range pushOptions {
		if _, err := pktline.WriteString(&b, o); err != nil {
			return nil, fmt.Errorf("push option: %w", err)
		}
	}
	_ = pktline.WriteFlush(&b)
	return b.Bytes(), nil
}

// refResult is the report-status line of one ref.
type refResult struct {
	ok bool
	// msg is the text after the ref name, or "" for none.
	msg string
}

// report is a parsed report-status.
type report struct {
	// unpack is "ok" or the reason that the server gives.
	unpack string
	refs   map[string]refResult
}

// maxReportSize bounds a report-status: the band-1 data of a side-band
// response, or the pkt-lines of a response without side-band. A
// report-status holds one line per ref.
const maxReportSize = 8 << 20

// readReport reads the report-status of a receive-pack response. With
// side-band it demultiplexes the response, writes progress (band 2) to
// stderr and fails on band 3, as send-pack does.
func readReport(r io.Reader, sideband bool, stderr io.Writer) (*report, error) {
	if sideband {
		var data bytes.Buffer
		if err := demuxSideband(r, &data, stderr); err != nil {
			return nil, err
		}
		r = &data
	}
	br := bufio.NewReader(r)
	n, line, err := pktline.ReadLine(br)
	if err != nil || n == pktline.Flush {
		return nil, fmt.Errorf("read unpack status: %w", errors.Join(err, errors.New("no unpack status")))
	}
	unpack, ok := strings.CutPrefix(strings.TrimSuffix(string(line), "\n"), "unpack ")
	if !ok {
		return nil, fmt.Errorf("unable to parse remote unpack status: %q", line)
	}
	rep := &report{unpack: unpack, refs: map[string]refResult{}}
	size := pktline.LenSize + len(line)
	for {
		n, line, err := pktline.ReadLine(br)
		if err != nil {
			return nil, fmt.Errorf("read ref status: %w", err)
		}
		if n == pktline.Flush {
			return rep, nil
		}
		if size += pktline.LenSize + len(line); size > maxReportSize {
			return nil, fmt.Errorf("report-status is over %d bytes", maxReportSize)
		}
		text := strings.TrimSuffix(string(line), "\n")
		head, rest, _ := strings.Cut(text, " ")
		ref, msg, _ := strings.Cut(rest, " ")
		switch head {
		case "ok":
			rep.refs[ref] = refResult{ok: true, msg: msg}
		case "ng":
			if msg == "" {
				msg = "failed"
			}
			rep.refs[ref] = refResult{msg: msg}
		default:
			return nil, fmt.Errorf("invalid ref status from remote: %q", text)
		}
	}
}

// demuxSideband copies band 1 of a side-band-64k stream to data, and band 2
// to stderr with "remote: " at the start of each line.
func demuxSideband(r io.Reader, data *bytes.Buffer, stderr io.Writer) error {
	atLineStart := true
	for {
		n, p, err := pktline.ReadLine(r)
		if err != nil {
			return fmt.Errorf("read side-band: %w", err)
		}
		if n == pktline.Flush {
			return nil
		}
		if len(p) == 0 {
			continue
		}
		switch p[0] {
		case 1:
			if data.Len()+len(p)-1 > maxReportSize {
				return fmt.Errorf("side-band data is over %d bytes", maxReportSize)
			}
			data.Write(p[1:])
		case 2:
			atLineStart = writeRemote(stderr, p[1:], atLineStart)
		case 3:
			return fmt.Errorf("remote error: %s", Sanitize(strings.TrimSpace(string(p[1:]))))
		default:
			return fmt.Errorf("side-band band %d", p[0])
		}
	}
}

// writeRemote writes progress text with "remote: " before each line, and
// returns whether the text ended a line. The server controls the text, so
// it goes through sanitize; the line breaks pass.
func writeRemote(w io.Writer, p []byte, atLineStart bool) bool {
	var b bytes.Buffer
	for len(p) > 0 {
		if atLineStart {
			b.WriteString("remote: ")
		}
		i := bytes.IndexAny(p, "\r\n")
		if i < 0 {
			sanitize(&b, p)
			atLineStart = false
			break
		}
		sanitize(&b, p[:i])
		b.WriteByte(p[i])
		p = p[i+1:]
		atLineStart = true
	}
	_, _ = w.Write(b.Bytes()) //nolint:errcheck // progress is best effort
	return atLineStart
}

// failed reports whether send-pack would exit non-zero for the report.
func (rep *report) failed(cmds []command) bool {
	if rep.unpack != "ok" {
		return true
	}
	for _, c := range cmds {
		if r, ok := rep.refs[c.ref]; !ok || !r.ok {
			return true
		}
	}
	return false
}

var missingXorbRE = regexp.MustCompile(`missing xorb ([0-9a-f]{64})`)

// missingXorb reports whether the server refused the push for a xorb that it
// does not hold. entiredb names the xorb in the unpack status. A ref status
// comes from a hook too, so it does not count.
func (rep *report) missingXorb() bool {
	return missingXorbRE.MatchString(rep.unpack)
}

// Sanitize returns server text as git writes it to a terminal: a control
// byte other than tab and newline becomes ^X (DEL becomes ^?), and a colour
// sequence (ESC [ digits m) passes. See git's sideband.c.
func Sanitize(s string) string {
	var b bytes.Buffer
	sanitize(&b, []byte(s))
	return b.String()
}

func sanitize(b *bytes.Buffer, p []byte) {
	for i := 0; i < len(p); i++ {
		c := p[i]
		switch {
		case c >= 0x20 && c != 0x7f, c == '\t', c == '\n':
			b.WriteByte(c)
		case c == 0x1b:
			if n := colourSequence(p[i:]); n > 0 {
				b.Write(p[i : i+n])
				i += n - 1
				continue
			}
			b.WriteString("^[")
		case c == 0x7f:
			b.WriteString("^?")
		default:
			b.WriteByte('^')
			b.WriteByte(0x40 + c)
		}
	}
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

// writeStatus writes the helper status lines that `git send-pack
// --helper-status` writes: one line for each ref of the advertisement in
// its order, then for each new ref in command order. A ref without a
// command is "no match". rep is nil for a dry run.
func writeStatus(w io.Writer, adv *advertisement, cmds []command, rep *report) error {
	byRef := make(map[string]bool, len(cmds))
	for _, c := range cmds {
		byRef[c.ref] = true
	}
	var b bytes.Buffer
	for _, r := range adv.refs {
		if !byRef[r.name] {
			b.WriteString("error " + r.name + " no match\n")
			continue
		}
		statusLine(&b, r.name, rep)
	}
	for _, c := range cmds {
		if adv.oldOID(c.ref) == "" {
			statusLine(&b, c.ref, rep)
		}
	}
	if _, err := w.Write(b.Bytes()); err != nil {
		return fmt.Errorf("write helper status: %w", err)
	}
	return nil
}

func statusLine(b *bytes.Buffer, ref string, rep *report) {
	if rep == nil {
		b.WriteString("ok " + ref + "\n")
		return
	}
	r, ok := rep.refs[ref]
	switch {
	case !ok:
		b.WriteString("error " + ref + " expecting report\n")
	case r.ok && r.msg == "":
		b.WriteString("ok " + ref + "\n")
	case r.ok:
		b.WriteString("ok " + ref + " " + quoteC(r.msg) + "\n")
	default:
		b.WriteString("error " + ref + " " + quoteC(r.msg) + "\n")
	}
}

// quoteC quotes s as git's quote_c_style does with core.quotePath on: it
// leaves s alone unless it holds a control byte, a quote, a backslash or a
// byte over 0x7e.
func quoteC(s string) string {
	need := false
	for i := range len(s) {
		if c := s[i]; c < 0x20 || c == '"' || c == '\\' || c >= 0x7f {
			need = true
			break
		}
	}
	if !need {
		return s
	}
	var b strings.Builder
	b.WriteByte('"')
	for i := range len(s) {
		c := s[i]
		switch c {
		case '\a':
			b.WriteString(`\a`)
		case '\b':
			b.WriteString(`\b`)
		case '\t':
			b.WriteString(`\t`)
		case '\n':
			b.WriteString(`\n`)
		case '\v':
			b.WriteString(`\v`)
		case '\f':
			b.WriteString(`\f`)
		case '\r':
			b.WriteString(`\r`)
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		default:
			if c < 0x20 || c >= 0x7f {
				fmt.Fprintf(&b, "\\%03o", c)
			} else {
				b.WriteByte(c)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}
