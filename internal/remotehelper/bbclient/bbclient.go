// Package bbclient talks to the BB endpoints of entiredb's client-chunked
// push and fetch: the xorb upload, the dedup query and the xorb download.
// entiredb's docs/specs/client-chunked-push.md and
// docs/specs/client-chunked-fetch.md are the contracts. The package checks
// every footer that the server returns against its xorb hash, so a recipe
// never names chunks that the server did not prove it holds.
package bbclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"strconv"
	"time"

	"github.com/jmwoliver/xet-go/xethash"
	"github.com/jmwoliver/xet-go/xorb"

	"github.com/entireio/cli/internal/entireclient/httpclient"
	"github.com/entireio/cli/internal/remotehelper/gitproto"
	"github.com/entireio/cli/internal/remotehelper/transport"
)

// Limits from the spec.
const (
	// MaxInFlight is the largest number of uploads that run at once. Each
	// upload holds node buffers on the server, so the spec asks for 2.
	MaxInFlight = 2
	// MaxDedupEntries is the largest number of anchors, and of xorbs, in
	// one dedup query.
	MaxDedupEntries = 4096
	// maxDedupFooters is the largest number of footers in one dedup answer.
	maxDedupFooters = 64
)

// Response size bounds. An upload answer holds one footer. A dedup answer
// holds at most maxDedupFooters footers and one entry per requested anchor
// and xorb, about 66 MiB in all. The client reads an answer in full before
// it checks the footer count, so the bound limits the memory that one
// answer can take.
const (
	// maxFooterChunkJSON bounds one footer chunk and its comma. The largest
	// is 122 bytes:
	// {"hash":"<64 hex>","unpacked_end":<8 digits>,"stored_end":<8 digits>},
	maxFooterChunkJSON = 128
	// maxFooterJSON bounds one footer and its key, about 1 MiB.
	maxFooterJSON = xorb.MaxChunks*maxFooterChunkJSON + 128
	// maxDedupEntryJSON bounds one anchor hit (164 bytes) or one xorb state
	// (96 bytes) and its comma.
	maxDedupEntryJSON = 192

	maxUploadResponse = 4 << 20
	maxDedupResponse  = maxDedupFooters*maxFooterJSON + 2*MaxDedupEntries*maxDedupEntryJSON + 4<<10
	maxErrorMessage   = 1 << 10
)

// Retry policy defaults.
const (
	defaultAttempts    = 6
	defaultBackoff     = 500 * time.Millisecond
	defaultMaxBackoff  = 30 * time.Second
	defaultMaxRetryAft = 60 * time.Second
)

// Doer sends a request to suffix under the repo path. *transport.Proxy
// implements it.
type Doer interface {
	Do(ctx context.Context, method, suffix string, body io.ReadSeeker, setHeaders func(*http.Request)) (*http.Response, error)
}

// State is the storage state of a xorb on the server.
type State string

// States of a xorb.
const (
	// StateStaged is a verified upload that a push has not used yet. The
	// server deletes it after about a day.
	StateStaged State = "staged"
	// StatePermanent is a xorb in the repo's store.
	StatePermanent State = "permanent"
	// StateMissing is a xorb that the server does not hold.
	StateMissing State = "missing"
)

// FooterChunk is one chunk of the footer of a stored xorb.
type FooterChunk struct {
	Hash xethash.Hash
	// UnpackedEnd is the cumulative unpacked length up to the end of the
	// chunk.
	UnpackedEnd uint64
	// StoredEnd is the cumulative stored length up to the end of the chunk,
	// with the 8-byte header of each chunk.
	StoredEnd uint64
}

// StatusError is an HTTP answer that the client does not retry, or the last
// answer after the retries ran out.
type StatusError struct {
	Method     string
	Suffix     string
	StatusCode int
	// Message is the start of the response body, sanitized for a
	// terminal: git shows it.
	Message string
}

func (e *StatusError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("%s %s: HTTP %d", e.Method, e.Suffix, e.StatusCode)
	}
	return fmt.Sprintf("%s %s: HTTP %d: %s", e.Method, e.Suffix, e.StatusCode, e.Message)
}

// Client calls the BB endpoints of one repo. It is safe for concurrent use
// when its Doer is.
type Client struct {
	doer          Doer
	attempts      int
	backoff       time.Duration
	maxBackoff    time.Duration
	maxRetryAfter time.Duration
	sleep         func(context.Context, time.Duration) error
	now           func() time.Time
}

// New returns a Client that sends its requests through d.
func New(d Doer) *Client {
	return &Client{
		doer:          d,
		attempts:      defaultAttempts,
		backoff:       defaultBackoff,
		maxBackoff:    defaultMaxBackoff,
		maxRetryAfter: defaultMaxRetryAft,
		sleep:         sleepContext,
		now:           time.Now,
	}
}

// do sends one request with retries and decodes a 2xx JSON answer into out.
// It retries a 5xx answer, a 429 or a transport failure: after the time that
// Retry-After asks for, else with exponential backoff. Every other answer is
// a StatusError at once. After the last attempt, the error wraps the last
// failure.
func (c *Client) do(ctx context.Context, method, suffix, contentType string, body []byte, maxResponse int64, out any) error {
	setHeaders := func(r *http.Request) {
		r.Header.Set("Content-Type", contentType)
		r.Header.Set("Accept", "application/json")
	}
	var lastErr error
	for attempt := range c.attempts {
		if attempt > 0 {
			wait := c.backoffFor(attempt)
			var retry *retryableError
			if errors.As(lastErr, &retry) && retry.wait >= 0 {
				wait = min(retry.wait, c.maxRetryAfter)
			}
			if err := c.sleep(ctx, wait); err != nil {
				return err
			}
		}
		resp, err := c.doer.Do(ctx, method, suffix, bytes.NewReader(body), setHeaders)
		if err != nil {
			if !retryableTransport(ctx, err) {
				return err //nolint:wrapcheck // Do names the request
			}
			lastErr = err
			continue
		}
		status, err := c.answer(resp, method, suffix, maxResponse, out)
		if err == nil || !retryableStatus(status) {
			return err
		}
		lastErr = err
	}
	return fmt.Errorf("gave up after %d attempts: %w", c.attempts, lastErr)
}

// retryableError is an HTTP answer that may succeed when sent again: a 5xx
// or a 429. wait is the delay that Retry-After asks for, or -1. It unwraps
// to its *StatusError.
type retryableError struct {
	*StatusError

	wait time.Duration
}

func (e *retryableError) Unwrap() error { return e.StatusError }

// answer reads resp and closes its body.
func (c *Client) answer(resp *http.Response, method, suffix string, maxResponse int64, out any) (int, error) {
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return resp.StatusCode, statusError(resp, method, suffix)
	}
	dec := json.NewDecoder(io.LimitReader(resp.Body, maxResponse))
	if err := dec.Decode(out); err != nil {
		return resp.StatusCode, fmt.Errorf("%s %s: decode response: %w", method, suffix, err)
	}
	return resp.StatusCode, nil
}

// statusError reads the start of the body of a refusal and returns it as a
// *StatusError, or as a *retryableError for a status that may succeed when
// sent again. The caller closes the body.
func statusError(resp *http.Response, method, suffix string) error {
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorMessage)) //nolint:errcheck // the message is best effort
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))    //nolint:errcheck // drain for connection reuse
	msg = gitproto.AppendSanitized(nil, bytes.TrimSpace(msg))
	se := &StatusError{Method: method, Suffix: suffix, StatusCode: resp.StatusCode, Message: string(msg)}
	if retryableStatus(resp.StatusCode) {
		return &retryableError{StatusError: se, wait: retryAfter(resp.Header.Get("Retry-After"))}
	}
	return se
}

// retryableStatus reports whether an answer may succeed when sent again.
// 400, 404, 413 and 422 are permanent for the BB endpoints, as are 401 and
// 403 after the transport's own retry.
func retryableStatus(status int) bool {
	return status == http.StatusTooManyRequests || status >= 500
}

// retryableTransport reports whether a transport failure may succeed when
// sent again. A refused redirect is a security decision, and a credential or
// request-building failure repeats. transport.ErrUnreachable means that the
// replica set has no node left to try. Only an info/refs request fills the
// set again, and it runs before the uploads, so a retry only delays the
// send-pack fallback.
func retryableTransport(ctx context.Context, err error) bool {
	if ctx.Err() != nil || errors.Is(err, httpclient.ErrRequestPolicy) || errors.Is(err, transport.ErrUnreachable) {
		return false
	}
	return errors.Is(err, transport.ErrNoNodeSucceeded)
}

// retryAfter parses a Retry-After value in seconds or as an HTTP date. It
// returns -1 for an absent or bad value.
func retryAfter(v string) time.Duration {
	if v == "" {
		return -1
	}
	if s, err := strconv.ParseUint(v, 10, 32); err == nil {
		return time.Duration(s) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		return max(time.Until(t), 0)
	}
	return -1
}

// backoffFor returns the delay before attempt (1-based retries), with up to
// 50% jitter.
func (c *Client) backoffFor(attempt int) time.Duration {
	d := c.backoff << min(attempt-1, 16)
	if d <= 0 || d > c.maxBackoff {
		d = c.maxBackoff
	}
	if d > 1 {
		d += rand.N(d / 2) //nolint:gosec // jitter, not security
	}
	return d
}

func sleepContext(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return fmt.Errorf("bbclient: wait to retry: %w", context.Cause(ctx))
	case <-t.C:
		return nil
	}
}

// textHash is a Xet hash in its JSON text form (xethash.Hash.String).
type textHash xethash.Hash

func (h textHash) MarshalText() ([]byte, error) { //nolint:unparam // encoding.TextMarshaler
	return []byte(xethash.Hash(h).String()), nil
}

func (h *textHash) UnmarshalText(b []byte) error {
	v, err := xethash.Parse(string(b))
	if err != nil {
		return fmt.Errorf("bbclient: %w", err)
	}
	*h = textHash(v)
	return nil
}

type wireChunk struct {
	Hash        textHash `json:"hash"`
	UnpackedEnd uint64   `json:"unpacked_end"`
	StoredEnd   uint64   `json:"stored_end"`
}

// parseFooter checks a footer from the server and returns it. The chunk
// hashes and unpacked sizes must hash to x: then the footer describes the
// xorb that the caller asked about, and a term over it is valid. The stored
// ends are not part of the hash, and a recipe does not use them, so
// parseFooter keeps them as the server sent them.
func parseFooter(x xethash.Hash, chunks []wireChunk) ([]FooterChunk, error) {
	if len(chunks) == 0 || len(chunks) > xorb.MaxChunks {
		return nil, fmt.Errorf("bbclient: footer of xorb %s has %d chunks, want 1 to %d", x, len(chunks), xorb.MaxChunks)
	}
	out := make([]FooterChunk, len(chunks))
	nodes := make([]xethash.Node, len(chunks))
	var unpacked uint64
	for i, c := range chunks {
		if c.UnpackedEnd <= unpacked || c.UnpackedEnd > xorb.MaxRawSize {
			return nil, fmt.Errorf("bbclient: footer of xorb %s: chunk %d ends at %d unpacked bytes", x, i, c.UnpackedEnd)
		}
		nodes[i] = xethash.Node{Hash: xethash.Hash(c.Hash), Size: c.UnpackedEnd - unpacked}
		out[i] = FooterChunk{Hash: xethash.Hash(c.Hash), UnpackedEnd: c.UnpackedEnd, StoredEnd: c.StoredEnd}
		unpacked = c.UnpackedEnd
	}
	if got := xethash.Xorb(nodes); got != x {
		return nil, fmt.Errorf("bbclient: footer of xorb %s hashes to %s", x, got)
	}
	return out, nil
}
