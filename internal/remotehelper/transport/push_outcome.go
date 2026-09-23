package transport

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
)

// A receive-pack POST can change refs, so failover resends it only when the
// earlier attempt cannot have applied. The helper accepts three kinds of
// evidence for that:
//
//   - The HTTP transport did not read the whole request body. The server
//     then never had the complete command list and pack, so it could not
//     apply them.
//   - A node answered before it had the whole body, or answered with a
//     redirect or 401. Each is that node's final answer, and none applies
//     the push.
//   - entiredb marked a 5xx response with PushOutcomeHeader: not-applied.
//
// Any other failure after a complete upload is ambiguous. The helper then
// stops and returns a PushOutcomeUnknownError. Git's own report-status
// still decides per-ref results when a response arrives in full.
//
// These rules protect only this helper. Stock git and older helpers keep
// their own retry behavior; see entiredb ADR 20260923-native-push-proxy.

// PushOutcomeHeader is the response header entiredb sets on a receive-pack
// POST that it could not complete. It is a cross-repository contract with
// entiredb's server/githttp native push proxy.
const PushOutcomeHeader = "X-Entire-Push-Outcome"

const (
	// pushOutcomeIndeterminate means the upload reached a primary that
	// can have applied it before the connection failed.
	pushOutcomeIndeterminate = "indeterminate"
	// pushOutcomeNotApplied means entiredb refused the push before any
	// primary received its body.
	pushOutcomeNotApplied = "not-applied"
)

// ErrPushOutcomeUnknown matches every PushOutcomeUnknownError.
var ErrPushOutcomeUnknown = errors.New("push outcome unknown")

// pushRecoveryHint tells the user how to recover from an unknown outcome.
// A read can reach a replica that lags the primary, so it cannot prove
// that a push failed.
const pushRecoveryHint = "The server may have applied some or all of the ref updates. " +
	"The helper did not send the push again, because a resend could repeat an update that already landed.\n" +
	"Check the remote refs before you push again. A fetch can reach a replica that lags the primary, " +
	"so a ref that still shows its old value does not prove that the push failed."

// PushOutcomeUnknownError reports a receive-pack whose result the helper
// cannot know: the whole push can have reached the server, and no complete
// report-status came back.
type PushOutcomeUnknownError struct {
	Cause error
}

func (e *PushOutcomeUnknownError) Error() string {
	return fmt.Sprintf("%v: %v\n%s", ErrPushOutcomeUnknown, e.Cause, pushRecoveryHint)
}

func (e *PushOutcomeUnknownError) Unwrap() []error { return []error{ErrPushOutcomeUnknown, e.Cause} }

// PushOutcomeUnknown wraps cause as a PushOutcomeUnknownError. An error that
// already is one passes through unchanged.
func PushOutcomeUnknown(cause error) error {
	if errors.Is(cause, ErrPushOutcomeUnknown) {
		return cause
	}
	return &PushOutcomeUnknownError{Cause: cause}
}

// isReceivePackPost reports whether a request can change refs.
func isReceivePackPost(method, suffix string) bool {
	return method == http.MethodPost && strings.HasSuffix(suffix, "/git-receive-pack")
}

// uploadTracker follows a receive-pack body through every copy of it that
// net/http reads. net/http rewinds a body through GetBody only for a retry
// after nothing reached the wire, or to follow a redirect, which
// checkRedirect refuses for writes. The last copy's count therefore bounds
// what the server can have received.
type uploadTracker struct {
	src  io.ReaderAt
	size int64

	mu   sync.Mutex
	last *countingReader
}

func newUploadTracker(body io.ReadSeeker) (*uploadTracker, error) {
	if body == nil {
		return &uploadTracker{src: strings.NewReader("")}, nil
	}
	size, err := body.Seek(0, io.SeekEnd)
	if err != nil {
		return nil, fmt.Errorf("measuring request body: %w", err)
	}
	if _, err := body.Seek(0, io.SeekStart); err != nil {
		return nil, fmt.Errorf("resetting request body: %w", err)
	}
	src, ok := body.(io.ReaderAt)
	if !ok {
		// Every caller passes a *bytes.Reader. The copy keeps the tracker
		// correct for any other ReadSeeker.
		buf, err := io.ReadAll(body)
		if err != nil {
			return nil, fmt.Errorf("reading request body: %w", err)
		}
		src = strings.NewReader(string(buf))
	}
	return &uploadTracker{src: src, size: size}, nil
}

// open returns a fresh copy of the body and makes it the tracked one.
func (u *uploadTracker) open() io.ReadCloser {
	c := &countingReader{r: io.NewSectionReader(u.src, 0, u.size)}
	u.mu.Lock()
	u.last = c
	u.mu.Unlock()
	return io.NopCloser(c)
}

// attach sets req's body, length, and rewind hook from the tracker.
func (u *uploadTracker) attach(req *http.Request) {
	req.Body = u.open()
	req.ContentLength = u.size
	req.GetBody = func() (io.ReadCloser, error) { return u.open(), nil }
	if u.size == 0 {
		req.Body = http.NoBody
	}
}

// complete reports whether the transport has read the whole body, which is
// the precondition for the server to have received it.
func (u *uploadTracker) complete() bool {
	u.mu.Lock()
	last := u.last
	u.mu.Unlock()
	return last == nil || last.n.Load() >= u.size
}

type countingReader struct {
	r io.Reader
	n atomic.Int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n.Add(int64(n))
	//nolint:wrapcheck // passthrough reader; wrapping would hide io.EOF
	return n, err
}

// writeTransportError classifies a failed receive-pack round trip. It
// returns nil when failover may resend the push.
func writeTransportError(node string, upload *uploadTracker, err error) error {
	if upload.complete() {
		return PushOutcomeUnknown(fmt.Errorf("node %s: connection failed after the whole push was sent: %w", node, err))
	}
	return nil
}

// writeResponseError classifies a receive-pack response from node. It
// returns nil when the response is a final answer the caller can hand to
// git. failover reports whether another node may receive the push instead.
// A non-nil error without failover ends the push.
//
// uploadComplete is sampled when the response arrived: a node that
// answered before it had the whole body cannot have applied it.
func writeResponseError(node string, resp *http.Response, uploadComplete bool) (failover bool, err error) {
	outcome := strings.ToLower(strings.TrimSpace(resp.Header.Get(PushOutcomeHeader)))
	if outcome == pushOutcomeIndeterminate {
		return false, PushOutcomeUnknown(fmt.Errorf("node %s: HTTP %d: %s", node, resp.StatusCode, readErrorBody(resp)))
	}
	switch {
	case isRedirect(resp.StatusCode):
		// The Location can embed a bearer as URL userinfo, so it stays out
		// of the message.
		_ = resp.Body.Close()
		return false, fmt.Errorf("node %s answered the push with HTTP %d redirect; "+
			"the helper does not follow redirects for a push, and the push was not applied", node, resp.StatusCode)
	case !shouldFailover(resp.StatusCode):
		return false, nil
	case outcome == pushOutcomeNotApplied || !uploadComplete:
		return true, nil
	default:
		return false, PushOutcomeUnknown(fmt.Errorf("node %s: HTTP %d after the whole push was sent: %s",
			node, resp.StatusCode, readErrorBody(resp)))
	}
}

// readErrorBody reads and closes a bounded error body for a message.
func readErrorBody(resp *http.Response) string {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024)) //nolint:errcheck // best-effort body read for error message
	_ = resp.Body.Close()
	msg := strings.TrimSpace(string(body))
	if strings.HasPrefix(msg, "<") {
		return ""
	}
	return msg
}

func outcomeIndeterminate(resp *http.Response) bool {
	return strings.EqualFold(strings.TrimSpace(resp.Header.Get(PushOutcomeHeader)), pushOutcomeIndeterminate)
}
