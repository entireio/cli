package bbclient

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jmwoliver/xet-go/xethash"
	"github.com/jmwoliver/xet-go/xorb"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/internal/remotehelper/bbchunk"
	"github.com/entireio/cli/internal/remotehelper/replicas"
	"github.com/entireio/cli/internal/remotehelper/transport"
)

const repoPath = "/et/alice/repo"

// testClient returns a Client that talks to srv through a real Proxy, and
// the delays that it slept.
func testClient(t *testing.T, srv *httptest.Server) (*Client, *[]time.Duration) {
	t.Helper()
	p := transport.New(transport.Config{
		Nodes: replicas.NodeConfig{InitialNodes: []string{srv.URL}, EntryURL: srv.URL},
		Path:  repoPath,
	})
	return withSleepRecorder(New(p))
}

func withSleepRecorder(c *Client) (*Client, *[]time.Duration) {
	var mu sync.Mutex
	var slept []time.Duration
	c.sleep = func(_ context.Context, d time.Duration) error {
		mu.Lock()
		slept = append(slept, d)
		mu.Unlock()
		return nil
	}
	return c, &slept
}

func testXorb(t *testing.T, seed uint64) *bbchunk.Xorb {
	t.Helper()
	rng := rand.New(rand.NewPCG(seed, seed))
	var b xorb.Builder
	for range 3 {
		data := make([]byte, 1000+rng.IntN(1000))
		for i := range data {
			data[i] = byte(rng.Uint32())
		}
		_, err := b.Add(data, xorb.Auto)
		require.NoError(t, err)
	}
	x, err := bbchunk.Seal(&b)
	require.NoError(t, err)
	return x
}

func footerOf(x *bbchunk.Xorb) []wireChunk {
	out := make([]wireChunk, len(x.Footer.ChunkHashes))
	for i, h := range x.Footer.ChunkHashes {
		u, s := uint64(x.Footer.UnpackedChunkOffsets[i]), uint64(x.Footer.ChunkBoundaryOffsets[i])
		out[i] = wireChunk{Hash: textHash(h), UnpackedEnd: u, StoredEnd: s}
	}
	return out
}

func writeJSON(t *testing.T, w http.ResponseWriter, status int, v any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	require.NoError(t, json.NewEncoder(w).Encode(v))
}

// uploadServer answers uploads of the xorbs in xs as staged. before, when it
// is not nil, may answer a request first.
func uploadServer(t *testing.T, xs []*bbchunk.Xorb, before func(w http.ResponseWriter, r *http.Request) bool) *httptest.Server {
	t.Helper()
	byHash := map[string]*bbchunk.Xorb{}
	for _, x := range xs {
		byHash[x.Hash.String()] = x
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if before != nil && before(w, r) {
			return
		}
		hash, ok := strings.CutPrefix(r.URL.Path, repoPath+"/bb/xorbs/")
		x := byHash[hash]
		if !ok || r.Method != http.MethodPut || x == nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil || string(body) != string(x.Network) || r.Header.Get("Content-Type") != "application/octet-stream" {
			http.Error(w, "xorb hash mismatch", http.StatusUnprocessableEntity)
			return
		}
		writeJSON(t, w, http.StatusCreated, uploadResponse{Xorb: textHash(x.Hash), State: StateStaged, Chunks: footerOf(x)})
	}))
}

func TestUpload(t *testing.T) {
	t.Parallel()
	x := testXorb(t, 1)
	srv := uploadServer(t, []*bbchunk.Xorb{x}, nil)
	defer srv.Close()
	c, slept := testClient(t, srv)

	r, err := c.Upload(t.Context(), x)
	require.NoError(t, err)
	require.Equal(t, x.Hash, r.Xorb)
	require.Equal(t, StateStaged, r.State)
	require.Len(t, r.Chunks, len(x.Footer.ChunkHashes))
	for i, h := range x.Footer.ChunkHashes {
		require.Equal(t, h, r.Chunks[i].Hash)
	}
	require.Empty(t, *slept)
}

func TestUploadPermanent(t *testing.T) {
	t.Parallel()
	x := testXorb(t, 2)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, http.StatusOK, uploadResponse{Xorb: textHash(x.Hash), State: StatePermanent, Chunks: footerOf(x)})
	}))
	defer srv.Close()
	c, _ := testClient(t, srv)

	r, err := c.Upload(t.Context(), x)
	require.NoError(t, err)
	require.Equal(t, StatePermanent, r.State)
}

func TestUploadRetriesBusyNode(t *testing.T) {
	t.Parallel()
	x := testXorb(t, 3)
	var calls atomic.Int32
	srv := uploadServer(t, []*bbchunk.Xorb{x}, func(w http.ResponseWriter, _ *http.Request) bool {
		if calls.Add(1) > 2 {
			return false
		}
		w.Header().Set("Retry-After", "3")
		http.Error(w, "no xorb buffer free", http.StatusServiceUnavailable)
		return true
	})
	defer srv.Close()
	c, slept := testClient(t, srv)

	r, err := c.Upload(t.Context(), x)
	require.NoError(t, err)
	require.Equal(t, StateStaged, r.State)
	require.Equal(t, int32(3), calls.Load())
	require.Equal(t, []time.Duration{3 * time.Second, 3 * time.Second}, *slept)
}

func TestUploadBusyCapsRetryAfter(t *testing.T) {
	t.Parallel()
	x := testXorb(t, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "86400")
		http.Error(w, "no xorb buffer free", http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	c, slept := testClient(t, srv)

	_, err := c.Upload(t.Context(), x)
	var se *StatusError
	require.ErrorAs(t, err, &se)
	require.Equal(t, http.StatusServiceUnavailable, se.StatusCode)
	require.Len(t, *slept, defaultAttempts-1)
	for _, d := range *slept {
		require.Equal(t, defaultMaxRetryAft, d)
	}
}

func TestUploadPermanentErrors(t *testing.T) {
	t.Parallel()
	for _, status := range []int{http.StatusBadRequest, http.StatusNotFound, http.StatusRequestEntityTooLarge, http.StatusUnprocessableEntity, http.StatusForbidden} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			t.Parallel()
			x := testXorb(t, 5)
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				http.Error(w, "xorb hash mismatch", status)
			}))
			defer srv.Close()
			c, slept := testClient(t, srv)

			_, err := c.Upload(t.Context(), x)
			var se *StatusError
			require.ErrorAs(t, err, &se)
			require.Equal(t, status, se.StatusCode)
			require.Equal(t, "xorb hash mismatch", se.Message)
			require.Equal(t, int32(1), calls.Load())
			require.Empty(t, *slept)
		})
	}
}

func TestUploadRefusesBadAnswer(t *testing.T) {
	t.Parallel()
	x, other := testXorb(t, 6), testXorb(t, 7)
	for name, resp := range map[string]uploadResponse{
		"other xorb":    {Xorb: textHash(other.Hash), State: StateStaged, Chunks: footerOf(other)},
		"other footer":  {Xorb: textHash(x.Hash), State: StateStaged, Chunks: footerOf(other)},
		"missing state": {Xorb: textHash(x.Hash), State: StateMissing, Chunks: footerOf(x)},
		"no chunks":     {Xorb: textHash(x.Hash), State: StateStaged},
		"bad ends": {Xorb: textHash(x.Hash), State: StateStaged, Chunks: func() []wireChunk {
			f := footerOf(x)
			f[1].UnpackedEnd = f[0].UnpackedEnd
			return f
		}()},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				writeJSON(t, w, http.StatusCreated, resp)
			}))
			defer srv.Close()
			c, _ := testClient(t, srv)
			_, err := c.Upload(t.Context(), x)
			require.Error(t, err)
		})
	}
}

// fakeDoer answers from a list of errors, then from next.
type fakeDoer struct {
	mu   sync.Mutex
	errs []error
	next Doer
}

func (f *fakeDoer) Do(ctx context.Context, method, suffix string, body io.ReadSeeker, setHeaders func(*http.Request)) (*http.Response, error) {
	f.mu.Lock()
	if len(f.errs) > 0 {
		err := f.errs[0]
		f.errs = f.errs[1:]
		f.mu.Unlock()
		return nil, err
	}
	f.mu.Unlock()
	return f.next.Do(ctx, method, suffix, body, setHeaders)
}

func TestUploadRetriesTransportFailure(t *testing.T) {
	t.Parallel()
	x := testXorb(t, 8)
	srv := uploadServer(t, []*bbchunk.Xorb{x}, nil)
	defer srv.Close()
	p := transport.New(transport.Config{Nodes: replicas.NodeConfig{InitialNodes: []string{srv.URL}, EntryURL: srv.URL}, Path: repoPath})
	d := &fakeDoer{errs: []error{transport.ErrNoNodeSucceeded, transport.ErrNoNodeSucceeded}, next: p}
	c, slept := withSleepRecorder(New(d))

	r, err := c.Upload(t.Context(), x)
	require.NoError(t, err)
	require.Equal(t, StateStaged, r.State)
	require.Len(t, *slept, 2)
	require.GreaterOrEqual(t, (*slept)[0], defaultBackoff)
	require.Less(t, (*slept)[0], defaultBackoff*3/2)
	require.GreaterOrEqual(t, (*slept)[1], 2*defaultBackoff, "backoff doubles")
}

// A failed connection removes the node from the replica set. Once the set
// is empty, Do fails at once with ErrUnreachable. Retries would only sleep
// through the backoff before the push falls back to send-pack.
func TestUploadStopsWhenNoNodeIsLeft(t *testing.T) {
	t.Parallel()
	x := testXorb(t, 10)
	down := httptest.NewServer(http.NotFoundHandler())
	down.Close()
	c, slept := testClient(t, down)

	_, err := c.Upload(t.Context(), x)
	require.ErrorIs(t, err, transport.ErrUnreachable)
	require.Len(t, *slept, 1, "one retry after the connection failure, none after the set is empty")
}

func TestUploadDoesNotRetryOtherFailures(t *testing.T) {
	t.Parallel()
	x := testXorb(t, 9)
	want := errors.New("refusing to attach credentials")
	c, slept := withSleepRecorder(New(&fakeDoer{errs: []error{want}}))

	_, err := c.Upload(t.Context(), x)
	require.ErrorIs(t, err, want)
	require.Empty(t, *slept)
}

func TestUploaderBoundsInFlight(t *testing.T) {
	t.Parallel()
	const n = 12
	xs := make([]*bbchunk.Xorb, n)
	for i := range xs {
		xs[i] = testXorb(t, uint64(100+i))
	}
	var inFlight, peak atomic.Int32
	full := make(chan struct{})
	var once sync.Once
	srv := uploadServer(t, xs, func(http.ResponseWriter, *http.Request) bool {
		cur := inFlight.Add(1)
		defer inFlight.Add(-1)
		for {
			p := peak.Load()
			if cur <= p || peak.CompareAndSwap(p, cur) {
				break
			}
		}
		if cur == MaxInFlight {
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

	u := c.NewUploader(t.Context())
	for _, x := range xs {
		require.NoError(t, u.Add(x))
	}
	require.NoError(t, u.Add(xs[0]), "a repeated xorb is skipped")
	results, err := u.Wait()
	require.NoError(t, err)
	require.Len(t, results, n)
	for _, x := range xs {
		require.Equal(t, StateStaged, results[x.Hash].State)
	}
	require.Equal(t, int32(MaxInFlight), peak.Load())
}

func TestUploaderStopsOnError(t *testing.T) {
	t.Parallel()
	good, bad := testXorb(t, 200), testXorb(t, 201)
	srv := uploadServer(t, []*bbchunk.Xorb{good}, nil)
	defer srv.Close()
	c, _ := testClient(t, srv)

	u := c.NewUploader(t.Context())
	require.NoError(t, u.Add(good))
	require.NoError(t, u.Add(bad))
	results, err := u.Wait()
	var se *StatusError
	require.ErrorAs(t, err, &se)
	require.Equal(t, http.StatusBadRequest, se.StatusCode)
	for h, r := range results {
		require.Equal(t, h, r.Xorb, "only finished uploads")
	}
	require.NotContains(t, results, bad.Hash)
	require.ErrorContains(t, u.Add(testXorb(t, 202)), "uploads stopped")
}

// orderedDoer answers the upload of first with 201, and the upload of every
// other xorb with 400 once the body of first's answer is closed. So the
// failure arrives after the client read first's answer, with no timing.
type orderedDoer struct {
	first    *bbchunk.Xorb
	readDone chan struct{}
}

type closeNotifier struct {
	io.Reader

	once *sync.Once
	done chan struct{}
}

func (c closeNotifier) Close() error {
	c.once.Do(func() { close(c.done) })
	return nil
}

func (d *orderedDoer) Do(ctx context.Context, _, suffix string, _ io.ReadSeeker, _ func(*http.Request)) (*http.Response, error) {
	if suffix == "/bb/xorbs/"+d.first.Hash.String() {
		body, err := json.Marshal(uploadResponse{Xorb: textHash(d.first.Hash), State: StateStaged, Chunks: footerOf(d.first)})
		if err != nil {
			return nil, err
		}
		rc := closeNotifier{Reader: strings.NewReader(string(body)), once: &sync.Once{}, done: d.readDone}
		return &http.Response{StatusCode: http.StatusCreated, Body: rc, Header: http.Header{}}, nil
	}
	select {
	case <-d.readDone:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return &http.Response{StatusCode: http.StatusBadRequest, Body: io.NopCloser(strings.NewReader("bad xorb")), Header: http.Header{}}, nil
}

// An upload that fails after another one finished leaves the finished one
// in the results, so the caller can record it for the next push.
func TestUploaderKeepsFinishedUploadsAfterAFailure(t *testing.T) {
	t.Parallel()
	a, b := testXorb(t, 210), testXorb(t, 211)
	c := New(&orderedDoer{first: a, readDone: make(chan struct{})})

	u := c.NewUploader(t.Context())
	require.NoError(t, u.Add(b))
	require.NoError(t, u.Add(a))
	results, err := u.Wait()
	var se *StatusError
	require.ErrorAs(t, err, &se)
	require.Equal(t, http.StatusBadRequest, se.StatusCode)
	require.Contains(t, results, a.Hash)
	require.Equal(t, StateStaged, results[a.Hash].State)
	require.NotContains(t, results, b.Hash)
}

// dedupServer answers dedup queries from the permanent xorbs in xs, and
// records each request.
func dedupServer(t *testing.T, xs []*bbchunk.Xorb, staged map[xethash.Hash]bool) (*httptest.Server, *[]dedupRequest) {
	t.Helper()
	type loc struct {
		x     *bbchunk.Xorb
		index int
	}
	anchors := map[xethash.Hash]loc{}
	byHash := map[xethash.Hash]*bbchunk.Xorb{}
	for _, x := range xs {
		byHash[x.Hash] = x
		for i, h := range x.Footer.ChunkHashes {
			anchors[h] = loc{x, i}
		}
	}
	var mu sync.Mutex
	var reqs []dedupRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != repoPath+"/bb/dedup" || r.Header.Get("Content-Type") != "application/json" {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		var req dedupRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if len(req.Anchors) > MaxDedupEntries || len(req.Xorbs) > MaxDedupEntries {
			http.Error(w, "too many", http.StatusRequestEntityTooLarge)
			return
		}
		mu.Lock()
		reqs = append(reqs, req)
		mu.Unlock()
		var resp dedupResponse
		resp.Footers = map[textHash]struct {
			Chunks []wireChunk `json:"chunks"`
		}{}
		for _, a := range req.Anchors {
			if l, ok := anchors[xethash.Hash(a)]; ok {
				resp.Anchors = append(resp.Anchors, struct {
					Chunk textHash `json:"chunk"`
					Xorb  textHash `json:"xorb"`
					Index int      `json:"index"`
				}{a, textHash(l.x.Hash), l.index})
				resp.Footers[textHash(l.x.Hash)] = struct {
					Chunks []wireChunk `json:"chunks"`
				}{footerOf(l.x)}
			}
		}
		for _, h := range req.Xorbs {
			state := StateMissing
			if x, ok := byHash[xethash.Hash(h)]; ok {
				state = StatePermanent
				if staged[x.Hash] {
					state = StateStaged
				}
			}
			resp.Xorbs = append(resp.Xorbs, struct {
				Xorb  textHash `json:"xorb"`
				State State    `json:"state"`
			}{h, state})
		}
		writeJSON(t, w, http.StatusOK, resp)
	}))
	return srv, &reqs
}

func randomHash(rng *rand.Rand) xethash.Hash {
	var h xethash.Hash
	for i := range h {
		h[i] = byte(rng.Uint32())
	}
	return h
}

func TestDedup(t *testing.T) {
	t.Parallel()
	held, staged := testXorb(t, 300), testXorb(t, 301)
	srv, reqs := dedupServer(t, []*bbchunk.Xorb{held, staged}, map[xethash.Hash]bool{staged.Hash: true})
	defer srv.Close()
	c, _ := testClient(t, srv)
	rng := rand.New(rand.NewPCG(1, 2))
	unknown := randomHash(rng)

	res, err := c.Dedup(t.Context(), []xethash.Hash{held.Footer.ChunkHashes[1], unknown}, []xethash.Hash{staged.Hash, unknown})
	require.NoError(t, err)
	require.Equal(t, []AnchorHit{{Chunk: held.Footer.ChunkHashes[1], Xorb: held.Hash, Index: 1}}, res.Anchors)
	require.Equal(t, []XorbState{{Xorb: staged.Hash, State: StateStaged}, {Xorb: unknown, State: StateMissing}}, res.Xorbs)
	require.Equal(t, []xethash.Hash{held.Hash}, keys(res.Footers), "footers come with anchor hits only")
	require.Len(t, res.Footers[held.Hash], len(held.Footer.ChunkHashes))
	require.False(t, res.Truncated)
	require.Len(t, *reqs, 1)
}

// The server caps the footers of one answer. A hit without its footer is a
// miss, not an error.
func TestDedupDropsHitWithoutFooter(t *testing.T) {
	t.Parallel()
	held, capped := testXorb(t, 310), testXorb(t, 311)
	hit := func(x *bbchunk.Xorb) map[string]any {
		return map[string]any{"chunk": x.Footer.ChunkHashes[0].String(), "xorb": x.Hash.String(), "index": 0}
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, http.StatusOK, map[string]any{
			"anchors":   []any{hit(held), hit(capped)},
			"xorbs":     []any{map[string]any{"xorb": capped.Hash.String(), "state": "permanent"}},
			"footers":   map[string]any{held.Hash.String(): map[string]any{"chunks": footerOf(held)}},
			"truncated": true,
		})
	}))
	defer srv.Close()
	c, _ := testClient(t, srv)

	res, err := c.Dedup(t.Context(), []xethash.Hash{held.Footer.ChunkHashes[0], capped.Footer.ChunkHashes[0]}, []xethash.Hash{capped.Hash})
	require.NoError(t, err)
	require.Equal(t, []AnchorHit{{Chunk: held.Footer.ChunkHashes[0], Xorb: held.Hash, Index: 0}}, res.Anchors)
	require.Equal(t, []XorbState{{Xorb: capped.Hash, State: StatePermanent}}, res.Xorbs)
	require.Equal(t, []xethash.Hash{held.Hash}, keys(res.Footers))
	require.True(t, res.Truncated)
}

// A footer that no accepted hit names does not reach the caller: the caller
// records the xorb of each footer as permanent.
func TestDedupDropsFooterWithoutHit(t *testing.T) {
	t.Parallel()
	held, unasked := testXorb(t, 320), testXorb(t, 321)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, http.StatusOK, map[string]any{
			"anchors": []any{map[string]any{"chunk": held.Footer.ChunkHashes[0].String(), "xorb": held.Hash.String(), "index": 0}},
			"footers": map[string]any{
				held.Hash.String():    map[string]any{"chunks": footerOf(held)},
				unasked.Hash.String(): map[string]any{"chunks": footerOf(unasked)},
			},
		})
	}))
	defer srv.Close()
	c, _ := testClient(t, srv)

	res, err := c.Dedup(t.Context(), []xethash.Hash{held.Footer.ChunkHashes[0]}, nil)
	require.NoError(t, err)
	require.Equal(t, []xethash.Hash{held.Hash}, keys(res.Footers))
}

// The server sends at most 64 footers in one answer.
func TestDedupRefusesTooManyFooters(t *testing.T) {
	t.Parallel()
	footers := map[string]any{}
	for i := range maxDedupFooters + 1 {
		x := testXorb(t, uint64(600+i))
		footers[x.Hash.String()] = map[string]any{"chunks": footerOf(x)}
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, http.StatusOK, map[string]any{"footers": footers})
	}))
	defer srv.Close()
	c, _ := testClient(t, srv)

	_, err := c.Dedup(t.Context(), []xethash.Hash{{1}}, nil)
	require.ErrorContains(t, err, "footers")
}

// The stored ends are not part of the xorb hash, so a footer with odd stored
// ends still proves the chunks.
func TestUploadToleratesStoredEnds(t *testing.T) {
	t.Parallel()
	x := testXorb(t, 330)
	f := footerOf(x)
	for i := range f {
		f[i].StoredEnd = 0
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, http.StatusCreated, uploadResponse{Xorb: textHash(x.Hash), State: StateStaged, Chunks: f})
	}))
	defer srv.Close()
	c, _ := testClient(t, srv)

	r, err := c.Upload(t.Context(), x)
	require.NoError(t, err)
	require.Len(t, r.Chunks, len(x.Footer.ChunkHashes))
}

func TestDedupEmpty(t *testing.T) {
	t.Parallel()
	srv, reqs := dedupServer(t, nil, nil)
	defer srv.Close()
	c, _ := testClient(t, srv)

	res, err := c.Dedup(t.Context(), nil, nil)
	require.NoError(t, err)
	require.Empty(t, res.Anchors)
	require.Empty(t, *reqs)
}

func TestDedupBatches(t *testing.T) {
	t.Parallel()
	held := testXorb(t, 400)
	srv, reqs := dedupServer(t, []*bbchunk.Xorb{held}, nil)
	defer srv.Close()
	c, _ := testClient(t, srv)
	rng := rand.New(rand.NewPCG(3, 4))

	anchors := make([]xethash.Hash, 2*MaxDedupEntries+5)
	for i := range anchors {
		anchors[i] = randomHash(rng)
	}
	anchors[MaxDedupEntries+1] = held.Footer.ChunkHashes[2]
	xorbs := []xethash.Hash{held.Hash, randomHash(rng)}

	res, err := c.Dedup(t.Context(), anchors, xorbs)
	require.NoError(t, err)
	require.Len(t, *reqs, 3)
	require.Len(t, (*reqs)[0].Anchors, MaxDedupEntries)
	require.Len(t, (*reqs)[0].Xorbs, 2)
	require.Len(t, (*reqs)[1].Anchors, MaxDedupEntries)
	require.Empty(t, (*reqs)[1].Xorbs)
	require.Len(t, (*reqs)[2].Anchors, 5)
	require.Equal(t, []AnchorHit{{Chunk: held.Footer.ChunkHashes[2], Xorb: held.Hash, Index: 2}}, res.Anchors)
	require.Len(t, res.Xorbs, 2)
	require.Equal(t, []xethash.Hash{held.Hash}, keys(res.Footers))
}

func keys(m map[xethash.Hash][]FooterChunk) []xethash.Hash {
	var out []xethash.Hash
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestDedupRefusesBadAnswer(t *testing.T) {
	t.Parallel()
	held, other := testXorb(t, 500), testXorb(t, 501)
	anchor := held.Footer.ChunkHashes[0]
	type footers = map[string]any
	for name, body := range map[string]map[string]any{
		"unasked anchor": {"anchors": []any{map[string]any{"chunk": other.Footer.ChunkHashes[0].String(), "xorb": other.Hash.String(), "index": 0}},
			"footers": footers{other.Hash.String(): map[string]any{"chunks": footerOf(other)}}},
		"anchor at wrong index": {"anchors": []any{map[string]any{"chunk": anchor.String(), "xorb": held.Hash.String(), "index": 1}},
			"footers": footers{held.Hash.String(): map[string]any{"chunks": footerOf(held)}}},
		"footer of another xorb": {"anchors": []any{map[string]any{"chunk": anchor.String(), "xorb": held.Hash.String(), "index": 0}},
			"footers": footers{held.Hash.String(): map[string]any{"chunks": footerOf(other)}}},
		"unasked xorb":          {"xorbs": []any{map[string]any{"xorb": other.Hash.String(), "state": "missing"}}},
		"unknown state":         {"xorbs": []any{map[string]any{"xorb": held.Hash.String(), "state": "lost"}}},
		"hash not in text form": {"xorbs": []any{map[string]any{"xorb": "zz", "state": "missing"}}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				writeJSON(t, w, http.StatusOK, body)
			}))
			defer srv.Close()
			c, _ := testClient(t, srv)
			_, err := c.Dedup(t.Context(), []xethash.Hash{anchor}, []xethash.Hash{held.Hash})
			require.Error(t, err)
		})
	}
}

func TestDedupPermanentError(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "too many entries", http.StatusRequestEntityTooLarge)
	}))
	defer srv.Close()
	c, slept := testClient(t, srv)
	_, err := c.Dedup(t.Context(), []xethash.Hash{{1}}, nil)
	var se *StatusError
	require.ErrorAs(t, err, &se)
	require.Equal(t, http.StatusRequestEntityTooLarge, se.StatusCode)
	require.Empty(t, *slept)
}

func TestRetryAfter(t *testing.T) {
	t.Parallel()
	require.Equal(t, 5*time.Second, retryAfter("5"))
	require.Equal(t, time.Duration(-1), retryAfter(""))
	require.Equal(t, time.Duration(-1), retryAfter("soon"))
	require.Equal(t, time.Duration(0), retryAfter("Mon, 02 Jan 2006 15:04:05 GMT"))
	d := retryAfter(time.Now().Add(time.Minute).UTC().Format(http.TimeFormat))
	require.Greater(t, d, 30*time.Second)
}

func TestTextHashMatchesXetText(t *testing.T) {
	t.Parallel()
	h := xethash.Chunk([]byte("hello"))
	b, err := json.Marshal(textHash(h))
	require.NoError(t, err)
	require.JSONEq(t, `"`+h.String()+`"`, string(b))
	var back textHash
	require.NoError(t, json.Unmarshal(b, &back))
	require.Equal(t, h, xethash.Hash(back))
}

// maxDedupResponse must hold the largest valid dedup answer, and no more than
// that with some slack: the client reads the whole answer into memory.
func TestDedupResponseBound(t *testing.T) {
	t.Parallel()
	var h textHash
	for i := range h {
		h[i] = 0xff
	}
	chunk, err := json.Marshal(wireChunk{Hash: h, UnpackedEnd: xorb.MaxRawSize, StoredEnd: xorb.MaxNetworkSize})
	require.NoError(t, err)
	require.LessOrEqual(t, len(chunk)+1, maxFooterChunkJSON)

	var wire dedupResponse
	wire.Anchors = make([]struct {
		Chunk textHash `json:"chunk"`
		Xorb  textHash `json:"xorb"`
		Index int      `json:"index"`
	}, 1)
	wire.Anchors[0].Chunk, wire.Anchors[0].Xorb, wire.Anchors[0].Index = h, h, xorb.MaxChunks-1
	wire.Xorbs = make([]struct {
		Xorb  textHash `json:"xorb"`
		State State    `json:"state"`
	}, 1)
	wire.Xorbs[0].Xorb, wire.Xorbs[0].State = h, StatePermanent
	anchor, err := json.Marshal(wire.Anchors[0])
	require.NoError(t, err)
	require.LessOrEqual(t, len(anchor)+1, maxDedupEntryJSON)
	state, err := json.Marshal(wire.Xorbs[0])
	require.NoError(t, err)
	require.LessOrEqual(t, len(state)+1, maxDedupEntryJSON)

	require.GreaterOrEqual(t, maxDedupResponse, maxDedupFooters*xorb.MaxChunks*(len(chunk)+1)+MaxDedupEntries*(len(anchor)+len(state)+2))
	require.LessOrEqual(t, maxDedupResponse, 80<<20)
}
