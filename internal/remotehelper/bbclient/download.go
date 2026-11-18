package bbclient

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/jmwoliver/xet-go/xethash"
	"github.com/jmwoliver/xet-go/xorb"
	"golang.org/x/sync/errgroup"

	"github.com/entireio/cli/internal/remotehelper/bbchunk"
)

// Download limits from the fetch spec.
const (
	// MaxDownloadsInFlight is the number of downloads that run at once. The
	// spec allows 4 and asks for 2: a node serves 4 downloads for one
	// principal and 16 in all.
	MaxDownloadsInFlight = 2
	// downloadAttempts is the number of tries of one xorb. A busy answer
	// does not use a try.
	downloadAttempts = 3
	// maxBusy bounds the time from the first request for a xorb in which
	// busy answers make the download wait and send again.
	maxBusy = 5 * time.Minute
	// maxBusyJitter bounds the random time added to Retry-After.
	maxBusyJitter = time.Second
)

// XorbSink holds downloaded xorbs. *bbcache.Store implements it.
type XorbSink interface {
	// HasComplete reports whether the sink holds the complete xorb h.
	HasComplete(h xethash.Hash) bool
	// PutComplete calls fill with a writer for the complete xorb h, and
	// keeps the bytes only when fill returns no error. It returns the error
	// from fill wrapped.
	PutComplete(h xethash.Hash, fill func(io.Writer) (*bbchunk.Footer, error)) error
}

// errBadDownload marks a download answer whose body failed a check or broke
// off. Another try may succeed.
var errBadDownload = errors.New("bad download")

// sinkError is a failure to write to the sink. The fault is local, so the
// download does not try again.
type sinkError struct{ err error }

func (e *sinkError) Error() string { return "write to the local store: " + e.err.Error() }

func (e *sinkError) Unwrap() error { return e.err }

// Download downloads the complete xorb h into sink, whether or not sink
// holds it. It checks every chunk, the footer and the length before sink
// keeps the bytes.
//
// It follows the retry rules of the fetch spec. A try is one request. Each
// xorb has at most 3 tries; a transport failure, a body that breaks off or
// fails a check, and a 503 without Retry-After use one, and the next try
// waits a backoff with jitter. A 503 with Retry-After is a busy node: it
// uses no try, and the next request waits for Retry-After, at least the
// first backoff, and up to 1 second of jitter, for at most 5 minutes from
// the first request. The floor keeps a "Retry-After: 0" from turning the
// busy wait into a tight loop of requests. Any other answer, 500 included,
// is a *StatusError at once: a 500 means that the stored xorb is corrupt.
func (c *Client) Download(ctx context.Context, h xethash.Hash, sink XorbSink) error {
	start := c.now()
	tries := 0
	for {
		err := c.downloadOnce(ctx, h, sink)
		if err == nil {
			return nil
		}
		var wait time.Duration
		switch busy, retry := downloadRetry(ctx, err); {
		case !retry:
			return downloadError(h, err)
		case busy >= 0:
			wait = max(min(busy, c.maxRetryAfter), c.backoff) + rand.N(maxBusyJitter) //nolint:gosec // jitter, not security
			if c.now().Add(wait).Sub(start) > maxBusy {
				return fmt.Errorf("%w (busy for %s)", downloadError(h, err), c.now().Sub(start).Round(time.Second))
			}
		default:
			tries++
			if tries == downloadAttempts {
				return fmt.Errorf("%w (gave up after %d attempts)", downloadError(h, err), downloadAttempts)
			}
			wait = c.backoffFor(tries)
		}
		if err := c.sleep(ctx, wait); err != nil {
			return err
		}
	}
}

// downloadError names the xorb of a failed download, unless err does: a
// *StatusError names the request path, which holds the hash.
func downloadError(h xethash.Hash, err error) error {
	var se *StatusError
	if errors.As(err, &se) {
		return fmt.Errorf("bbclient: download: %w", err)
	}
	return fmt.Errorf("bbclient: download xorb %s: %w", h, err)
}

// downloadRetry reports whether a failed download may try again. busy is
// the Retry-After of a 503 that has one, which uses no try, or -1.
func downloadRetry(ctx context.Context, err error) (busy time.Duration, retry bool) {
	var local *sinkError
	if ctx.Err() != nil || errors.As(err, &local) {
		return -1, false
	}
	var se *StatusError
	if errors.As(err, &se) {
		var r *retryableError
		if se.StatusCode != http.StatusServiceUnavailable || !errors.As(err, &r) {
			return -1, false
		}
		return r.wait, true
	}
	return -1, errors.Is(err, errBadDownload) || retryableTransport(ctx, err)
}

func (c *Client) downloadOnce(ctx context.Context, h xethash.Hash, sink XorbSink) error {
	suffix := "/bb/xorbs/" + h.String()
	resp, err := c.doer.Do(ctx, http.MethodGet, suffix, nil, func(r *http.Request) {
		r.Header.Set("Accept", "application/octet-stream")
	})
	if err != nil {
		return err //nolint:wrapcheck // Do names the request
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return statusError(resp, http.MethodGet, suffix)
	}
	if resp.ContentLength > xorb.MaxCompleteSize {
		return fmt.Errorf("answer of %d bytes, over the %d-byte limit", resp.ContentLength, xorb.MaxCompleteSize)
	}
	if err := checkETag(resp.Header.Get("ETag"), h); err != nil {
		return err
	}
	body := &bodyReader{r: resp.Body}
	err = sink.PutComplete(h, func(w io.Writer) (*bbchunk.Footer, error) {
		f, err := bbchunk.CheckComplete(io.TeeReader(body, &sinkWriter{w: w}), h, resp.ContentLength)
		if errors.Is(err, bbchunk.ErrMalformedXorb) {
			err = fmt.Errorf("%w: %w", errBadDownload, err)
		}
		return f, err
	})
	if err != nil {
		return err //nolint:wrapcheck // PutComplete names the xorb
	}
	return nil
}

// checkETag checks an ETag header, when the answer has one, against h. The
// server sends the strong ETag "<xorb hex>".
func checkETag(etag string, h xethash.Hash) error {
	if etag == "" {
		return nil
	}
	if strings.TrimPrefix(etag, "W/") != `"`+h.String()+`"` {
		return fmt.Errorf("%w: ETag %.80q names another xorb", errBadDownload, etag)
	}
	return nil
}

// bodyReader marks a failed read of the answer body as errBadDownload. A
// body shorter than Content-Length fails with io.ErrUnexpectedEOF.
type bodyReader struct{ r io.Reader }

func (b *bodyReader) Read(p []byte) (int, error) {
	n, err := b.r.Read(p)
	if err != nil && !errors.Is(err, io.EOF) {
		err = fmt.Errorf("%w: read body: %w", errBadDownload, err)
	}
	return n, err
}

// sinkWriter marks a failed write to the sink as a sinkError.
type sinkWriter struct{ w io.Writer }

func (s *sinkWriter) Write(p []byte) (int, error) {
	n, err := s.w.Write(p)
	if err != nil {
		return n, &sinkError{err: err}
	}
	return n, nil
}

// DownloadAll downloads each xorb of xorbs that sink does not hold, at most
// MaxDownloadsInFlight at once. xorbs may repeat a hash. progress, when it is
// not nil, gets the number of finished downloads and the number of all
// downloads, once before the first download and after each one; the calls do
// not overlap. The first failure cancels the other downloads, and DownloadAll
// returns it.
func (c *Client) DownloadAll(ctx context.Context, xorbs []xethash.Hash, sink XorbSink, progress func(done, total int)) error {
	seen := make(map[xethash.Hash]bool, len(xorbs))
	var missing []xethash.Hash
	for _, h := range xorbs {
		if seen[h] {
			continue
		}
		seen[h] = true
		if !sink.HasComplete(h) {
			missing = append(missing, h)
		}
	}
	var mu sync.Mutex
	done := 0
	report := func() {
		if progress != nil {
			progress(done, len(missing))
		}
	}
	report()
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(MaxDownloadsInFlight)
	for _, h := range missing {
		if gctx.Err() != nil {
			break
		}
		g.Go(func() error {
			if err := c.Download(gctx, h, sink); err != nil {
				return err
			}
			mu.Lock()
			defer mu.Unlock()
			done++
			report()
			return nil
		})
	}
	return g.Wait() //nolint:wrapcheck // Download names the xorb
}
