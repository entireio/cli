package githelper

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// sizeLimitedReceivePack is an httptest receive-pack endpoint that enforces a
// declared-size limit the way entiredb's Gate 1 does: a push declaring more
// than limit gets 413 "push rejected: declared size N exceeds limit M" before
// its body is read.
type sizeLimitedReceivePack struct {
	limit int64

	mu           sync.Mutex
	declared     string
	contentLen   int64
	bodyRead     int64
	refusedEarly bool
}

func (s *sizeLimitedReceivePack) handler(t *testing.T, ref string) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "info/refs"):
			// An empty repository: the capabilities line on the zero id.
			w.Header().Set("Content-Type", "application/x-git-receive-pack-advertisement")
			fmt.Fprint(w, serviceAnnouncement(serviceReceivePack,
				strings.Repeat("0", 40)+" capabilities^{}\x00report-status ofs-delta\n"))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, serviceReceivePack):
			s.mu.Lock()
			s.declared = r.Header.Get("X-Entire-Push-Size")
			s.contentLen = r.ContentLength
			s.mu.Unlock()
			if n, err := strconv.ParseInt(s.declared, 10, 64); err == nil && n > s.limit {
				s.mu.Lock()
				s.refusedEarly = true
				s.mu.Unlock()
				http.Error(w, fmt.Sprintf("push rejected: declared size %d exceeds limit %d", n, s.limit), http.StatusRequestEntityTooLarge)
				return
			}
			n, _ := io.Copy(io.Discard, r.Body) //nolint:errcheck // test server
			s.mu.Lock()
			s.bodyRead = n
			s.mu.Unlock()
			w.Header().Set("Content-Type", "application/x-git-receive-pack-result")
			fmt.Fprint(w, pktLine("unpack ok\n"))
			fmt.Fprint(w, pktLine("ok "+ref+"\n"))
			fmt.Fprint(w, "0000")
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	})
}

// newPushRepo makes an isolated repository (see newRealGitRepo) with one
// commit holding size bytes of random data, and returns its path and HEAD.
func newPushRepo(t *testing.T, size int) (string, string) {
	t.Helper()
	dir, run := newRealGitRepo(t)
	// Random data does not compress, so the pack stays near size.
	data := make([]byte, size)
	if _, err := rand.Read(data); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "blob.bin"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	run("add", "blob.bin")
	run("commit", "-q", "-m", "data")
	return dir, run("rev-parse", "HEAD")
}

// TestHandlePush_RealSendPack_DeclaresSizeAndLands drives handlePush with the
// real git send-pack: the POST declares the exact body length and a push under
// the limit lands normally.
func TestHandlePush_RealSendPack_DeclaresSizeAndLands(t *testing.T) {
	// No t.Parallel(): t.Chdir, because send-pack runs in the process cwd.
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	dir, head := newPushRepo(t, 256<<10)
	t.Chdir(dir)
	srv := &sizeLimitedReceivePack{limit: 64 << 20}
	server := httptest.NewServer(srv.handler(t, testRefMain))
	defer server.Close()

	var stdout bytes.Buffer
	err := handlePush(t.Context(), testTransport(server), &refAdvCache{}, "push "+head+":"+testRefMain,
		&Options{}, bufio.NewReader(strings.NewReader("\n")), &stdout)
	if err != nil {
		t.Fatalf("handlePush: %v\nstdout: %s", err, stdout.String())
	}
	srv.mu.Lock()
	defer srv.mu.Unlock()
	if srv.declared == "" {
		t.Fatal("real send-pack push sent no X-Entire-Push-Size")
	}
	if want := strconv.FormatInt(srv.bodyRead, 10); srv.declared != want {
		t.Errorf("X-Entire-Push-Size = %s, body read = %s", srv.declared, want)
	}
	if srv.contentLen >= 0 && strconv.FormatInt(srv.contentLen, 10) != srv.declared {
		t.Errorf("Content-Length %d disagrees with X-Entire-Push-Size %s", srv.contentLen, srv.declared)
	}
	if !strings.Contains(stdout.String(), "ok "+testRefMain) {
		t.Errorf("push not reported ok: %q", stdout.String())
	}
}

// TestHandlePush_RealSendPack_OverLimitRefusedWithMarker is the production
// failure this fixes: a push over the server's limit is refused from its
// declared size, before the body is read, and surfaces the stable marker
// instead of a dropped connection.
func TestHandlePush_RealSendPack_OverLimitRefusedWithMarker(t *testing.T) {
	// No t.Parallel(): t.Chdir, because send-pack runs in the process cwd.
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	dir, head := newPushRepo(t, 256<<10)
	t.Chdir(dir)
	srv := &sizeLimitedReceivePack{limit: 32 << 10}
	server := httptest.NewServer(srv.handler(t, testRefMain))
	defer server.Close()

	var stdout bytes.Buffer
	err := handlePush(t.Context(), testTransport(server), &refAdvCache{}, "push "+head+":"+testRefMain,
		&Options{}, bufio.NewReader(strings.NewReader("\n")), &stdout)
	if err == nil {
		t.Fatalf("over-limit push succeeded; stdout: %s", stdout.String())
	}
	srv.mu.Lock()
	defer srv.mu.Unlock()
	if !srv.refusedEarly {
		t.Fatal("server did not refuse from the declared size")
	}
	want := fmt.Sprintf("entire: push too large: size %s exceeds limit %d", srv.declared, srv.limit)
	if !strings.Contains(err.Error(), want) {
		t.Errorf("error = %v\nwant it to contain %q", err, want)
	}
	if strings.Contains(stdout.String(), "ok "+testRefMain) {
		t.Errorf("refused push reported ok: %q", stdout.String())
	}
}
