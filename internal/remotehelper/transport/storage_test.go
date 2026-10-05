package transport

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/entireio/cli/internal/remotehelper/replicas"
)

const diskFullMsg = "push rejected: insufficient disk space (free=1, threshold=10, push=5)"

func diskFullServer() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, diskFullMsg, http.StatusInsufficientStorage)
	}))
}

func recordingProxy(t *testing.T, nodes []string) (*Proxy, *[]string) {
	t.Helper()
	var mu sync.Mutex
	var failed []string
	p := New(Config{
		Nodes:        replicas.NodeConfig{InitialNodes: nodes, ClusterHost: mustHost(t, nodes[0])},
		Path:         "/et/alice/repo",
		OnNodeFailed: func(node string) { mu.Lock(); failed = append(failed, node); mu.Unlock() },
	})
	return p, &failed
}

// A node refusing a push for lack of disk is healthy: it must not be marked
// failed (which purges the persisted replica set) and the user must get the
// server's reason, not "all N nodes failed".
func TestInsufficientStorageIsNotANodeFailure(t *testing.T) {
	t.Parallel()
	full := diskFullServer()
	defer full.Close()
	p, failed := recordingProxy(t, []string{full.URL})

	_, err := p.ServiceRPC(context.Background(), "git-receive-pack", strings.NewReader("body"))

	var storage *InsufficientStorageError
	if !errors.As(err, &storage) {
		t.Fatalf("want *InsufficientStorageError, got %T: %v", err, err)
	}
	if !strings.Contains(err.Error(), "low on disk space") || !strings.Contains(err.Error(), "free=1") {
		t.Errorf("error = %v", err)
	}
	if strings.Contains(err.Error(), "nodes failed") {
		t.Errorf("disk-full reported as node failure: %v", err)
	}
	if len(*failed) != 0 {
		t.Errorf("onNodeFailed = %v, want none", *failed)
	}
	if len(p.nodes) != 1 {
		t.Errorf("node removed after a disk-full refusal: %v", p.nodes)
	}
}

// Another replica may have room, so the push moves on without marking the
// full one failed.
func TestInsufficientStorageAdvancesToNextReplica(t *testing.T) {
	t.Parallel()
	full := diskFullServer()
	defer full.Close()
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body) //nolint:errcheck // test
		fmt.Fprint(w, "ok")
	}))
	defer ok.Close()
	p, failed := recordingProxy(t, []string{full.URL, ok.URL})
	// doWithFailover starts at a random node; pin the full one so the 507
	// branch is exercised on every run.
	p.stickyNode = full.URL

	resp, err := p.ServiceRPC(context.Background(), "git-receive-pack", strings.NewReader("body"))
	if err != nil {
		t.Fatalf("expected the second replica to take the push: %v", err)
	}
	defer resp.Close()
	if len(*failed) != 0 {
		t.Errorf("onNodeFailed = %v, want none", *failed)
	}
}

// 413 means "push too large" only on receive-pack; on a fetch it is some
// intermediary's limit and keeps the generic error.
func TestServiceRPC_413IsPushTooLargeOnlyForReceivePack(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "push rejected: declared size 9 exceeds limit 5", http.StatusRequestEntityTooLarge)
	}))
	defer srv.Close()
	p := proxyWithClient([]string{srv.URL}, "/et/alice/repo", "", "", &http.Client{})

	var tooLarge *PushTooLargeError
	_, err := p.ServiceRPC(context.Background(), "git-receive-pack", strings.NewReader("x"))
	if !errors.As(err, &tooLarge) || err.Error() != "entire: push too large: size 9 exceeds limit 5" {
		t.Errorf("receive-pack 413: %T %v", err, err)
	}
	_, err = p.ServiceRPC(context.Background(), "git-upload-pack", strings.NewReader("x"))
	if errors.As(err, &tooLarge) || strings.Contains(err.Error(), "push too large") {
		t.Errorf("upload-pack 413 reported as a push: %v", err)
	}
}
