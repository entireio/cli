package transport

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/entireio/cli/internal/remotehelper/gitproto"
)

// receivedPOST is one receive-pack POST as the server saw it.
type receivedPOST struct {
	path          string
	contentLength int64
	body          []byte
}

type postRecorder struct {
	mu    sync.Mutex
	posts []receivedPOST
}

func (r *postRecorder) record(t *testing.T, req *http.Request) {
	t.Helper()
	body, err := io.ReadAll(req.Body)
	if err != nil {
		t.Errorf("reading POST body: %v", err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.posts = append(r.posts, receivedPOST{path: req.URL.Path, contentLength: req.ContentLength, body: body})
}

// newReplayBody builds a multi-chunk receive-pack body the way the push
// paths do, so a resend has to rewind across chunk boundaries.
func newReplayBody(t *testing.T) (*gitproto.RequestBody, []byte) {
	t.Helper()
	want := bytes.Repeat([]byte("0123456789abcdef"), 10_000) // ~156 KiB, three chunks
	var body gitproto.RequestBody
	if _, err := body.ReadFrom(bytes.NewReader(want)); err != nil {
		t.Fatal(err)
	}
	return &body, want
}

func assertFullBodies(t *testing.T, rec *postRecorder, wantPosts int, want []byte) {
	t.Helper()
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.posts) != wantPosts {
		t.Fatalf("server saw %d POSTs, want %d", len(rec.posts), wantPosts)
	}
	for i, p := range rec.posts {
		if !bytes.Equal(p.body, want) {
			t.Errorf("POST %d to %s carried %d bytes, want the full %d", i, p.path, len(p.body), len(want))
		}
		if p.contentLength != int64(len(want)) {
			t.Errorf("POST %d to %s Content-Length = %d, want %d", i, p.path, p.contentLength, len(want))
		}
	}
}

// A replica that reads the whole body and then fails must not leave the
// failover with a drained body: the next replica gets all of it again.
func TestServiceRPC_ChunkedBodyReplaysOnFailover(t *testing.T) {
	t.Parallel()
	body, want := newReplayBody(t)

	rec := &postRecorder{}
	var mu sync.Mutex
	failed := false
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.record(t, r)
		mu.Lock()
		first := !failed
		failed = true
		mu.Unlock()
		if first {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		io.WriteString(w, "ok") //nolint:errcheck // test server
	})
	a := httptest.NewServer(handler)
	defer a.Close()
	b := httptest.NewServer(handler)
	defer b.Close()

	p := proxyWithClient([]string{a.URL, b.URL}, "/et/owner/repo", "", "", &http.Client{})
	resp, err := p.ServiceRPC(t.Context(), "git-receive-pack", body.Reader(), body.ApplyTo)
	if err != nil {
		t.Fatalf("ServiceRPC: %v", err)
	}
	resp.Close()

	assertFullBodies(t, rec, 2, want)
}

// net/http resends a 307'd POST through GetBody; without it the redirect
// would not be followed, and with a shared reader it would send nothing.
func TestServiceRPC_ChunkedBodyReplaysOnRedirect(t *testing.T) {
	t.Parallel()
	body, want := newReplayBody(t)

	rec := &postRecorder{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.record(t, r)
		if !strings.HasPrefix(r.URL.Path, "/moved/") {
			http.Redirect(w, r, "/moved"+r.URL.Path, http.StatusTemporaryRedirect)
			return
		}
		io.WriteString(w, "ok") //nolint:errcheck // test server
	}))
	defer server.Close()

	p := testProxy(server)
	resp, err := p.ServiceRPC(t.Context(), "git-receive-pack", body.Reader(), body.ApplyTo)
	if err != nil {
		t.Fatalf("ServiceRPC: %v", err)
	}
	resp.Close()

	assertFullBodies(t, rec, 2, want)
}
