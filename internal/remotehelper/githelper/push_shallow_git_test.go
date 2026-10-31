package githelper

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// TestHandlePush_RealSendPack_ShallowRepoAmendsAgent pushes from a shallow
// clone with the real git send-pack, pinning the protocol shape the agent
// amend relies on: the client's shallow lines come ahead of the first
// command, and the helper agent belongs on that command's capabilities.
// Enough shallow lines to push the command across send-pack's first
// 65515-byte payload are covered in gitproto, where the framing is built
// directly instead of from a repository with over a thousand grafts.
func TestHandlePush_RealSendPack_ShallowRepoAmendsAgent(t *testing.T) {
	// No t.Parallel(): t.Chdir, because send-pack runs in the process cwd.
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	src, runSrc := newRealGitRepo(t)
	for i := range 3 {
		runSrc("commit", "-q", "--allow-empty", "-m", fmt.Sprintf("c%d", i))
	}
	dir := filepath.Join(t.TempDir(), "shallow")
	runSrc("clone", "-q", "--depth", "1", "file://"+src, dir)
	run := func(args ...string) string {
		t.Helper()
		base := []string{"-C", dir, "-c", "user.name=test", "-c", "user.email=test@example.com", "-c", "commit.gpgsign=false"}
		out, err := exec.CommandContext(context.Background(), "git", append(base, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	run("commit", "-q", "--allow-empty", "-m", "on shallow")
	head := run("rev-parse", "HEAD")
	t.Chdir(dir)

	var mu sync.Mutex
	var body []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "info/refs"):
			w.Header().Set("Content-Type", "application/x-git-receive-pack-advertisement")
			fmt.Fprint(w, serviceAnnouncement(serviceReceivePack,
				strings.Repeat("0", 40)+" capabilities^{}\x00report-status ofs-delta shallow agent=test-server\n"))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, serviceReceivePack):
			got, _ := io.ReadAll(r.Body) //nolint:errcheck // test server
			mu.Lock()
			body = got
			mu.Unlock()
			w.Header().Set("Content-Type", "application/x-git-receive-pack-result")
			fmt.Fprint(w, pktLine("unpack ok\n"))
			fmt.Fprint(w, pktLine("ok "+testRefMain+"\n"))
			fmt.Fprint(w, "0000")
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	var stdout bytes.Buffer
	err := handlePush(t.Context(), testTransport(server), &refAdvCache{}, "push "+head+":"+testRefMain,
		&Options{}, bufio.NewReader(strings.NewReader("\n")), &stdout)
	if err != nil {
		t.Fatalf("handlePush: %v\nstdout: %s", err, stdout.String())
	}

	mu.Lock()
	defer mu.Unlock()
	if !bytes.HasPrefix(body[4:], []byte("shallow ")) {
		t.Fatalf("fixture: send-pack did not lead with a shallow line: %q", body[:min(len(body), 80)])
	}
	caps := bytes.Index(body, []byte{0})
	if caps < 0 {
		t.Fatal("request carries no capabilities")
	}
	line := body[caps:]
	line = line[:bytes.IndexByte(line, '\n')]
	if !bytes.Contains(line, []byte("+"+Agent)) {
		t.Errorf("capabilities %q lack the helper agent %q", line, Agent)
	}
}
