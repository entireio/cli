package githelper

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/go-git/go-git/v6/plumbing/format/pktline"
	"github.com/go-git/go-git/v6/plumbing/protocol/packp"

	"github.com/entireio/cli/internal/remotehelper/debuglog"
	"github.com/entireio/cli/internal/remotehelper/gitproto"
)

// handleConnect handles a connect session for the given service.
func handleConnect(ctx context.Context, t Transport, service string, stdin io.Reader, stdout io.Writer) error {
	refs, err := t.InfoRefs(ctx, service)
	if err != nil {
		return fmt.Errorf("connect %s info/refs: %w", service, err)
	}
	defer refs.Close()

	refReader := bufio.NewReader(refs)
	var reply packp.SmartReply
	if err := reply.Decode(refReader); err != nil {
		return fmt.Errorf("parsing info/refs: %w", err)
	}

	if _, err := io.Copy(stdout, refReader); err != nil {
		return fmt.Errorf("streaming refs: %w", err)
	}

	switch service {
	case serviceUploadPack:
		return handleFetch(ctx, t, stdin, stdout)
	case serviceReceivePack:
		// fall through
	default:
		return fmt.Errorf("unsupported service: %s", service)
	}

	reqBody, err := gitproto.ReadReceivePackRequest(stdin)
	if err != nil {
		return fmt.Errorf("reading git request: %w", err)
	}
	if err := gitproto.AppendAgentToReceivePackBody(reqBody, Agent); err != nil {
		return fmt.Errorf("amending receive-pack agent: %w", err)
	}
	debuglog.Printf("git request: %d bytes", reqBody.Len())

	if reqBody.Len() == 0 {
		return nil
	}

	resp, err := t.ServiceRPC(ctx, service, reqBody.Reader(), receivePackHeaders(reqBody))
	if err != nil {
		return fmt.Errorf("connect %s POST: %w", service, err)
	}
	defer resp.Close()

	if _, err := io.Copy(stdout, resp); err != nil {
		return fmt.Errorf("streaming response: %w", err)
	}

	return nil
}

// handleFetch handles upload-pack negotiation for connect mode by
// translating canonical git's stateful conversation into HTTP
// stateless-RPC requests against entire-server.
//
// Canonical git in connect mode acts as if it has a bidirectional
// pipe to git-upload-pack: wants + flush, then alternating have
// batches and server ACKs, terminated by done. HTTP smart-git is
// stateless: each POST to /git-upload-pack is independent and must
// carry the full wants list plus every have the client wants the
// server to consider.
//
// We bridge by buffering wants permanently, accumulating haves across
// rounds, and issuing one POST per have-batch flush. The server's
// real ACK/NAK response streams back to git, so git's MAX_IN_VAIN
// terminates on the first common ancestor instead of walking the
// entire rev queue. When git sends done, a final POST with the
// accumulated wants + haves + done streams the pack response.
//
// Shallow fetches add a probe round: if the wants section contains a
// `deepen…` line, canonical git parks waiting for the server's
// shallow/unshallow update before writing any haves. We fire an extra
// POST on the wants-flush carrying only the buffered wants, stream the
// shallow-update response to git, then resume the normal have/done
// flow. The wants buffer is preserved (every subsequent stateless-RPC
// round must repeat the wants block including the deepen lines, or the
// server treats the request as non-shallow).
func handleFetch(ctx context.Context, t Transport, stdin io.Reader, stdout io.Writer) error {
	var wantsBuf, havesBuf bytes.Buffer
	lenBuf := make([]byte, pktline.LenSize)
	pastWants := false
	deepenSeen := false
	roundNumber := 0
	var acks ackState

	for {
		_, err := io.ReadFull(stdin, lenBuf)
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				// Git closed stdin before done. Either it aborted or
				// (more commonly) decided no fetch was needed before
				// ever sending wants. Either way, exit cleanly.
				return nil
			}
			return fmt.Errorf("reading pkt-line length: %w", err)
		}

		pktLen, err := pktline.ParseLength(lenBuf)
		if err != nil {
			return fmt.Errorf("connect fetch: %w", err)
		}

		if pktLen == pktline.Flush {
			if !pastWants {
				// End of wants section. Include the flush in the wants
				// buffer (every request body needs it as the wants
				// terminator) and switch to accumulating haves.
				wantsBuf.Write(lenBuf)
				pastWants = true
				debuglog.Printf("end of wants (%d bytes)", wantsBuf.Len())
				if deepenSeen {
					if err := postShallowProbe(ctx, t, stdout, &wantsBuf); err != nil {
						return err
					}
				}
				continue
			}
			// End of a have batch. POST wants + cumulative haves +
			// terminating flush so the server can compute ACKs.
			roundNumber++
			if err := postRound(ctx, t, stdout, &wantsBuf, &havesBuf, &acks, false, deepenSeen, roundNumber); err != nil {
				return err
			}
			continue
		}

		if pktLen < pktline.LenSize {
			// Special pktlines (0001 delim, 0002 response-end) — not
			// expected in v0/v1 client→server stream, but tolerate by
			// pass-through to the appropriate buffer.
			dest := &wantsBuf
			if pastWants {
				dest = &havesBuf
			}
			dest.Write(lenBuf)
			continue
		}

		content := make([]byte, pktLen-pktline.LenSize)
		if _, err := io.ReadFull(stdin, content); err != nil {
			return fmt.Errorf("reading pkt-line content: %w", err)
		}

		// `deepen`, `deepen-since`, `deepen-not` in the wants section
		// arm the probe POST on the upcoming wants-flush. `shallow X`
		// from the client is purely informational (client telling the
		// server about its existing shallow boundary) and does NOT
		// trigger a server shallow-update unless paired with deepen,
		// so it doesn't need a probe round.
		if !pastWants && bytes.HasPrefix(content, []byte("deepen")) {
			deepenSeen = true
		}

		dest := &wantsBuf
		if pastWants {
			dest = &havesBuf
		}
		dest.Write(lenBuf)
		dest.Write(content)

		if strings.TrimSpace(string(content)) == "done" {
			// Final round: server returns ACK/NAK + pack data.
			roundNumber++
			debuglog.Printf("done line received, sending final request (round %d)", roundNumber)
			return postRound(ctx, t, stdout, &wantsBuf, &havesBuf, &acks, true, deepenSeen, roundNumber)
		}
	}
}

// postShallowProbe issues the shallow-update probe POST that canonical
// git in connect mode is waiting for after writing `wants + deepen +
// flush`. The body is exactly the buffered wants (already terminated
// by the wants-flush); the server response is `shallow…/unshallow…
// + flush` and is streamed verbatim to git's stdout. No synthetic NAK
// is appended — unlike intermediate have-batch rounds, the probe
// response IS the round terminator (its trailing flush) that git is
// waiting for, and appending NAK would confuse the client into
// thinking negotiation has already happened.
func postShallowProbe(ctx context.Context, t Transport, stdout io.Writer, wantsBuf *bytes.Buffer) error {
	body, err := gitproto.AppendAgentToUploadPackRequest(append([]byte(nil), wantsBuf.Bytes()...), Agent)
	if err != nil {
		return fmt.Errorf("amending upload-pack agent: %w", err)
	}
	debuglog.Printf("posting shallow-update probe: %d bytes", len(body))
	resp, err := t.ServiceRPC(ctx, serviceUploadPack, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("shallow-update probe POST: %w", err)
	}
	defer resp.Close()
	if _, err := io.Copy(stdout, resp); err != nil {
		return fmt.Errorf("streaming shallow-update response: %w", err)
	}
	return nil
}

// postRound assembles one stateless-RPC request body from the
// buffered wants and cumulative haves, posts it to /git-upload-pack,
// and streams the response back to git on stdout. Canonical git in
// connect (stateful) mode reads each non-final round until NAK. Older
// servers end such a round without one, so postRound adds it then. A
// second NAK would end the next round early and leave ACK/NAK lines
// in front of the pack ("protocol error: bad band").
//
// When stripShallow is true, the helper consumes the duplicate
// shallow/unshallow + flush prefix the server emits on every round
// that carries deepen in its body (HTTP stateless-RPC processes each
// POST independently — every response begins with the boundary
// section). Canonical git in connect mode only consumes the FIRST
// shallow response (fetch-pack.c:453-481); subsequent rounds enter
// the ACK/NAK loop directly, where a leftover "shallow X" pktline
// kills get_ack (fetch-pack.c:253: `expected ACK/NAK, got '%s'`).
func postRound(ctx context.Context, t Transport, stdout io.Writer, wantsBuf, havesBuf *bytes.Buffer, acks *ackState, final, stripShallow bool, roundNumber int) error {
	body := make([]byte, 0, wantsBuf.Len()+havesBuf.Len()+pktline.LenSize)
	body = append(body, wantsBuf.Bytes()...)
	body = append(body, havesBuf.Bytes()...)
	if !final {
		body = append(body, "0000"...)
	}
	debuglog.Printf("posting round %d: %d bytes (final=%v)", roundNumber, len(body), final)
	body, err := gitproto.AppendAgentToUploadPackRequest(body, Agent)
	if err != nil {
		return fmt.Errorf("amending upload-pack agent: %w", err)
	}

	resp, err := t.ServiceRPC(ctx, serviceUploadPack, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("connect fetch round %d POST: %w", roundNumber, err)
	}
	defer resp.Close()

	var src io.Reader = resp
	if stripShallow {
		src, err = drainShallowUpdate(resp)
		if err != nil {
			return fmt.Errorf("draining shallow-update prefix on round %d: %w", roundNumber, err)
		}
	}

	if !wantsMultiACK(wantsBuf.Bytes()) {
		if err := relaySingleACKRound(stdout, src, acks, final); err != nil {
			return fmt.Errorf("streaming round %d response: %w", roundNumber, err)
		}
		return nil
	}

	var tail roundTail
	if _, err := io.Copy(io.MultiWriter(stdout, &tail), src); err != nil {
		return fmt.Errorf("streaming round %d response: %w", roundNumber, err)
	}
	if !final && !tail.endsWithNAK() {
		if _, err := stdout.Write([]byte(nakPkt)); err != nil {
			return fmt.Errorf("writing NAK terminator for round %d: %w", roundNumber, err)
		}
	}
	return nil
}

const nakPkt = "0008NAK\n"

// packHeader starts a pack file, which no pkt-line length can.
const packHeader = "PACK"

// roundTail keeps the last bytes of a streamed round response. The server
// sets the response size, so the helper does not buffer the round.
type roundTail struct {
	b [len(nakPkt)]byte
	n int
}

func (t *roundTail) Write(p []byte) (int, error) {
	if len(p) >= len(t.b) {
		copy(t.b[:], p[len(p)-len(t.b):])
		t.n = len(t.b)
		return len(p), nil
	}
	keep := min(t.n, len(t.b)-len(p))
	copy(t.b[:keep], t.b[t.n-keep:t.n])
	copy(t.b[keep:], p)
	t.n = keep + len(p)
	return len(p), nil
}

// endsWithNAK reports whether the round ended with a NAK pkt-line. No ACK
// line can end with these bytes.
func (t *roundTail) endsWithNAK() bool {
	return string(t.b[:t.n]) == nakPkt
}

// ackState carries what git has seen across the rounds of one fetch.
type ackState struct {
	// acked is set once git has received the ACK of a single-ACK fetch.
	acked bool
}

// wantsMultiACK reports whether git asked for multi_ack or
// multi_ack_detailed on its first want line.
func wantsMultiACK(wants []byte) bool {
	_, payload, err := pktline.ReadLine(bytes.NewReader(wants))
	if err != nil {
		return false
	}
	for _, f := range bytes.Fields(payload) {
		if string(f) == "multi_ack" || string(f) == "multi_ack_detailed" {
			return true
		}
	}
	return false
}

// relaySingleACKRound relays a round of a fetch without multi_ack. Git's
// upload-pack then answers each round with NAK until the first common
// have, ACKs that have once, and sends nothing more before the pack
// (upload-pack.c, get_common_commits). Every request repeats all haves,
// so the server ACKs the common have again in later rounds. Git reads
// those repeated ACKs as pack data, so they are dropped here.
func relaySingleACKRound(w io.Writer, r io.Reader, acks *ackState, final bool) error {
	// PeekLine peeks a whole pkt-line, and side-band-64k pack packets
	// reach pktline.MaxSize.
	br := bufio.NewReaderSize(r, pktline.MaxSize)
	for {
		l, payload, done, err := peekRoundLine(br, final)
		if err != nil {
			return err
		}
		if done {
			break
		}
		line := string(bytes.TrimSuffix(payload, []byte("\n")))
		isACK := strings.HasPrefix(line, "ACK ")
		if final && !isACK {
			// The pack follows the negotiation lines.
			break
		}
		n := max(l, pktline.LenSize)
		raw, err := br.Peek(n)
		if err != nil {
			return fmt.Errorf("reading pkt-line: %w", err)
		}
		switch {
		case isACK && acks.acked:
		case isACK:
			if _, err := w.Write(raw); err != nil {
				return fmt.Errorf("writing ACK: %w", err)
			}
			acks.acked = true
		case line == "NAK":
		default:
			if _, err := w.Write(raw); err != nil {
				return fmt.Errorf("writing pkt-line: %w", err)
			}
		}
		if _, err := br.Discard(n); err != nil {
			return fmt.Errorf("discarding pkt-line: %w", err)
		}
	}
	if _, err := io.Copy(w, br); err != nil {
		return fmt.Errorf("copying response: %w", err)
	}
	if !final && !acks.acked {
		if _, err := w.Write([]byte(nakPkt)); err != nil {
			return fmt.Errorf("writing NAK: %w", err)
		}
	}
	return nil
}

// peekRoundLine peeks the next pkt-line of a round response. done reports
// that no negotiation line follows: the response ended, the server sent ERR
// (passed on to git, which reports it and stops), or, in the done round, the
// pack starts raw, as it does without side-band (its header is no pkt-line).
func peekRoundLine(br *bufio.Reader, final bool) (l int, payload []byte, done bool, err error) {
	if final {
		if header, peekErr := br.Peek(len(packHeader)); peekErr == nil && string(header) == packHeader {
			return 0, nil, true, nil
		}
	}
	l, payload, err = pktline.PeekLine(br)
	var errLine *pktline.ErrorLine
	switch {
	case errors.Is(err, io.EOF), errors.As(err, &errLine):
		return 0, nil, true, nil
	case err != nil:
		return 0, nil, false, fmt.Errorf("peeking pkt-line: %w", err)
	}
	return l, payload, false, nil
}

// drainShallowUpdate consumes a leading shallow/unshallow pktline run
// + the terminating flush from r and returns the trimmed reader. It is
// the stream-mirror of fetch-pack.c:205-223 `consume_shallow_list`,
// applied to intermediate AND final rounds in our connect-mode bridge
// because each have-round POST repeats `deepen` and the server
// dutifully re-emits the boundary section.
//
// If the first pktline isn't shallow/unshallow, returns r untouched —
// safe to call defensively on every shallow round even when the
// server (e.g. a probe-only response) happens to have no
// shallow-update prefix.
func drainShallowUpdate(r io.Reader) (io.Reader, error) {
	br := bufio.NewReader(r)
	for {
		l, payload, err := pktline.PeekLine(br)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return br, nil
			}
			return nil, fmt.Errorf("peeking shallow-update pktline: %w", err)
		}
		if l == pktline.Flush {
			if _, err := br.Discard(pktline.LenSize); err != nil {
				return nil, fmt.Errorf("discarding shallow-update flush: %w", err)
			}
			return br, nil
		}
		if !bytes.HasPrefix(payload, []byte("shallow ")) &&
			!bytes.HasPrefix(payload, []byte("unshallow ")) {
			return br, nil
		}
		if _, err := br.Discard(l); err != nil {
			return nil, fmt.Errorf("discarding shallow pktline: %w", err)
		}
	}
}
