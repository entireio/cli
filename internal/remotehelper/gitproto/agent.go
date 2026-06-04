package gitproto

import (
	"bytes"
	"fmt"
	"io"
	"strings"

	"github.com/go-git/go-git/v6/plumbing/format/pktline"
)

// AppendAgentToV2Request appends helperAgent to an existing v2
// client-side agent line. It leaves requests without agent= unchanged.
func AppendAgentToV2Request(message []byte, helperAgent string) ([]byte, error) {
	if helperAgent == "" {
		return message, nil
	}

	for off := 0; off < len(message); {
		line, err := parseRawPktLine(message, off)
		if err != nil {
			return nil, err
		}
		if line.special() {
			off = line.next
			continue
		}
		if !bytes.HasPrefix(line.payload, []byte("agent=")) {
			off = line.next
			continue
		}

		amendedPayload := appendAgentPayload(line.payload, helperAgent, " ")
		if bytes.Equal(amendedPayload, line.payload) {
			return message, nil
		}
		return replacePktLine(message, line, amendedPayload)
	}

	return message, nil
}

// AppendAgentToUploadPackRequest appends helperAgent to the agent
// capability in a v0/v1 upload-pack request. It leaves requests
// without agent= unchanged.
func AppendAgentToUploadPackRequest(message []byte, helperAgent string) ([]byte, error) {
	if helperAgent == "" || len(message) == 0 {
		return message, nil
	}

	line, err := parseRawPktLine(message, 0)
	if err != nil {
		return nil, err
	}
	if line.special() || !bytes.HasPrefix(line.payload, []byte("want ")) {
		return message, nil
	}

	payload, eol := splitTrailingLF(line.payload)
	fields := bytes.SplitN(payload, []byte{' '}, 3)
	if len(fields) < 3 {
		return message, nil
	}

	caps, changed := appendAgentCapability(fields[2], helperAgent)
	if !changed {
		return message, nil
	}

	amendedPayload := make([]byte, 0, len(payload)+len(helperAgent)+1+len(eol))
	amendedPayload = append(amendedPayload, fields[0]...)
	amendedPayload = append(amendedPayload, ' ')
	amendedPayload = append(amendedPayload, fields[1]...)
	amendedPayload = append(amendedPayload, ' ')
	amendedPayload = append(amendedPayload, caps...)
	amendedPayload = append(amendedPayload, eol...)
	return replacePktLine(message, line, amendedPayload)
}

// AppendAgentToReceivePackRequest appends helperAgent to the agent
// capability on the first receive-pack command line. It leaves
// requests without agent= unchanged.
func AppendAgentToReceivePackRequest(message []byte, helperAgent string) ([]byte, error) {
	if helperAgent == "" {
		return message, nil
	}

	for off := 0; off < len(message); {
		line, err := parseRawPktLine(message, off)
		if err != nil {
			return nil, err
		}
		if line.special() {
			return message, nil
		}

		command, capsPayload, ok := bytes.Cut(line.payload, []byte{0})
		if !ok {
			off = line.next
			continue
		}

		capsPayload, eol := splitTrailingLF(capsPayload)
		caps, changed := appendAgentCapability(capsPayload, helperAgent)
		if !changed {
			return message, nil
		}

		amendedPayload := make([]byte, 0, len(line.payload)+len(helperAgent)+1)
		amendedPayload = append(amendedPayload, command...)
		amendedPayload = append(amendedPayload, 0)
		amendedPayload = append(amendedPayload, caps...)
		amendedPayload = append(amendedPayload, eol...)
		return replacePktLine(message, line, amendedPayload)
	}

	return message, nil
}

type rawPktLine struct {
	start   int
	next    int
	length  int
	payload []byte
}

func (l rawPktLine) special() bool {
	return l.length == pktline.Flush ||
		l.length == pktline.Delim ||
		l.length == pktline.ResponseEnd
}

func parseRawPktLine(message []byte, off int) (rawPktLine, error) {
	if len(message)-off < pktline.LenSize {
		return rawPktLine{}, fmt.Errorf("pkt-line length at offset %d: %w", off, io.ErrUnexpectedEOF)
	}
	pktLen, err := pktline.ParseLength(message[off : off+pktline.LenSize])
	if err != nil {
		return rawPktLine{}, fmt.Errorf("parsing pkt-line length at offset %d: %w", off, err)
	}
	switch pktLen {
	case pktline.Flush, pktline.Delim, pktline.ResponseEnd:
		return rawPktLine{start: off, next: off + pktline.LenSize, length: pktLen}, nil
	}
	if pktLen < pktline.LenSize {
		return rawPktLine{}, fmt.Errorf("invalid pkt-line length %d at offset %d", pktLen, off)
	}
	next := off + pktLen
	if next > len(message) {
		return rawPktLine{}, fmt.Errorf("pkt-line length %d at offset %d exceeds message length", pktLen, off)
	}
	return rawPktLine{
		start:   off,
		next:    next,
		length:  pktLen,
		payload: message[off+pktline.LenSize : next],
	}, nil
}

func replacePktLine(message []byte, line rawPktLine, payload []byte) ([]byte, error) {
	if len(payload)+pktline.LenSize > 0xffff {
		return nil, fmt.Errorf("amended pkt-line too long: %d bytes", len(payload)+pktline.LenSize)
	}
	var out bytes.Buffer
	out.Grow(len(message) + len(payload) - len(line.payload))
	out.Write(message[:line.start])
	if _, err := fmt.Fprintf(&out, "%04x", len(payload)+pktline.LenSize); err != nil {
		return nil, fmt.Errorf("writing pkt-line length: %w", err)
	}
	out.Write(payload)
	out.Write(message[line.next:])
	return out.Bytes(), nil
}

func appendAgentPayload(payload []byte, helperAgent, separator string) []byte {
	body, eol := splitTrailingLF(payload)
	value := strings.TrimPrefix(string(body), "agent=")
	amendedValue, changed := appendAgentValue(value, helperAgent, separator)
	if !changed {
		return payload
	}
	amended := make([]byte, 0, len("agent=")+len(amendedValue)+len(eol))
	amended = append(amended, "agent="...)
	amended = append(amended, amendedValue...)
	amended = append(amended, eol...)
	return amended
}

func appendAgentCapability(caps []byte, helperAgent string) ([]byte, bool) {
	parts := bytes.Split(caps, []byte{' '})
	for i, part := range parts {
		value, ok := bytes.CutPrefix(part, []byte("agent="))
		if !ok {
			continue
		}
		amendedValue, changed := appendAgentValue(string(value), helperAgent, "+")
		if !changed {
			return caps, false
		}
		parts[i] = []byte("agent=" + amendedValue)
		return bytes.Join(parts, []byte{' '}), true
	}
	return caps, false
}

func appendAgentValue(value, helperAgent, separator string) (string, bool) {
	if value == "" {
		return helperAgent, true
	}
	if strings.Contains(value, helperAgent) {
		return value, false
	}
	return value + separator + helperAgent, true
}

func splitTrailingLF(b []byte) ([]byte, []byte) {
	if len(b) > 0 && b[len(b)-1] == '\n' {
		return b[:len(b)-1], b[len(b)-1:]
	}
	return b, nil
}
