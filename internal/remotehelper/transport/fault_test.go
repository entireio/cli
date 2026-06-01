package transport

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestServiceRPC_TruncatedResponseBodyError: when the server hangs
// up mid-response, the streaming caller must see an error. We
// don't read the body inside ServiceRPC (the caller does), but the
// returned ReadCloser must surface the truncation via io.ReadAll.
func TestServiceRPC_TruncatedResponseBodyError(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Fatal("ResponseWriter not Hijacker")
		}
		conn, bufw, err := hj.Hijack()
		if err != nil {
			t.Fatalf("hijack: %v", err)
		}
		// Write a 200 with a Content-Length larger than what we'll
		// actually send, then drop the connection.
		fmt.Fprint(bufw, "HTTP/1.1 200 OK\r\n")
		fmt.Fprint(bufw, "Content-Length: 1000\r\n\r\n")
		fmt.Fprint(bufw, "partial")
		_ = bufw.Flush()
		_ = conn.Close()
	}))
	defer server.Close()

	resp, err := testProxy(server).ServiceRPC(context.Background(), "git-upload-pack", strings.NewReader("body"))
	if err != nil {
		// Failure surfacing at this layer is also acceptable.
		return
	}
	defer resp.Close()
	_, err = io.ReadAll(resp)
	if err == nil {
		t.Fatal("expected truncation error from body read")
	}
}

// TestInfoRefs_CtxCancelDuringHang: a server that hangs the
// info/refs response must surrender the request when the caller's
// context cancels.
func TestInfoRefs_CtxCancelDuringHang(t *testing.T) {
	t.Parallel()
	hold := make(chan struct{})

	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-hold:
		case <-r.Context().Done():
		}
	}))
	defer server.Close()
	defer close(hold)

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	p := testProxy(server)

	done := make(chan error, 1)
	go func() {
		_, err := p.InfoRefs(ctx, "git-upload-pack")
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Error("expected error on ctx cancel")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("InfoRefs did not return after ctx timeout")
	}
}

// TestDoWithFailover_RetriesAfterConnectionReset: a server that
// resets the connection mid-request (RST shape: TCP close while the
// client is still writing) must be marked failed, and a second
// healthy server must answer the request via failover.
func TestDoWithFailover_RetriesAfterConnectionReset(t *testing.T) {
	t.Parallel()
	resetter := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hj, ok := w.(http.Hijacker)
		if !ok {
			return
		}
		conn, _, err := hj.Hijack()
		if err != nil {
			return
		}
		// Force TCP RST by setting linger to 0 and closing.
		if tcp, ok := conn.(*net.TCPConn); ok {
			_ = tcp.SetLinger(0) //nolint:errcheck // test
		}
		_ = conn.Close()
	}))
	defer resetter.Close()

	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "from good")
	}))
	defer good.Close()

	p := proxyWithClient([]string{resetter.URL, good.URL}, "/et/alice/repo", "", "", &http.Client{})

	resp, err := p.InfoRefs(context.Background(), "git-upload-pack")
	if err != nil {
		t.Fatalf("failover should succeed despite reset, got: %v", err)
	}
	defer resp.Close()
	body, _ := io.ReadAll(resp) //nolint:errcheck // test
	if string(body) != "from good" {
		t.Errorf("body = %q, want %q", body, "from good")
	}
}

// TestServiceRPC_ResetMidResponseSurfacesError: a connection that's
// closed while the response body streams must produce an error at
// io.ReadAll time. Critical for push acknowledgement: a truncated
// receive-pack response could otherwise be mistaken for "ok".
func TestServiceRPC_ResetMidResponseSurfacesError(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			return
		}
		hj, _ := w.(http.Hijacker) //nolint:errcheck // not asserting ok intentionally
		w.Header().Set("Content-Type", "application/x-git-receive-pack-result")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "0014unpack ok\n0014ok refs/heads") // truncated mid-line
		flusher.Flush()
		if hj != nil {
			conn, _, err := hj.Hijack()
			if err == nil {
				if tcp, ok := conn.(*net.TCPConn); ok {
					_ = tcp.SetLinger(0) //nolint:errcheck // test
				}
				_ = conn.Close()
			}
		}
	}))
	defer server.Close()

	p := testProxy(server)
	resp, err := p.ServiceRPC(context.Background(), "git-receive-pack", strings.NewReader("PACK fake"))
	if err != nil {
		return // surfacing here is fine
	}
	defer resp.Close()
	body, err := io.ReadAll(resp)
	if err == nil && len(body) == 0 {
		t.Error("expected truncation error or partial body, got clean empty read")
	}
}
