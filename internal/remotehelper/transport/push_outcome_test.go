package transport

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/internal/remotehelper/replicas"
)

// pushNode is a test data-plane node that records every receive-pack POST
// and how many body bytes it read, then answers through handle.
type pushNode struct {
	srv    *httptest.Server
	posts  atomic.Int32
	mu     sync.Mutex
	bodies []int
}

func newPushNode(t *testing.T, handle http.HandlerFunc) *pushNode {
	t.Helper()
	n := &pushNode{}
	n.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		n.posts.Add(1)
		if handle != nil {
			handle(w, r)
			return
		}
		body, _ := io.ReadAll(r.Body) //nolint:errcheck // test node; a short read is recorded by length
		n.mu.Lock()
		n.bodies = append(n.bodies, len(body))
		n.mu.Unlock()
		w.Header().Set("Content-Type", "application/x-git-receive-pack-result")
		_, _ = io.WriteString(w, "report") //nolint:errcheck // test
	}))
	t.Cleanup(n.srv.Close)
	return n
}

func (n *pushNode) url() string { return n.srv.URL }

// readAllThen reads the whole request body before it runs then.
func readAllThen(then func(w http.ResponseWriter)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body) //nolint:errcheck // test
		then(w)
	}
}

// dropConnection closes the connection with a TCP reset and sends no
// response.
func dropConnection(w http.ResponseWriter) {
	conn, _, err := http.NewResponseController(w).Hijack()
	if err != nil {
		panic(err)
	}
	if tcp, ok := conn.(*net.TCPConn); ok {
		_ = tcp.SetLinger(0) //nolint:errcheck // test
	}
	_ = conn.Close()
}

// pushProxy builds a proxy over nodes that tries them in order: the first
// node is sticky, and failover walks the list from there.
func pushProxy(t *testing.T, onFailed func(string), nodes ...*pushNode) *Proxy {
	t.Helper()
	urls := make([]string, len(nodes))
	for i, n := range nodes {
		urls[i] = n.url()
	}
	p := New(Config{
		Nodes: replicas.NodeConfig{
			InitialNodes: urls,
			ClusterHost:  mustHost(t, urls[0]),
		},
		Path:         "/et/owner/repo",
		OnNodeFailed: onFailed,
	})
	p.stickyNode = urls[0]
	return p
}

func receivePack(p *Proxy, body []byte) (string, error) {
	resp, err := p.ServiceRPC(context.Background(), "git-receive-pack", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	defer resp.Close()
	out, err := io.ReadAll(resp)
	return string(out), err
}

func posts(nodes ...*pushNode) []int32 {
	out := make([]int32, len(nodes))
	for i, n := range nodes {
		out[i] = n.posts.Load()
	}
	return out
}

// A large body exceeds what loopback socket buffers can absorb, so a node
// that stops reading early leaves most of it unread by the transport.
var largePush = bytes.Repeat([]byte("p"), 64<<20)

func TestPush_FailsOverWhenNodeIsUnreachable(t *testing.T) {
	t.Parallel()
	dead := newPushNode(t, nil)
	dead.srv.Close()
	good := newPushNode(t, nil)
	var failed []string
	p := pushProxy(t, func(n string) { failed = append(failed, n) }, dead, good)

	out, err := receivePack(p, []byte("commands+pack"))
	require.NoError(t, err)
	assert.Equal(t, "report", out)
	assert.Equal(t, []int32{0, 1}, posts(dead, good))
	assert.Equal(t, []int{len("commands+pack")}, good.bodies, "the resent body is complete")
	assert.Equal(t, []string{dead.url()}, failed)
}

func TestPush_FailsOverWhenConnectionDropsDuringUpload(t *testing.T) {
	t.Parallel()
	dropper := newPushNode(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.CopyN(io.Discard, r.Body, 64<<10) //nolint:errcheck // test
		dropConnection(w)
	})
	good := newPushNode(t, nil)
	p := pushProxy(t, nil, dropper, good)

	_, err := receivePack(p, largePush)
	require.NoError(t, err)
	assert.Equal(t, []int32{1, 1}, posts(dropper, good))
	assert.Equal(t, []int{len(largePush)}, good.bodies)
}

func TestPush_FailsOverWhenNodeAnswersBeforeUploadCompletes(t *testing.T) {
	t.Parallel()
	early := newPushNode(t, func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "node is shutting down", http.StatusServiceUnavailable)
	})
	good := newPushNode(t, nil)
	p := pushProxy(t, nil, early, good)

	_, err := receivePack(p, largePush)
	require.NoError(t, err)
	assert.Equal(t, []int32{1, 1}, posts(early, good))
}

func TestPush_NotResentWhenResponseIsLostAfterUpload(t *testing.T) {
	t.Parallel()
	committer := newPushNode(t, readAllThen(dropConnection))
	other := newPushNode(t, nil)
	var failed []string
	p := pushProxy(t, func(n string) { failed = append(failed, n) }, committer, other)

	_, err := receivePack(p, []byte("commands+pack"))
	require.ErrorIs(t, err, ErrPushOutcomeUnknown)
	assert.Contains(t, err.Error(), "lags the primary", "the error carries the recovery guidance")
	assert.Equal(t, []int32{1, 0}, posts(committer, other), "a push that may have landed is never resent")
	assert.Empty(t, failed, "an ambiguous push does not evict a node")
}

func TestPush_NotResentOnIndeterminateOutcome(t *testing.T) {
	t.Parallel()
	var nodes []*pushNode
	nodes = append(nodes, newPushNode(t, readAllThen(func(w http.ResponseWriter) {
		w.Header().Set(PushOutcomeHeader, "indeterminate")
		http.Error(w, "the connection to the primary failed; the push may have been applied", http.StatusBadGateway)
	})))
	nodes = append(nodes, newPushNode(t, nil), newPushNode(t, nil))
	p := pushProxy(t, nil, nodes...)

	_, err := receivePack(p, []byte("commands+pack"))
	require.ErrorIs(t, err, ErrPushOutcomeUnknown)
	assert.Contains(t, err.Error(), "the push may have been applied")
	assert.Equal(t, []int32{1, 0, 0}, posts(nodes...))
	assert.Len(t, p.nodes, 3, "the cached replica set is kept")
}

// An indeterminate marker wins over every other signal, including a node
// that answered before it read the whole body.
func TestPush_IndeterminateOutcomeWinsOverEarlyAnswer(t *testing.T) {
	t.Parallel()
	early := newPushNode(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set(PushOutcomeHeader, "indeterminate")
		http.Error(w, "unknown", http.StatusUnauthorized)
	})
	other := newPushNode(t, nil)
	p := pushProxy(t, nil, early, other)

	_, err := receivePack(p, largePush)
	require.ErrorIs(t, err, ErrPushOutcomeUnknown)
	assert.Equal(t, []int32{1, 0}, posts(early, other))
}

func TestPush_NotResentOnUnmarked5xxAfterUpload(t *testing.T) {
	t.Parallel()
	for _, status := range []int{http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			t.Parallel()
			failing := newPushNode(t, readAllThen(func(w http.ResponseWriter) {
				http.Error(w, "upstream failed", status)
			}))
			other := newPushNode(t, nil)
			p := pushProxy(t, nil, failing, other)

			_, err := receivePack(p, []byte("commands+pack"))
			require.ErrorIs(t, err, ErrPushOutcomeUnknown)
			assert.Contains(t, err.Error(), "upstream failed")
			assert.Equal(t, []int32{1, 0}, posts(failing, other))
		})
	}
}

func TestPush_FailsOverOnNotAppliedOutcome(t *testing.T) {
	t.Parallel()
	refused := newPushNode(t, readAllThen(func(w http.ResponseWriter) {
		w.Header().Set(PushOutcomeHeader, "not-applied")
		http.Error(w, "primary unavailable", http.StatusServiceUnavailable)
	}))
	good := newPushNode(t, nil)
	p := pushProxy(t, nil, refused, good)

	out, err := receivePack(p, []byte("commands+pack"))
	require.NoError(t, err)
	assert.Equal(t, "report", out)
	assert.Equal(t, []int32{1, 1}, posts(refused, good))
}

func TestPush_RefusesRedirect(t *testing.T) {
	t.Parallel()
	target := newPushNode(t, nil)
	redirector := newPushNode(t, readAllThen(func(w http.ResponseWriter) {
		w.Header().Set("Location", strings.Replace(target.url(), "http://", "http://x:secret-token@", 1)+"/et/owner/repo/git-receive-pack")
		w.WriteHeader(http.StatusTemporaryRedirect)
	}))
	other := newPushNode(t, nil)
	p := pushProxy(t, nil, redirector, other)

	_, err := receivePack(p, []byte("commands+pack"))
	require.Error(t, err)
	require.NotErrorIs(t, err, ErrPushOutcomeUnknown, "a redirect is the node's final answer")
	assert.Contains(t, err.Error(), "redirect")
	assert.NotContains(t, err.Error(), "secret-token")
	assert.Equal(t, []int32{1, 0, 0}, posts(redirector, target, other))
}

func TestPush_ResendsOnceAfter401(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	node := newPushNode(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body) //nolint:errcheck // test
		if calls.Add(1) == 1 {
			http.Error(w, "expired", http.StatusUnauthorized)
			return
		}
		_, _ = w.Write(body) //nolint:errcheck // test
	})
	p := pushProxy(t, nil, node)

	out, err := receivePack(p, []byte("commands+pack"))
	require.NoError(t, err)
	assert.Equal(t, "commands+pack", out, "the resend carries the whole body")
	assert.Equal(t, int32(2), node.posts.Load())
}

func TestPush_LostResponseAfter401ResendIsUnknown(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	node := newPushNode(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body) //nolint:errcheck // test
		if calls.Add(1) == 1 {
			http.Error(w, "expired", http.StatusUnauthorized)
			return
		}
		dropConnection(w)
	})
	other := newPushNode(t, nil)
	p := pushProxy(t, nil, node, other)

	_, err := receivePack(p, []byte("commands+pack"))
	require.ErrorIs(t, err, ErrPushOutcomeUnknown)
	assert.Equal(t, []int32{2, 0}, posts(node, other))
}

// Reads carry no side effects, so they keep failing over on every 5xx.
func TestUploadPack_StillFailsOverOn5xxAfterUpload(t *testing.T) {
	t.Parallel()
	failing := newPushNode(t, readAllThen(func(w http.ResponseWriter) {
		http.Error(w, "boom", http.StatusBadGateway)
	}))
	good := newPushNode(t, nil)
	p := pushProxy(t, nil, failing, good)

	resp, err := p.ServiceRPC(context.Background(), "git-upload-pack", bytes.NewReader([]byte("want")))
	require.NoError(t, err)
	require.NoError(t, resp.Close())
	assert.Equal(t, []int32{1, 1}, posts(failing, good))
}

func TestPushOutcomeUnknown_WrapsOnce(t *testing.T) {
	t.Parallel()
	cause := errors.New("reset")
	once := PushOutcomeUnknown(cause)
	twice := PushOutcomeUnknown(once)
	require.Same(t, once, twice)
	require.ErrorIs(t, twice, cause)
	assert.Equal(t, 1, strings.Count(twice.Error(), "lags the primary"))
}
