// Package gitproto implements higher-level Git smart-HTTP / stateless-RPC
// wire helpers built on top of the pkt-line layer in
// github.com/go-git/go-git/v6/plumbing/format/pktline:
//
//   - v2 capability-advertisement detection (IsV2Advertisement)
//   - v2-request command extraction (V2Command)
//   - reading a flush-terminated v2 message verbatim
//     (ReadFlushTerminatedMessage)
//   - stripping the smart-HTTP service announcement
//     (ReadPostServiceAdvertisement, backed by packp.SmartReply)
//   - reading the receive-pack request body the helper relays
//   - reading the send-pack stateless-rpc outer framing
//
// Everything here is pure Git protocol — no Entire-specific knowledge.
package gitproto

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/go-git/go-git/v6/plumbing/format/pktline"
	"github.com/go-git/go-git/v6/plumbing/protocol/packp"
)

// IsV2Advertisement reports whether p begins with the v2 service
// capability line ("version 2\n"). The peek is conservative: if the
// length prefix is malformed or claims more bytes than p contains,
// we return false rather than panic.
func IsV2Advertisement(p []byte) bool {
	if len(p) < pktline.LenSize {
		return false
	}
	n, err := pktline.ParseLength(p[:pktline.LenSize])
	if err != nil || n < pktline.LenSize || len(p) < n {
		return false
	}
	return string(p[pktline.LenSize:n]) == "version 2\n"
}

// V2Command extracts the "command=" value from a v2 fetch / ls-refs /
// bundle-uri request message. The message is the bytes between two
// flushes (callers typically pass the buffer returned by
// ReadFlushTerminatedMessage). Returns "" when the message is
// malformed, truncated, or carries no command pktline.
func V2Command(message []byte) string {
	for len(message) > 0 {
		if len(message) < pktline.LenSize {
			return ""
		}
		pktLen, err := pktline.ParseLength(message[:pktline.LenSize])
		if err != nil {
			return ""
		}
		switch pktLen {
		case pktline.Flush:
			return ""
		case pktline.Delim, pktline.ResponseEnd:
			message = message[pktline.LenSize:]
			continue
		}
		if pktLen < pktline.LenSize || len(message) < pktLen {
			return ""
		}
		line := string(message[pktline.LenSize:pktLen])
		if after, ok := strings.CutPrefix(line, "command="); ok {
			return strings.TrimSuffix(after, "\n")
		}
		message = message[pktLen:]
	}
	return ""
}

// ReadFlushTerminatedMessage reads pkt-lines from r until a flush
// (0000), returning the concatenated raw bytes including every length
// prefix and the trailing flush. The boolean is false (with nil
// error) when r is at EOF before any byte has been read — the
// natural signal that the stream closed cleanly between messages.
//
// Special lengths 0001 (delim) and 0002 (response-end) are passed
// through verbatim — they're part of v2 framing.
//
// We reach for the raw bytes instead of pktline.Scanner because the
// helper proxies messages over HTTP unchanged; reconstructing the
// length prefixes from Scanner's payload-only output would re-encode
// and risk drift.
func ReadFlushTerminatedMessage(r io.Reader) ([]byte, bool, error) {
	var buf bytes.Buffer
	lenBuf := make([]byte, pktline.LenSize)
	for {
		n, err := io.ReadFull(r, lenBuf)
		if err != nil {
			if errors.Is(err, io.EOF) && n == 0 && buf.Len() == 0 {
				return nil, false, nil
			}
			if errors.Is(err, io.ErrUnexpectedEOF) || (errors.Is(err, io.EOF) && n > 0) {
				return nil, false, fmt.Errorf("reading pkt-line length: %w", io.ErrUnexpectedEOF)
			}
			return nil, false, fmt.Errorf("reading pkt-line length: %w", err)
		}
		buf.Write(lenBuf)

		pktLen, err := pktline.ParseLength(lenBuf)
		if err != nil {
			return nil, false, err //nolint:wrapcheck // pktline.ParseLength already returns a typed error
		}
		switch pktLen {
		case pktline.Flush:
			return buf.Bytes(), true, nil
		case pktline.Delim, pktline.ResponseEnd:
			continue
		}
		if pktLen < pktline.LenSize {
			return nil, false, fmt.Errorf("invalid pkt-line length %d", pktLen)
		}

		payload := make([]byte, pktLen-pktline.LenSize)
		if _, err := io.ReadFull(r, payload); err != nil {
			return nil, false, fmt.Errorf("reading pkt-line content: %w", err)
		}
		buf.Write(payload)
	}
}

// ReadPostServiceAdvertisement reads a v0/v1 smart-HTTP info/refs body
// and returns the bytes that follow the service announcement plus
// its terminating flush — i.e. just the ref advertisement.
//
// `git send-pack --stateless-rpc` runs the input through
// discover_version + get_remote_heads, which die with "protocol
// error: unexpected '# service=...'" if the announcement preamble is
// left in. Strip it once, here, so callers can feed the result
// straight into send-pack.
func ReadPostServiceAdvertisement(r io.Reader) ([]byte, error) {
	br := bufio.NewReader(r)
	var reply packp.SmartReply
	if err := reply.Decode(br); err != nil {
		return nil, fmt.Errorf("decoding smart-HTTP service announcement: %w", err)
	}
	body, err := io.ReadAll(br)
	if err != nil {
		return nil, fmt.Errorf("reading ref advertisement: %w", err)
	}
	return body, nil
}

// ReadReceivePackRequest reads a v0/v1 git-receive-pack request body
// from r: a sequence of command pkt-lines terminated by a flush,
// optionally followed by a raw PACK stream when at least one command
// is not a delete (i.e. the new OID isn't all zeros).
//
// The returned bytes are exactly what was on the wire — caller
// streams them to the remote without further framing. EOF before the
// flush is reported (treating it as a clean close would let a
// truncated request look like a clean delete-only push).
func ReadReceivePackRequest(r io.Reader) ([]byte, error) {
	var buf bytes.Buffer
	lenBuf := make([]byte, pktline.LenSize)
	needPackData := false

	for {
		n, err := io.ReadFull(r, lenBuf)
		if err != nil {
			if errors.Is(err, io.EOF) && n == 0 && buf.Len() == 0 {
				return buf.Bytes(), nil
			}
			if errors.Is(err, io.ErrUnexpectedEOF) || (errors.Is(err, io.EOF) && n > 0) {
				return nil, fmt.Errorf("reading pkt-line length: %w", io.ErrUnexpectedEOF)
			}
			return nil, fmt.Errorf("reading pkt-line length: %w", err)
		}

		pktLen, err := pktline.ParseLength(lenBuf)
		if err != nil {
			return nil, fmt.Errorf("receive-pack request: %w", err)
		}
		buf.Write(lenBuf)

		if pktLen == pktline.Flush {
			if needPackData {
				if _, err := io.Copy(&buf, r); err != nil {
					return nil, fmt.Errorf("reading pack data: %w", err)
				}
			}
			return buf.Bytes(), nil
		}
		if pktLen < pktline.LenSize {
			continue
		}

		content := make([]byte, pktLen-pktline.LenSize)
		if _, err := io.ReadFull(r, content); err != nil {
			return nil, fmt.Errorf("reading pkt-line content: %w", err)
		}
		buf.Write(content)

		if !needPackData && receivePackCommandNeedsPack(content) {
			needPackData = true
		}
	}
}

// receivePackCommandNeedsPack reports whether a receive-pack command
// pkt-line describes a non-delete update (new OID not all zeros).
// Wire shape: "<old-id> <new-id> <ref>[\0<capabilities>]\n". Works
// for both SHA-1 (40-hex) and SHA-256 (64-hex) object IDs.
func receivePackCommandNeedsPack(content []byte) bool {
	line := content
	if i := bytes.IndexByte(line, 0); i >= 0 {
		line = line[:i]
	} else {
		line = bytes.TrimSuffix(line, []byte("\n"))
	}
	_, rest, ok := bytes.Cut(line, []byte{' '})
	if !ok {
		return false
	}
	newOID, _, ok := bytes.Cut(rest, []byte{' '})
	if !ok || len(newOID) == 0 {
		return false
	}
	for _, b := range newOID {
		if b != '0' {
			return true
		}
	}
	return false
}

// ReadSendPackRequest reads one request emitted by
// `git send-pack --stateless-rpc` and writes the unwrapped payload to
// dst. Send-pack wraps the receive-pack request in an outer pkt-line
// stream so it can mark request boundaries without closing stdout;
// the HTTP body is the concatenated payload of those outer packets.
//
// Returns when the outer flush is seen. Special lengths 1/2 are
// tolerated and skipped (current send-pack doesn't emit them, but
// the shape is permissive). EOF before the flush is wrapped as
// io.ErrUnexpectedEOF — a truncated send-pack stdout is never a
// clean end.
func ReadSendPackRequest(r *bufio.Reader, dst *bytes.Buffer) error {
	for {
		lenBuf := make([]byte, pktline.LenSize)
		n, err := io.ReadFull(r, lenBuf)
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				_ = n
				return fmt.Errorf("reading send-pack packet length: %w", io.ErrUnexpectedEOF)
			}
			return fmt.Errorf("reading send-pack packet length: %w", err)
		}
		pktLen, err := pktline.ParseLength(lenBuf)
		if err != nil {
			return fmt.Errorf("parsing send-pack packet length: %w", err)
		}
		if pktLen == pktline.Flush {
			return nil
		}
		if pktLen < pktline.LenSize {
			continue
		}

		payload := make([]byte, pktLen-pktline.LenSize)
		if _, err := io.ReadFull(r, payload); err != nil {
			return fmt.Errorf("reading send-pack packet payload: %w", err)
		}
		if _, err := dst.Write(payload); err != nil {
			return fmt.Errorf("buffering send-pack packet payload: %w", err)
		}
	}
}
