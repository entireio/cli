package bbclient

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jmwoliver/xet-go/xethash"
	"github.com/jmwoliver/xet-go/xorb"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/internal/remotehelper/bbchunk"
)

// memSink is an XorbSink in memory. It keeps the bytes that fill writes
// only when fill succeeds, as bbcache does.
type memSink struct {
	mu      sync.Mutex
	held    map[xethash.Hash][]byte
	footers map[xethash.Hash]*bbchunk.Footer
	// failWrite, when set, fails every write to the sink.
	failWrite error
}

func newMemSink() *memSink {
	return &memSink{held: map[xethash.Hash][]byte{}, footers: map[xethash.Hash]*bbchunk.Footer{}}
}

func (m *memSink) HasComplete(h xethash.Hash) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.held[h]
	return ok
}

type failingWriter struct{ err error }

func (f failingWriter) Write([]byte) (int, error) { return 0, f.err }

func (m *memSink) PutComplete(h xethash.Hash, fill func(io.Writer) (*bbchunk.Footer, error)) error {
	var buf bytes.Buffer
	var w io.Writer = &buf
	if m.failWrite != nil {
		w = failingWriter{err: m.failWrite}
	}
	f, err := fill(w)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.held[h] = buf.Bytes()
	m.footers[h] = f
	return nil
}

func (m *memSink) get(h xethash.Hash) []byte {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.held[h]
}

// downloadServer serves the complete form of the xorbs in xs as entiredb
// does: GET only, the hash in its text form, the permanent xorb or 404.
// before, when it is not nil, may answer a request first. It counts the
// requests.
func downloadServer(t *testing.T, xs []*bbchunk.Xorb, before func(w http.ResponseWriter, r *http.Request) bool) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	byHash := map[string]*bbchunk.Xorb{}
	for _, x := range xs {
		byHash[x.Hash.String()] = x
	}
	var requests atomic.Int32
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Header.Get("Range") != "" {
			t.Errorf("download sent Range %q", r.Header.Get("Range"))
		}
		if before != nil && before(w, r) {
			return
		}
		hash, ok := strings.CutPrefix(r.URL.Path, repoPath+"/bb/xorbs/")
		if !ok || r.Method != http.MethodGet {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if _, err := xethash.Parse(hash); err != nil {
			http.Error(w, "bad xorb hash", http.StatusBadRequest)
			return
		}
		x := byHash[hash]
		if x == nil {
			http.Error(w, "xorb not found", http.StatusNotFound)
			return
		}
		serveComplete(w, x.Hash, x.Complete())
	})), &requests
}

func serveComplete(w http.ResponseWriter, h xethash.Hash, body []byte) {
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.Header().Set("ETag", `"`+h.String()+`"`)
	w.Header().Set("Cache-Control", "private, max-age=31536000, immutable")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body) //nolint:errcheck // test
}

func TestDownload(t *testing.T) {
	t.Parallel()
	x := testXorb(t, 300)
	srv, requests := downloadServer(t, []*bbchunk.Xorb{x}, nil)
	defer srv.Close()
	c, slept := testClient(t, srv)
	sink := newMemSink()

	require.NoError(t, c.Download(t.Context(), x.Hash, sink))
	require.Equal(t, x.Complete(), sink.get(x.Hash))
	require.Equal(t, x.Footer, sink.footers[x.Hash])
	require.Equal(t, int32(1), requests.Load())
	require.Empty(t, *slept)
}

func TestDownloadPermanentErrors(t *testing.T) {
	t.Parallel()
	for _, status := range []int{http.StatusBadRequest, http.StatusNotFound, http.StatusForbidden,
		http.StatusTooManyRequests, http.StatusBadGateway} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			t.Parallel()
			x := testXorb(t, 301)
			srv, requests := downloadServer(t, nil, func(w http.ResponseWriter, _ *http.Request) bool {
				http.Error(w, "large objects are not enabled", status)
				return true
			})
			defer srv.Close()
			c, slept := testClient(t, srv)
			sink := newMemSink()

			err := c.Download(t.Context(), x.Hash, sink)
			var se *StatusError
			require.ErrorAs(t, err, &se)
			require.Equal(t, status, se.StatusCode)
			require.Contains(t, se.Message, "large objects are not enabled")
			require.Equal(t, int32(1), requests.Load())
			require.Empty(t, *slept)
			require.False(t, sink.HasComplete(x.Hash))
		})
	}
}

func TestDownloadNotFound(t *testing.T) {
	t.Parallel()
	x := testXorb(t, 302)
	srv, requests := downloadServer(t, nil, nil)
	defer srv.Close()
	c, _ := testClient(t, srv)

	err := c.Download(t.Context(), x.Hash, newMemSink())
	var se *StatusError
	require.ErrorAs(t, err, &se)
	require.Equal(t, http.StatusNotFound, se.StatusCode)
	require.Equal(t, 1, strings.Count(err.Error(), x.Hash.String()), "the error names the xorb once: %v", err)
	require.Equal(t, int32(1), requests.Load())
}

// The server's text reaches the terminal through git, so control bytes in
// it are sanitized.
func TestStatusErrorIsSanitized(t *testing.T) {
	t.Parallel()
	x := testXorb(t, 314)
	srv, _ := downloadServer(t, nil, func(w http.ResponseWriter, _ *http.Request) bool {
		http.Error(w, "not found\x1b]0;owned\x07", http.StatusNotFound)
		return true
	})
	defer srv.Close()
	c, _ := testClient(t, srv)

	err := c.Download(t.Context(), x.Hash, newMemSink())
	var se *StatusError
	require.ErrorAs(t, err, &se)
	require.Equal(t, "not found^[]0;owned^G", se.Message)
}

// A 500 means that the stored xorb is too large or corrupt. Another try
// gets the same answer.
func TestDownloadServerErrorIsFinal(t *testing.T) {
	t.Parallel()
	x := testXorb(t, 312)
	srv, requests := downloadServer(t, []*bbchunk.Xorb{x}, func(w http.ResponseWriter, _ *http.Request) bool {
		http.Error(w, "stored xorb is too large", http.StatusInternalServerError)
		return true
	})
	defer srv.Close()
	c, slept := testClient(t, srv)

	err := c.Download(t.Context(), x.Hash, newMemSink())
	var se *StatusError
	require.ErrorAs(t, err, &se)
	require.Equal(t, http.StatusInternalServerError, se.StatusCode)
	require.Equal(t, int32(1), requests.Load())
	require.Empty(t, *slept)
}

// A 503 with Retry-After does not use one of the 3 tries. The download
// waits for Retry-After and up to 1 second of jitter. A 503 without
// Retry-After uses a try and waits a backoff.
func TestDownloadWaitsForBusyAnswers(t *testing.T) {
	t.Parallel()
	x := testXorb(t, 303)
	var busy atomic.Int32
	srv, requests := downloadServer(t, []*bbchunk.Xorb{x}, func(w http.ResponseWriter, _ *http.Request) bool {
		switch n := busy.Add(1); {
		case n <= 4:
			w.Header().Set("Retry-After", "7")
		case n <= 6:
		default:
			return false
		}
		http.Error(w, "busy", http.StatusServiceUnavailable)
		return true
	})
	defer srv.Close()
	c, slept := testClient(t, srv)
	sink := newMemSink()

	require.NoError(t, c.Download(t.Context(), x.Hash, sink))
	require.Equal(t, x.Complete(), sink.get(x.Hash))
	require.Equal(t, int32(7), requests.Load())
	require.Len(t, *slept, 6)
	for i, d := range (*slept)[:4] {
		require.GreaterOrEqual(t, d, 7*time.Second, "wait %d", i)
		require.Less(t, d, 8*time.Second, "wait %d", i)
	}
	require.GreaterOrEqual(t, (*slept)[4], defaultBackoff)
	require.GreaterOrEqual(t, (*slept)[5], 2*defaultBackoff)
}

// "Retry-After: 0" still waits at least the first backoff. Without the
// floor, each busy answer would wait only the jitter, and a busy node would
// get a request about every half second for 5 minutes.
func TestDownloadBusyRetryAfterZeroWaitsBackoff(t *testing.T) {
	t.Parallel()
	x := testXorb(t, 314)
	var busy atomic.Int32
	srv, requests := downloadServer(t, []*bbchunk.Xorb{x}, func(w http.ResponseWriter, _ *http.Request) bool {
		if busy.Add(1) > 10 {
			return false
		}
		w.Header().Set("Retry-After", "0")
		http.Error(w, "busy", http.StatusServiceUnavailable)
		return true
	})
	defer srv.Close()
	c, slept := testClient(t, srv)
	sink := newMemSink()

	require.NoError(t, c.Download(t.Context(), x.Hash, sink))
	require.Equal(t, x.Complete(), sink.get(x.Hash))
	require.Equal(t, int32(11), requests.Load(), "busy answers use no try")
	require.Len(t, *slept, 10)
	for i, d := range *slept {
		require.GreaterOrEqual(t, d, defaultBackoff, "wait %d", i)
		require.Less(t, d, defaultBackoff+maxBusyJitter, "wait %d", i)
	}
}

// Busy answers end the download 5 minutes after its first request.
func TestDownloadBusyIsBoundedByTime(t *testing.T) {
	t.Parallel()
	x := testXorb(t, 313)
	srv, requests := downloadServer(t, []*bbchunk.Xorb{x}, func(w http.ResponseWriter, _ *http.Request) bool {
		w.Header().Set("Retry-After", "60")
		http.Error(w, "busy", http.StatusServiceUnavailable)
		return true
	})
	defer srv.Close()
	c, slept := testClient(t, srv)

	err := c.Download(t.Context(), x.Hash, newMemSink())
	var se *StatusError
	require.ErrorAs(t, err, &se)
	require.Equal(t, http.StatusServiceUnavailable, se.StatusCode)
	require.Contains(t, err.Error(), "busy for")
	var total time.Duration
	for _, d := range *slept {
		total += d
	}
	require.LessOrEqual(t, total, maxBusy)
	require.Greater(t, total+61*time.Second, maxBusy, "the next wait would pass the bound")
	require.Equal(t, int32(len(*slept)+1), requests.Load())
	require.Greater(t, len(*slept), downloadAttempts, "busy answers use no try")
}

// A 503 without Retry-After uses a try: the node may be shutting down, and
// another node can answer the next try.
func TestDownloadGivesUpAfterThreeTries(t *testing.T) {
	t.Parallel()
	x := testXorb(t, 304)
	srv, requests := downloadServer(t, []*bbchunk.Xorb{x}, func(w http.ResponseWriter, _ *http.Request) bool {
		http.Error(w, "shutting down", http.StatusServiceUnavailable)
		return true
	})
	defer srv.Close()
	c, slept := testClient(t, srv)

	err := c.Download(t.Context(), x.Hash, newMemSink())
	var se *StatusError
	require.ErrorAs(t, err, &se)
	require.Equal(t, http.StatusServiceUnavailable, se.StatusCode)
	require.Contains(t, err.Error(), "gave up after 3 attempts")
	require.Equal(t, int32(downloadAttempts), requests.Load())
	require.Len(t, *slept, downloadAttempts-1)
}

// Each bad answer below fails one try. The next try gets the xorb.
func TestDownloadRetriesBadBody(t *testing.T) {
	t.Parallel()
	x := testXorb(t, 305)
	other := testXorb(t, 306)
	body := x.Complete()
	cases := map[string]func(w http.ResponseWriter){
		"truncated body": func(w http.ResponseWriter) {
			w.Header().Set("Content-Length", strconv.Itoa(len(body)))
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(body[:len(body)/2]) //nolint:errcheck // test
		},
		"other xorb": func(w http.ResponseWriter) {
			serveComplete(w, x.Hash, other.Complete())
		},
		"flipped byte": func(w http.ResponseWriter) {
			b := bytes.Clone(body)
			b[xorb.HeaderSize+5] ^= 1
			serveComplete(w, x.Hash, b)
		},
		"network form": func(w http.ResponseWriter) {
			serveComplete(w, x.Hash, x.Network)
		},
		"wrong ETag": func(w http.ResponseWriter) {
			w.Header().Set("ETag", `"`+other.Hash.String()+`"`)
			_, _ = w.Write(body) //nolint:errcheck // test
		},
	}
	for name, bad := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var served atomic.Bool
			srv, requests := downloadServer(t, []*bbchunk.Xorb{x}, func(w http.ResponseWriter, _ *http.Request) bool {
				if served.Swap(true) {
					return false
				}
				bad(w)
				return true
			})
			defer srv.Close()
			c, slept := testClient(t, srv)
			sink := newMemSink()

			require.NoError(t, c.Download(t.Context(), x.Hash, sink))
			require.Equal(t, body, sink.get(x.Hash))
			require.Equal(t, int32(2), requests.Load())
			require.Len(t, *slept, 1)
		})
	}
}

func TestDownloadRefusesBadBodyEveryTime(t *testing.T) {
	t.Parallel()
	x := testXorb(t, 307)
	other := testXorb(t, 308)
	srv, requests := downloadServer(t, nil, func(w http.ResponseWriter, _ *http.Request) bool {
		serveComplete(w, x.Hash, other.Complete())
		return true
	})
	defer srv.Close()
	c, _ := testClient(t, srv)
	sink := newMemSink()

	err := c.Download(t.Context(), x.Hash, sink)
	require.ErrorIs(t, err, bbchunk.ErrMalformedXorb)
	require.Equal(t, int32(downloadAttempts), requests.Load())
	require.False(t, sink.HasComplete(x.Hash))
}

func TestDownloadAcceptsWeakOrMissingETag(t *testing.T) {
	t.Parallel()
	x := testXorb(t, 309)
	for name, etag := range map[string]string{"weak": `W/"` + x.Hash.String() + `"`, "missing": ""} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			srv, _ := downloadServer(t, nil, func(w http.ResponseWriter, _ *http.Request) bool {
				if etag != "" {
					w.Header().Set("ETag", etag)
				}
				_, _ = w.Write(x.Complete()) //nolint:errcheck // test
				return true
			})
			defer srv.Close()
			c, _ := testClient(t, srv)
			require.NoError(t, c.Download(t.Context(), x.Hash, newMemSink()))
		})
	}
}

// staticDoer answers every request with a copy of resp.
type staticDoer struct {
	resp  http.Response
	calls atomic.Int32
}

func (d *staticDoer) Do(context.Context, string, string, io.ReadSeeker, func(*http.Request)) (*http.Response, error) {
	d.calls.Add(1)
	r := d.resp
	r.Body = io.NopCloser(strings.NewReader(""))
	return &r, nil
}

func TestDownloadRefusesTooLargeAnswer(t *testing.T) {
	t.Parallel()
	x := testXorb(t, 310)
	d := &staticDoer{resp: http.Response{StatusCode: http.StatusOK, ContentLength: xorb.MaxCompleteSize + 1, Header: http.Header{}}}
	c, slept := withSleepRecorder(New(d))

	err := c.Download(t.Context(), x.Hash, newMemSink())
	require.ErrorContains(t, err, "over the")
	require.Equal(t, int32(1), d.calls.Load())
	require.Empty(t, *slept)
}

// A failed write to the local store is not a fault of the server, so the
// download does not try again.
func TestDownloadDoesNotRetrySinkFailure(t *testing.T) {
	t.Parallel()
	x := testXorb(t, 311)
	srv, requests := downloadServer(t, []*bbchunk.Xorb{x}, nil)
	defer srv.Close()
	c, slept := testClient(t, srv)
	want := errors.New("disk full")
	sink := newMemSink()
	sink.failWrite = want

	err := c.Download(t.Context(), x.Hash, sink)
	require.ErrorIs(t, err, want)
	require.NotErrorIs(t, err, bbchunk.ErrMalformedXorb, "a full disk is not a bad xorb")
	require.NotContains(t, err.Error(), "malformed")
	require.Contains(t, err.Error(), "write to the local store: disk full")
	require.Equal(t, int32(1), requests.Load())
	require.Empty(t, *slept)
}

func TestDownloadAllBoundsInFlight(t *testing.T) {
	t.Parallel()
	const n = 10
	xs := make([]*bbchunk.Xorb, n)
	hs := make([]xethash.Hash, 0, n+2)
	for i := range xs {
		xs[i] = testXorb(t, uint64(400+i))
		hs = append(hs, xs[i].Hash)
	}
	hs = append(hs, xs[0].Hash, xs[3].Hash)
	var inFlight, peak atomic.Int32
	full := make(chan struct{})
	var once sync.Once
	srv, requests := downloadServer(t, xs, func(http.ResponseWriter, *http.Request) bool {
		cur := inFlight.Add(1)
		defer inFlight.Add(-1)
		for {
			p := peak.Load()
			if cur <= p || peak.CompareAndSwap(p, cur) {
				break
			}
		}
		if cur == MaxDownloadsInFlight {
			once.Do(func() { close(full) })
		}
		select {
		case <-full:
		case <-time.After(5 * time.Second):
		}
		return false
	})
	defer srv.Close()
	c, _ := testClient(t, srv)
	sink := newMemSink()
	held := testXorb(t, 450)
	sink.held[held.Hash] = held.Complete()
	hs = append(hs, held.Hash)

	var mu sync.Mutex
	var reports [][2]int
	require.NoError(t, c.DownloadAll(t.Context(), hs, sink, func(done, total int) {
		mu.Lock()
		reports = append(reports, [2]int{done, total})
		mu.Unlock()
	}))
	for _, x := range xs {
		require.Equal(t, x.Complete(), sink.get(x.Hash))
	}
	require.Equal(t, int32(MaxDownloadsInFlight), peak.Load())
	require.Equal(t, int32(n), requests.Load(), "one request per missing xorb")
	require.Len(t, reports, n+1)
	for i, r := range reports {
		require.Equal(t, [2]int{i, n}, r)
	}
}

func TestDownloadAllStopsOnError(t *testing.T) {
	t.Parallel()
	good := testXorb(t, 500)
	missing := testXorb(t, 501)
	srv, _ := downloadServer(t, []*bbchunk.Xorb{good}, nil)
	defer srv.Close()
	c, _ := testClient(t, srv)

	err := c.DownloadAll(t.Context(), []xethash.Hash{missing.Hash, good.Hash}, newMemSink(), nil)
	var se *StatusError
	require.ErrorAs(t, err, &se)
	require.Equal(t, http.StatusNotFound, se.StatusCode)
	require.Contains(t, err.Error(), missing.Hash.String())
}
