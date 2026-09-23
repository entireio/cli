package main

// End-to-end push-outcome tests. Real git pushes through the built
// git-remote-entire binary to TLS nodes backed by `git http-backend`, so the
// server is real receive-pack. Faults are injected around it. Each test
// asserts how many receive-pack POSTs reached the nodes, and that a push
// whose result is unknown is never resent or reported as a success.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var helperBuild struct {
	once sync.Once
	dir  string
	err  error
}

// helperDir builds git-remote-entire once per test binary and returns the
// directory that holds it.
func helperDir(t *testing.T) string {
	t.Helper()
	helperBuild.once.Do(func() {
		dir, err := os.MkdirTemp("", "git-remote-entire-test-") //nolint:usetesting // the build outlives any one test; TestMain removes it
		if err != nil {
			helperBuild.err = err
			return
		}
		helperBuild.dir = dir
		out, err := exec.CommandContext(context.Background(), "go", "build", "-o", filepath.Join(dir, "git-remote-entire"), ".").CombinedOutput()
		if err != nil {
			helperBuild.err = fmt.Errorf("go build: %w\n%s", err, out)
		}
	})
	require.NoError(t, helperBuild.err)
	return helperBuild.dir
}

func TestMain(m *testing.M) {
	code := m.Run()
	if helperBuild.dir != "" {
		_ = os.RemoveAll(helperBuild.dir)
	}
	os.Exit(code)
}

const pushRepoPath = "/et/owner/repo"

// pushFault decides how a node answers one receive-pack POST. serve runs the
// real receive-pack and returns its complete response.
type pushFault func(w http.ResponseWriter, serve func() *httptest.ResponseRecorder)

// pushCluster is a set of nodes that serve one bare repository.
type pushCluster struct {
	root  string
	bare  string
	nodes []*httptest.Server
	posts atomic.Int32
	// faults[i] answers the i-th POST across all nodes; later POSTs are
	// served normally.
	faults []pushFault
}

func newPushCluster(t *testing.T, nodeCount int, faults ...pushFault) *pushCluster {
	t.Helper()
	c := &pushCluster{root: t.TempDir(), faults: faults}
	c.bare = filepath.Join(c.root, filepath.FromSlash(strings.TrimPrefix(pushRepoPath, "/")))
	runGit(t, "", "init", "-q", "--bare", "--initial-branch=main", c.bare)

	gitPath, err := exec.LookPath("git")
	require.NoError(t, err)
	backend := &cgi.Handler{
		Path: gitPath,
		Args: []string{"http-backend"},
		Env:  []string{"GIT_PROJECT_ROOT=" + c.root, "GIT_HTTP_EXPORT_ALL=1", "REMOTE_USER=tester"},
	}
	for range nodeCount {
		c.nodes = append(c.nodes, httptest.NewUnstartedServer(nil))
	}
	for _, n := range c.nodes {
		n.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			c.serve(t, backend, w, r)
		})
		n.StartTLS()
		t.Cleanup(n.Close)
	}
	return c
}

func (c *pushCluster) serve(t *testing.T, backend http.Handler, w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/.well-known/entire-cluster.json" {
		core := c.nodes[0].URL
		err := json.NewEncoder(w).Encode(struct {
			CoreURLs             []string `json:"core_urls"`
			JurisdictionAudience string   `json:"jurisdiction_audience"`
			JurisdictionCoreURL  string   `json:"jurisdiction_core_url"`
		}{[]string{core}, core, core})
		assert.NoError(t, err)
		return
	}
	if strings.HasSuffix(r.URL.Path, "/info/refs") {
		urls := make([]string, len(c.nodes))
		for i, n := range c.nodes {
			urls[i] = n.URL
		}
		w.Header().Set("X-Entire-Replicas", strings.Join(urls, ","))
		backend.ServeHTTP(w, r)
		return
	}
	if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/git-receive-pack") {
		backend.ServeHTTP(w, r)
		return
	}
	i := int(c.posts.Add(1)) - 1
	serve := func() *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		backend.ServeHTTP(rec, r)
		return rec
	}
	if i < len(c.faults) && c.faults[i] != nil {
		c.faults[i](w, serve)
		return
	}
	relay(t, w, serve())
}

func relay(t *testing.T, w http.ResponseWriter, rec *httptest.ResponseRecorder) {
	t.Helper()
	for k, v := range rec.Header() {
		w.Header()[k] = v
	}
	w.WriteHeader(rec.Code)
	_, _ = w.Write(rec.Body.Bytes()) //nolint:errcheck // test relay
}

// resetConnection ends the exchange with a TCP reset.
func resetConnection(w http.ResponseWriter) {
	conn, _, err := http.NewResponseController(w).Hijack()
	if err != nil {
		panic(err)
	}
	if tcp, ok := conn.(*net.TCPConn); ok {
		_ = tcp.SetLinger(0) //nolint:errcheck // test
	}
	_ = conn.Close()
}

// applyThenLoseResponse applies the push and drops the connection before
// any response byte.
func applyThenLoseResponse(w http.ResponseWriter, serve func() *httptest.ResponseRecorder) {
	serve()
	resetConnection(w)
}

// applyThenIndeterminate applies the push and answers as entiredb's proxy
// does when its connection to the primary fails after the upload.
func applyThenIndeterminate(w http.ResponseWriter, serve func() *httptest.ResponseRecorder) {
	serve()
	w.Header().Set("X-Entire-Push-Outcome", "indeterminate")
	http.Error(w, "the connection to the primary failed after the push was sent; the push may have been applied, so fetch before you retry", http.StatusBadGateway)
}

// applyThenTruncateReport applies the push and sends only the start of its
// report-status: the unpack line, then a reset.
func applyThenTruncateReport(w http.ResponseWriter, serve func() *httptest.ResponseRecorder) {
	rec := serve()
	body := rec.Body.Bytes()
	cut := bytes.Index(body, []byte("unpack ok"))
	if cut < 0 {
		panic(fmt.Sprintf("no unpack line in report %q", body))
	}
	w.Header().Set("Content-Type", rec.Header().Get("Content-Type"))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body[:cut+len("unpack ok\n")+8]) //nolint:errcheck // test
	_ = http.NewResponseController(w).Flush()
	resetConnection(w)
}

// refuseBeforeUpload answers as entiredb's proxy does when the primary is
// unreachable before any byte of the push was forwarded.
func refuseBeforeUpload(w http.ResponseWriter, _ func() *httptest.ResponseRecorder) {
	w.Header().Set("X-Entire-Push-Outcome", "not-applied")
	w.Header().Set("Retry-After", "5")
	http.Error(w, "the primary is unavailable; the push was not applied", http.StatusServiceUnavailable)
}

type pushClient struct {
	t   *testing.T
	dir string
	env []string
	url string
}

func newPushClient(t *testing.T, c *pushCluster) *pushClient {
	t.Helper()
	home := t.TempDir()
	host := strings.TrimPrefix(c.nodes[0].URL, "https://")
	env := append(os.Environ(),
		"PATH="+helperDir(t)+string(os.PathListSeparator)+os.Getenv("PATH"),
		"HOME="+home,
		"XDG_CACHE_HOME="+filepath.Join(home, "cache"),
		"XDG_CONFIG_HOME="+filepath.Join(home, "config"),
		"ENTIRE_CONFIG_DIR="+filepath.Join(home, "entire"),
		"ENTIRE_TOKEN_STORE=file",
		"ENTIRE_TOKEN_STORE_PATH="+filepath.Join(home, "tokens.json"),
		"ENTIRE_TEST_AUTH_STORE_FILE="+filepath.Join(home, "auth.json"),
		"ENTIRE_TLS_SKIP_VERIFY=true",
		"ENTIRE_TOKEN="+makeTestJWT(t, c.nodes[0].URL),
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL="+filepath.Join(home, "gitconfig"),
		"GIT_TERMINAL_PROMPT=0",
	)
	p := &pushClient{t: t, dir: t.TempDir(), env: env, url: "entire://" + host + pushRepoPath}
	p.git("init", "-q", "--initial-branch=main")
	p.git("config", "user.name", "tester")
	p.git("config", "user.email", "tester@example.com")
	p.git("remote", "add", "origin", p.url)
	return p
}

func (p *pushClient) run(args ...string) (string, error) {
	cmd := exec.CommandContext(p.t.Context(), "git", args...)
	cmd.Dir = p.dir
	cmd.Env = p.env
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func (p *pushClient) git(args ...string) string {
	p.t.Helper()
	out, err := p.run(args...)
	require.NoError(p.t, err, out)
	return strings.TrimSpace(out)
}

func (p *pushClient) commit(msg string) string {
	p.t.Helper()
	p.git("commit", "-q", "--allow-empty", "-m", msg)
	return p.git("rev-parse", "HEAD")
}

func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, string(out))
	return strings.TrimSpace(string(out))
}

func remoteRef(t *testing.T, c *pushCluster, ref string) string {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", "--git-dir", c.bare, "rev-parse", "--verify", "-q", ref)
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func trackingRef(p *pushClient, ref string) string {
	out, err := p.run("rev-parse", "--verify", "-q", ref)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

// Each case runs through both helper paths for receive-pack. The helper
// reads the protocol version from GIT_PROTOCOL: version=1 selects connect,
// and version=2 (or none) selects push through send-pack.
var pushProtocols = map[string]string{"connect": "version=1", "push": "version=2"}

// pathCommand is the helper command each path logs under ENTIRE_DEBUG.
var pathCommand = map[string]string{
	"connect": `command: "connect git-receive-pack"`,
	"push":    `command: "push refs/heads/main:refs/heads/main"`,
}

func forEachProtocol(t *testing.T, fn func(t *testing.T, path string)) {
	t.Helper()
	for path := range pushProtocols {
		t.Run(path, func(t *testing.T) {
			t.Parallel()
			fn(t, path)
		})
	}
}

func (p *pushClient) push(path string, args ...string) (string, error) {
	cmd := exec.CommandContext(p.t.Context(), "git", append([]string{"push", "--porcelain", "origin"}, args...)...)
	cmd.Dir = p.dir
	cmd.Env = append(slices.Clone(p.env), "GIT_PROTOCOL="+pushProtocols[path])
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func TestPushOutcome_BaselinePushSucceeds(t *testing.T) {
	t.Parallel()
	forEachProtocol(t, func(t *testing.T, protocol string) {
		c := newPushCluster(t, 1)
		p := newPushClient(t, c)
		tip := p.commit("one")
		p.env = append(p.env, "ENTIRE_DEBUG=1")
		out, err := p.push(protocol, "main")
		require.NoError(t, err, out)
		assert.Contains(t, out, pathCommand[protocol], "the push takes the %s path", protocol)
		assert.Equal(t, tip, remoteRef(t, c, "refs/heads/main"))
		assert.Equal(t, int32(1), c.posts.Load())
	})
}

func TestPushOutcome_LostResponseAfterCommitIsNotResent(t *testing.T) {
	t.Parallel()
	forEachProtocol(t, func(t *testing.T, protocol string) {
		c := newPushCluster(t, 3, applyThenLoseResponse)
		p := newPushClient(t, c)
		tip := p.commit("one")

		out, err := p.push(protocol, "main")
		require.Error(t, err, out)
		assert.Contains(t, out, "push outcome unknown")
		assert.Contains(t, out, "lags the primary")
		assert.Equal(t, int32(1), c.posts.Load(), "a push that may have landed is never resent:\n%s", out)
		assert.Equal(t, tip, remoteRef(t, c, "refs/heads/main"), "the push did land")
		assert.Empty(t, trackingRef(p, "refs/remotes/origin/main"), "git must not record an unknown outcome as success")
	})
}

func TestPushOutcome_IndeterminateWithSeveralCachedNodesIsNotResent(t *testing.T) {
	t.Parallel()
	forEachProtocol(t, func(t *testing.T, protocol string) {
		c := newPushCluster(t, 3, nil, applyThenIndeterminate)
		p := newPushClient(t, c)
		first := p.commit("one")
		// The first push fills the helper's replica cache with all three
		// nodes, so the second push starts warm.
		out, err := p.push(protocol, "main")
		require.NoError(t, err, out)
		second := p.commit("two")

		out, err = p.push(protocol, "main")
		require.Error(t, err, out)
		assert.Contains(t, out, "push outcome unknown")
		assert.Contains(t, out, "the push may have been applied")
		assert.Equal(t, int32(2), c.posts.Load(), "one POST per push:\n%s", out)
		assert.Equal(t, second, remoteRef(t, c, "refs/heads/main"))
		assert.Equal(t, first, trackingRef(p, "refs/remotes/origin/main"), "the unknown push is not recorded")
	})
}

func TestPushOutcome_TruncatedReportStatusIsUnknown(t *testing.T) {
	t.Parallel()
	forEachProtocol(t, func(t *testing.T, protocol string) {
		c := newPushCluster(t, 2, applyThenTruncateReport)
		p := newPushClient(t, c)
		tip := p.commit("one")
		p.git("branch", "second")

		out, err := p.push(protocol, "main", "second")
		require.Error(t, err, out)
		assert.Contains(t, out, "push outcome unknown")
		assert.Equal(t, int32(1), c.posts.Load(), out)
		assert.Equal(t, tip, remoteRef(t, c, "refs/heads/main"))
		assert.NotContains(t, porcelainLines(out), "*\trefs/heads/main:refs/heads/main\t[new branch]",
			"a ref whose status was cut off is not reported as pushed")
		assert.Empty(t, trackingRef(p, "refs/remotes/origin/main"))
		assert.Empty(t, trackingRef(p, "refs/remotes/origin/second"))
	})
}

func TestPushOutcome_PartialMultiRefAcceptanceIsPreserved(t *testing.T) {
	t.Parallel()
	forEachProtocol(t, func(t *testing.T, protocol string) {
		c := newPushCluster(t, 2)
		hook := filepath.Join(c.bare, "hooks", "update")
		require.NoError(t, os.WriteFile(hook, []byte("#!/bin/sh\ncase \"$1\" in refs/heads/blocked) echo 'blocked by policy' >&2; exit 1;; esac\n"), 0o755))
		p := newPushClient(t, c)
		tip := p.commit("one")
		p.git("branch", "blocked")

		out, err := p.push(protocol, "main", "blocked")
		require.Error(t, err, out)
		lines := porcelainLines(out)
		assert.Contains(t, lines, "*\trefs/heads/main:refs/heads/main\t[new branch]")
		assert.Contains(t, lines, "!\trefs/heads/blocked:refs/heads/blocked\t[remote rejected] (hook declined)")
		assert.NotContains(t, out, "push outcome unknown", "a complete report is a known outcome")
		assert.Equal(t, int32(1), c.posts.Load())
		assert.Equal(t, tip, remoteRef(t, c, "refs/heads/main"))
		assert.Empty(t, remoteRef(t, c, "refs/heads/blocked"))
		assert.Equal(t, tip, trackingRef(p, "refs/remotes/origin/main"))
	})
}

func TestPushOutcome_NotAppliedFailsOverToAnotherNode(t *testing.T) {
	t.Parallel()
	forEachProtocol(t, func(t *testing.T, protocol string) {
		c := newPushCluster(t, 2, nil, refuseBeforeUpload)
		p := newPushClient(t, c)
		p.commit("one")
		out, err := p.push(protocol, "main")
		require.NoError(t, err, out)
		tip := p.commit("two")

		out, err = p.push(protocol, "main")
		require.NoError(t, err, out)
		assert.Equal(t, int32(3), c.posts.Load(), "the refused push is sent once more:\n%s", out)
		assert.Equal(t, tip, remoteRef(t, c, "refs/heads/main"))
		assert.Equal(t, tip, trackingRef(p, "refs/remotes/origin/main"))
	})
}

func porcelainLines(out string) []string {
	var lines []string
	s := bufio.NewScanner(strings.NewReader(out))
	for s.Scan() {
		lines = append(lines, s.Text())
	}
	return lines
}

// Guard against a silent harness regression: the nodes must serve a real
// receive-pack report.
func TestPushOutcome_HarnessServesRealReceivePack(t *testing.T) {
	t.Parallel()
	c := newPushCluster(t, 1)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, c.nodes[0].URL+pushRepoPath+"/info/refs?service=git-receive-pack", nil)
	require.NoError(t, err)
	resp, err := c.nodes[0].Client().Do(req)
	require.NoError(t, err)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
	assert.Contains(t, string(body), "report-status")
}
