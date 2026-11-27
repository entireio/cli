package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jmwoliver/xet-go/xethash"
	"github.com/jmwoliver/xet-go/xorb"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/internal/remotehelper/bbchunk"
	"github.com/entireio/cli/internal/remotehelper/bbclient"
	"github.com/entireio/cli/internal/remotehelper/replicas"
	"github.com/entireio/cli/internal/remotehelper/transport"
)

// bbAuthServer answers the BB endpoints of /et/alice/repo. It answers 401 to
// every request without "Bearer <accept>", and records the Authorization
// header of each request.
type bbAuthServer struct {
	accept string
	x      *bbchunk.Xorb
	// rejectWait holds each rejected request until n requests were
	// rejected, so the 401s overlap.
	rejectWait *sync.WaitGroup

	mu    sync.Mutex
	auths []string
}

func (s *bbAuthServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	auth := r.Header.Get("Authorization")
	s.mu.Lock()
	s.auths = append(s.auths, r.Method+" "+r.URL.Path+" "+auth)
	s.mu.Unlock()
	if auth != "Bearer "+s.accept {
		if s.rejectWait != nil {
			s.rejectWait.Done()
			s.rejectWait.Wait()
		}
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.Method == http.MethodPut && r.URL.Path == "/et/alice/repo/bb/xorbs/"+s.x.Hash.String():
		_, _ = io.Copy(io.Discard, r.Body) //nolint:errcheck // test
		chunks := []map[string]any{}
		f := s.x.Footer
		for i, h := range f.ChunkHashes {
			chunks = append(chunks, map[string]any{"hash": h.String(), "unpacked_end": f.UnpackedChunkOffsets[i], "stored_end": f.ChunkBoundaryOffsets[i]})
		}
		w.WriteHeader(http.StatusCreated)
		encodeTestJSON(w, map[string]any{"xorb": s.x.Hash.String(), "state": "staged", "chunks": chunks})
	case r.Method == http.MethodGet && r.URL.Path == "/et/alice/repo/bb/xorbs/"+s.x.Hash.String():
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(s.x.Complete()) //nolint:errcheck // test
	case r.Method == http.MethodPost && r.URL.Path == "/et/alice/repo/bb/dedup":
		encodeTestJSON(w, map[string]any{"anchors": []any{}, "xorbs": []any{}, "footers": map[string]any{}})
	default:
		http.NotFound(w, r)
	}
}

func encodeTestJSON(w io.Writer, v map[string]any) {
	if err := json.NewEncoder(w).Encode(v); err != nil {
		panic(err)
	}
}

func (s *bbAuthServer) authorizations() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.auths...)
}

// bbAuthProxy wires a Proxy as run does: the credential gate of
// setAuthWithProvider and the 401 observer of refreshingProvider.
func bbAuthProxy(t *testing.T, srv *httptest.Server, cred refreshableCredential) *transport.Proxy {
	t.Helper()
	u, err := url.Parse(srv.URL)
	require.NoError(t, err)
	provider, onUnauthorized := refreshingProvider(cred)
	return transport.New(transport.Config{
		Nodes:          replicas.NodeConfig{InitialNodes: []string{srv.URL}, EntryURL: srv.URL, ClusterHost: u.Hostname()},
		Path:           "/et/alice/repo",
		SetAuth:        setAuthWithProvider(provider),
		OnUnauthorized: onUnauthorized,
	})
}

func bbAuthXorb(t *testing.T) *bbchunk.Xorb {
	t.Helper()
	var b xorb.Builder
	_, err := b.Add([]byte(strings.Repeat("large blob chunk ", 512)), xorb.Auto)
	require.NoError(t, err)
	x, err := bbchunk.Seal(&b)
	require.NoError(t, err)
	return x
}

// memSink is a bbclient.XorbSink in memory.
type memSink struct{ got []byte }

func (m *memSink) HasComplete(xethash.Hash) bool { return false }

func (m *memSink) PutComplete(_ xethash.Hash, fill func(io.Writer) (*bbchunk.Footer, error)) error {
	var b strings.Builder
	if _, err := fill(&b); err != nil {
		return err
	}
	m.got = []byte(b.String())
	return nil
}

// The credential gate admits the BB upload, dedup and download requests, so
// the bearer reaches the server on each.
func TestBBRequestsCarryTheCredential(t *testing.T) {
	t.Parallel()
	s := &bbAuthServer{accept: "jwt-a", x: bbAuthXorb(t)}
	srv := httptest.NewServer(s)
	defer srv.Close()
	client := bbclient.New(bbAuthProxy(t, srv, &fakeRefreshableCredential{token: "jwt-a"}))

	res, err := client.Upload(t.Context(), s.x)
	require.NoError(t, err)
	require.Equal(t, bbclient.StateStaged, res.State)
	_, err = client.Dedup(t.Context(), []xethash.Hash{s.x.Footer.ChunkHashes[0]}, nil)
	require.NoError(t, err)
	var sink memSink
	require.NoError(t, client.Download(t.Context(), s.x.Hash, &sink))
	require.Equal(t, s.x.Complete(), sink.got)

	require.Equal(t, []string{
		"PUT /et/alice/repo/bb/xorbs/" + s.x.Hash.String() + " Bearer jwt-a",
		"POST /et/alice/repo/bb/dedup Bearer jwt-a",
		"GET /et/alice/repo/bb/xorbs/" + s.x.Hash.String() + " Bearer jwt-a",
	}, s.authorizations())
}

// Four uploads that run at once all get a 401 for the same bearer. Each
// retry carries the refreshed bearer, and the credential mints once.
func TestBBConcurrentUnauthorizedShareOneRefresh(t *testing.T) {
	t.Parallel()
	const uploads = 4
	var rejected sync.WaitGroup
	rejected.Add(uploads)
	s := &bbAuthServer{accept: "jwt-b", x: bbAuthXorb(t), rejectWait: &rejected}
	srv := httptest.NewServer(s)
	defer srv.Close()
	cred := &fakeRefreshableCredential{token: "jwt-a", tokens: []string{"jwt-b", "jwt-c"}}
	client := bbclient.New(bbAuthProxy(t, srv, cred))

	errs := make(chan error, uploads)
	for range uploads {
		go func() {
			_, err := client.Upload(t.Context(), s.x)
			errs <- err
		}()
	}
	for range uploads {
		select {
		case err := <-errs:
			require.NoError(t, err)
		case <-time.After(30 * time.Second):
			t.Fatal("uploads did not finish")
		}
	}
	require.Equal(t, 1, cred.mintCount())
	var retried int
	for _, a := range s.authorizations() {
		if strings.HasSuffix(a, "Bearer jwt-b") {
			retried++
		}
	}
	require.Equal(t, uploads, retried)
}
