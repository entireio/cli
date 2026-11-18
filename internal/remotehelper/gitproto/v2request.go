package gitproto

import (
	"bytes"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/go-git/go-git/v6/plumbing/format/pktline"
)

// HasV2FetchFeature reports whether the fetch= capability of a v2
// advertisement lists feature.
func HasV2FetchFeature(advertisement []byte, feature string) bool {
	for off := 0; off < len(advertisement); {
		line, err := parseRawPktLine(advertisement, off)
		if err != nil {
			return false
		}
		off = line.next
		if line.special() {
			continue
		}
		value, ok := bytes.CutPrefix(line.payload, []byte("fetch="))
		if !ok {
			continue
		}
		value, _ = splitTrailingLF(value)
		for _, f := range bytes.Split(value, []byte{' '}) {
			if string(f) == feature {
				return true
			}
		}
		return false
	}
	return false
}

// V2Request is a parsed v2 command request: the capability lines before
// the delim packet and the argument lines after it, without their newlines.
type V2Request struct {
	Capabilities []string
	Arguments    []string
	// delim is set when the request has the delim packet.
	delim bool
}

// ParseV2Request parses a flush-terminated v2 command request, as
// ReadFlushTerminatedMessage returns it.
func ParseV2Request(message []byte) (*V2Request, error) {
	req := &V2Request{}
	args := false
	for off := 0; off < len(message); {
		line, err := parseRawPktLine(message, off)
		if err != nil {
			return nil, err
		}
		off = line.next
		switch line.length {
		case pktline.Flush:
			if off != len(message) {
				return nil, errors.New("v2 request: data after the flush")
			}
			return req, nil
		case pktline.Delim:
			if args {
				return nil, errors.New("v2 request: second delim")
			}
			args, req.delim = true, true
			continue
		case pktline.ResponseEnd:
			return nil, errors.New("v2 request: unexpected response-end packet")
		}
		text, _ := splitTrailingLF(line.payload)
		if args {
			req.Arguments = append(req.Arguments, string(text))
		} else {
			req.Capabilities = append(req.Capabilities, string(text))
		}
	}
	return nil, errors.New("v2 request: no flush")
}

// Capability returns the value of the capability key=value, or "" and
// false when the request does not send key.
func (r *V2Request) Capability(key string) (string, bool) {
	for _, c := range r.Capabilities {
		if v, ok := strings.CutPrefix(c, key+"="); ok {
			return v, true
		}
	}
	return "", false
}

// HasArgument reports whether the request sends the argument line arg.
func (r *V2Request) HasArgument(arg string) bool {
	return slices.Contains(r.Arguments, arg)
}

// AddV2Argument appends the argument line arg to a flush-terminated v2
// command request, before its flush. It adds the delim packet when the
// request has no arguments yet.
func AddV2Argument(message []byte, arg string) ([]byte, error) {
	req, err := ParseV2Request(message)
	if err != nil {
		return nil, err
	}
	body := message[:len(message)-pktline.LenSize]
	var out bytes.Buffer
	out.Grow(len(message) + 2*pktline.LenSize + len(arg) + 1)
	out.Write(body)
	if !req.delim {
		out.WriteString("0001")
	}
	if _, err := pktline.WriteString(&out, arg+"\n"); err != nil {
		return nil, fmt.Errorf("v2 request: write argument: %w", err)
	}
	out.WriteString("0000")
	return out.Bytes(), nil
}
