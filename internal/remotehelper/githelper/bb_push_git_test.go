package githelper

import (
	"bufio"
	"bytes"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/jmwoliver/xet-go/xethash"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/execx"
	"github.com/entireio/cli/internal/remotehelper/debuglog"
	"github.com/entireio/cli/internal/remotehelper/gitproto"
	"github.com/entireio/cli/internal/remotehelper/replicas"
	"github.com/entireio/cli/internal/remotehelper/transport"
)

// fallbackLog is the end of the debug line of a push that fell back to
// send-pack.
const fallbackLog = "; pushing with send-pack"

// bbSide is one server of a bbPair.
type bbSide struct {
	fake   *fakeBB
	bare   string
	server *httptest.Server
	// log is the debug log of the last push.
	log string
}

// bbPair pushes one local repository to two servers: one that advertises
// entire-bb, so the helper builds the push, and one that does not, so
// send-pack builds it. Every push must give the same helper status lines
// and leave the same refs on both.
type bbPair struct {
	dir   string
	run   func(args ...string) string
	bb    *bbSide
	plain *bbSide
}

func newBBPair(t *testing.T) *bbPair {
	t.Helper()
	return newBBPairFormat(t, "sha1")
}

// newBBPairFormat is newBBPair with repositories of objectFormat.
func newBBPairFormat(t *testing.T, objectFormat string) *bbPair {
	t.Helper()
	dir, run := newRealGitRepo(t)
	if objectFormat != "sha1" {
		require.NoError(t, os.RemoveAll(filepath.Join(dir, ".git")))
		run("init", "-q", "--object-format="+objectFormat)
	}
	p := &bbPair{dir: dir, run: run}
	for _, advertise := range []bool{true, false} {
		backend, bare := gitBackend(t)
		if objectFormat != "sha1" {
			require.NoError(t, os.RemoveAll(bare))
			runGit(t, "init", "-q", "--bare", "-b", "main", "--object-format="+objectFormat, bare)
			runGit(t, "-C", bare, "config", "http.receivepack", "true")
		}
		runGit(t, "-C", bare, "config", "receive.advertisePushOptions", "true")
		fake := newFakeBB(t, backend, advertise)
		server := httptest.NewServer(fake)
		t.Cleanup(server.Close)
		side := &bbSide{fake: fake, bare: bare, server: server}
		if advertise {
			p.bb = side
		} else {
			p.plain = side
		}
	}
	run("commit", "-q", "--allow-empty", "-m", "root")
	p.seed("HEAD:refs/heads/main", "HEAD:refs/heads/other")
	t.Chdir(dir)
	return p
}

func (p *bbPair) sides() []*bbSide { return []*bbSide{p.bb, p.plain} }

// seed pushes to both bare repositories directly, without the helper.
func (p *bbPair) seed(specs ...string) {
	for _, s := range p.sides() {
		p.run(append([]string{"push", "-q", "-f", s.bare}, specs...)...)
	}
}

// commitBlob commits size random bytes from seed at path and returns the
// data.
func (p *bbPair) commitBlob(path string, size int, seed uint64) []byte {
	data := make([]byte, size)
	rng := rand.New(rand.NewChaCha8([32]byte{byte(seed), byte(seed >> 8)}))
	for i := range data {
		data[i] = byte(rng.Uint32())
	}
	p.commitData(path, data)
	return data
}

func (p *bbPair) commitData(path string, data []byte) {
	full := filepath.Join(p.dir, path)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		panic(err)
	}
	if err := os.WriteFile(full, data, 0o600); err != nil {
		panic(err)
	}
	p.run("add", path)
	p.run("commit", "-q", "-m", "add "+path)
}

// syncBuffer is a bytes.Buffer for writers on several goroutines.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

// handlePush runs the helper's push of specs through tr and keeps the
// push's debug log in s.log.
func (s *bbSide) handlePush(t *testing.T, tr Transport, opts *Options, specs []string) (string, error) {
	t.Helper()
	var rest strings.Builder
	for _, spec := range specs[1:] {
		rest.WriteString("push " + spec + "\n")
	}
	rest.WriteString("\n")
	t.Setenv("ENTIRE_DEBUG", "1")
	var log syncBuffer
	prev := debuglog.SetOutput(&log)
	defer debuglog.SetOutput(prev)
	var stdout bytes.Buffer
	err := handlePush(t.Context(), tr, &refAdvCache{}, "push "+specs[0], opts,
		bufio.NewReader(strings.NewReader(rest.String())), &stdout)
	s.log = log.String()
	return stdout.String(), err
}

func (s *bbSide) push(t *testing.T, opts *Options, specs ...string) (string, error) {
	t.Helper()
	return s.handlePush(t, testTransport(s.server), opts, specs)
}

func (s *bbSide) refs(t *testing.T) string {
	t.Helper()
	return runGit(t, "-C", s.bare, "for-each-ref", "--format=%(refname) %(objectname)")
}

// push pushes specs to both servers with the options that opts makes, and
// checks that both paths answer alike. It returns the status lines.
func (p *bbPair) push(t *testing.T, opts func() *Options, specs ...string) (string, error) {
	t.Helper()
	bbOut, bbErr := p.bb.push(t, opts(), specs...)
	plainOut, plainErr := p.plain.push(t, opts(), specs...)
	require.Equal(t, plainOut, bbOut, "helper status lines")
	require.Equal(t, plainErr == nil, bbErr == nil, "push errors: client-chunked %v, send-pack %v", bbErr, plainErr)
	require.Equal(t, p.plain.refs(t), p.bb.refs(t), "refs after the push")
	require.NotContains(t, p.plain.log, fallbackLog, "the plain server never offers the client-chunked push")
	return bbOut, bbErr
}

func noOptions() *Options { return &Options{} }

// requireChunked checks that the entire-bb server took n chunked entries in
// all.
func (p *bbPair) requireChunked(t *testing.T, n int) {
	t.Helper()
	_, _, _, chunked := p.bb.fake.counts()
	require.Equal(t, n, chunked, "chunked entries on the entire-bb server")
	_, _, _, plain := p.plain.fake.counts()
	require.Zero(t, plain)
}

// requireHelperPush checks that the helper built the last push to the
// entire-bb server.
func (p *bbPair) requireHelperPush(t *testing.T) {
	t.Helper()
	require.NotContains(t, p.bb.log, fallbackLog, "the helper built the push")
}

// requireSendPack checks that the last push to the entire-bb server fell
// back to send-pack for reason.
func (p *bbPair) requireSendPack(t *testing.T, reason string) {
	t.Helper()
	require.Contains(t, p.bb.log, fallbackLog, "send-pack built the push")
	require.Contains(t, p.bb.log, reason)
}

func writeHook(t *testing.T, bare, name, script string) {
	t.Helper()
	path := filepath.Join(bare, "hooks", name)
	require.NoError(t, os.WriteFile(path, []byte("#!/bin/sh\n"+script), 0o755))
}

func TestHandlePush_BB_SameResultBothPaths(t *testing.T) {
	// No t.Parallel(): t.Chdir, because the push runs git in the process cwd.

	t.Run("fast-forward with a large blob", func(t *testing.T) {
		p := newBBPair(t)
		p.commitBlob("big.bin", 300<<10, 1)
		out, err := p.push(t, noOptions, "refs/heads/main:refs/heads/main")
		require.NoError(t, err)
		require.Equal(t, "ok refs/heads/main\nerror refs/heads/other no match\n\n", out)
		p.requireChunked(t, 1)
		p.requireHelperPush(t)
	})

	t.Run("new branch with a large blob", func(t *testing.T) {
		p := newBBPair(t)
		p.commitBlob("dir/big.bin", 200<<10, 2)
		out, err := p.push(t, noOptions, "HEAD:refs/heads/feature")
		require.NoError(t, err)
		require.Contains(t, out, "ok refs/heads/feature\n")
		p.requireChunked(t, 1)
		p.requireHelperPush(t)
	})

	// git rejects a non-fast-forward before it runs the helper. When one
	// reaches the helper, the planner leaves it to send-pack.
	t.Run("rejected non-fast-forward", func(t *testing.T) {
		p := newBBPair(t)
		p.commitBlob("big.bin", 200<<10, 3)
		p.seed("HEAD:refs/heads/main")
		p.run("reset", "-q", "--hard", "HEAD~1")
		p.commitBlob("other.bin", 200<<10, 4)
		out, err := p.push(t, noOptions, "refs/heads/main:refs/heads/main")
		require.Error(t, err)
		require.Contains(t, out, "error refs/heads/main non-fast forward\n")
		p.requireChunked(t, 0)
		p.requireSendPack(t, "not a fast-forward")
	})

	// One refused ref makes the whole push fall back, so the large blob of
	// the other ref goes whole.
	t.Run("client-rejected ref and a new branch with a large blob", func(t *testing.T) {
		p := newBBPair(t)
		p.commitBlob("big.bin", 200<<10, 15)
		p.seed("HEAD:refs/heads/main")
		p.run("reset", "-q", "--hard", "HEAD~1")
		p.commitBlob("other.bin", 200<<10, 16)
		out, err := p.push(t, noOptions, "refs/heads/main:refs/heads/main", "HEAD:refs/heads/feature")
		require.Error(t, err)
		require.Contains(t, out, "error refs/heads/main non-fast forward\n")
		require.Contains(t, out, "ok refs/heads/feature\n")
		p.requireChunked(t, 0)
		p.requireSendPack(t, "not a fast-forward")
	})

	t.Run("force", func(t *testing.T) {
		p := newBBPair(t)
		p.commitBlob("big.bin", 200<<10, 5)
		p.seed("HEAD:refs/heads/main")
		p.run("reset", "-q", "--hard", "HEAD~1")
		p.commitBlob("other.bin", 200<<10, 6)
		out, err := p.push(t, noOptions, "+refs/heads/main:refs/heads/main")
		require.NoError(t, err)
		require.Contains(t, out, "ok refs/heads/main\n")
		p.requireChunked(t, 1)
		p.requireHelperPush(t)
	})

	t.Run("force-with-lease", func(t *testing.T) {
		p := newBBPair(t)
		p.commitBlob("big.bin", 200<<10, 7)
		p.seed("HEAD:refs/heads/main")
		served := p.run("rev-parse", "HEAD")
		p.run("reset", "-q", "--hard", "HEAD~1")
		p.commitBlob("other.bin", 200<<10, 8)
		rewritten := p.run("rev-parse", "HEAD")

		out, err := p.push(t, func() *Options { return &Options{cas: []string{testRefMain + ":" + rewritten}} }, "refs/heads/main:refs/heads/main")
		require.Error(t, err)
		require.Contains(t, out, "error refs/heads/main stale info\n")
		p.requireSendPack(t, "stale lease")

		out, err = p.push(t, func() *Options { return &Options{cas: []string{testRefMain + ":" + served}} }, "refs/heads/main:refs/heads/main")
		require.NoError(t, err)
		require.Contains(t, out, "ok refs/heads/main\n")
		p.requireChunked(t, 1)
		p.requireHelperPush(t)
	})

	t.Run("atomic", func(t *testing.T) {
		p := newBBPair(t)
		for _, s := range p.sides() {
			writeHook(t, s.bare, "update", "[ \"$1\" = refs/heads/blocked ] && exit 1\nexit 0\n")
		}
		p.commitBlob("big.bin", 200<<10, 9)
		atomic := func() *Options { return &Options{atomic: true} }

		out, err := p.push(t, atomic, "HEAD:refs/heads/main", "HEAD:refs/heads/blocked")
		require.Error(t, err)
		require.Contains(t, out, "error refs/heads/blocked")
		require.NotContains(t, out, "ok refs/heads/main")
		p.requireHelperPush(t)

		out, err = p.push(t, atomic, "HEAD:refs/heads/main", "HEAD:refs/heads/feature")
		require.NoError(t, err)
		require.Contains(t, out, "ok refs/heads/main\n")
		require.Contains(t, out, "ok refs/heads/feature\n")
		p.requireChunked(t, 2)
		p.requireHelperPush(t)
	})

	t.Run("refused ref without atomic", func(t *testing.T) {
		p := newBBPair(t)
		for _, s := range p.sides() {
			writeHook(t, s.bare, "update", "[ \"$1\" = refs/heads/blocked ] && exit 1\nexit 0\n")
		}
		p.commitBlob("big.bin", 200<<10, 10)
		out, err := p.push(t, noOptions, "HEAD:refs/heads/main", "HEAD:refs/heads/blocked")
		require.Error(t, err)
		require.Contains(t, out, "ok refs/heads/main\n")
		require.Contains(t, out, "error refs/heads/blocked hook declined\n")
		p.requireChunked(t, 1)
		p.requireHelperPush(t)
	})

	t.Run("push options", func(t *testing.T) {
		p := newBBPair(t)
		for _, s := range p.sides() {
			writeHook(t, s.bare, "pre-receive", "echo \"$GIT_PUSH_OPTION_COUNT ${GIT_PUSH_OPTION_0-} ${GIT_PUSH_OPTION_1-}\" > push-options.txt\n")
		}
		p.commitBlob("big.bin", 200<<10, 11)
		out, err := p.push(t, func() *Options { return &Options{pushOptions: []string{"ci.skip", "x=y z"}} }, "HEAD:refs/heads/main")
		require.NoError(t, err)
		require.Contains(t, out, "ok refs/heads/main\n")
		for _, s := range p.sides() {
			got, err := os.ReadFile(filepath.Join(s.bare, "push-options.txt"))
			require.NoError(t, err)
			require.Equal(t, "2 ci.skip x=y z\n", string(got))
		}
		p.requireChunked(t, 1)
		p.requireHelperPush(t)
	})

	t.Run("delete with an update", func(t *testing.T) {
		p := newBBPair(t)
		p.commitBlob("big.bin", 200<<10, 12)
		out, err := p.push(t, noOptions, ":refs/heads/other", "HEAD:refs/heads/main")
		require.NoError(t, err)
		require.Equal(t, "ok refs/heads/main\nok refs/heads/other\n\n", out)
		p.requireChunked(t, 1)
		p.requireHelperPush(t)
	})

	t.Run("delete only", func(t *testing.T) {
		p := newBBPair(t)
		out, err := p.push(t, noOptions, ":refs/heads/other")
		require.NoError(t, err)
		require.Contains(t, out, "ok refs/heads/other\n")
		p.requireChunked(t, 0)
		p.requireSendPack(t, "no large blobs in the push")
	})

	t.Run("dry run", func(t *testing.T) {
		p := newBBPair(t)
		p.commitBlob("big.bin", 200<<10, 13)
		out, err := p.push(t, func() *Options { return &Options{dryRun: true} }, "HEAD:refs/heads/main")
		require.NoError(t, err)
		require.Contains(t, out, "ok refs/heads/main\n")
		puts, _, receives, _ := p.bb.fake.counts()
		require.Zero(t, puts, "a dry run uploads nothing")
		require.Zero(t, receives, "a dry run posts nothing")
		p.requireHelperPush(t)
	})

	t.Run("small blobs only", func(t *testing.T) {
		p := newBBPair(t)
		p.commitBlob("small.bin", 1<<10, 14)
		out, err := p.push(t, noOptions, "HEAD:refs/heads/main")
		require.NoError(t, err)
		require.Contains(t, out, "ok refs/heads/main\n")
		p.requireChunked(t, 0)
		p.requireSendPack(t, "no large blobs in the push")
	})
}

// A SHA-256 repository writes 32-byte object IDs into the entries and a
// SHA-256 pack trailer.
func TestHandlePush_BB_SHA256(t *testing.T) {
	// No t.Parallel(): t.Chdir.
	p := newBBPairFormat(t, "sha256")
	p.commitBlob("big.bin", 300<<10, 70)
	out, err := p.push(t, noOptions, "HEAD:refs/heads/main")
	require.NoError(t, err)
	require.Contains(t, out, "ok refs/heads/main\n")
	p.requireChunked(t, 1)
	p.requireHelperPush(t)
	runGit(t, "-C", p.bb.bare, "fsck", "--strict")
}

// Two blobs that differ in a few bytes share most chunks. The second blob's
// recipe names the first one's xorb, so the upload is about one blob.
func TestHandlePush_BB_DedupsWithinPush(t *testing.T) {
	// No t.Parallel(): t.Chdir.
	p := newBBPair(t)
	const size = 1 << 20
	a := p.commitBlob("a.bin", size, 20)
	b := bytes.Clone(a)
	copy(b[size/2:], "a small edit in the middle of the blob")
	p.commitData("b.bin", b)

	out, err := p.push(t, noOptions, "HEAD:refs/heads/main")
	require.NoError(t, err)
	require.Contains(t, out, "ok refs/heads/main\n")
	p.requireChunked(t, 2)
	_, uploaded, _, _ := p.bb.fake.counts()
	require.Less(t, uploaded, int64(size+size/2), "the second blob reuses the first one's chunks")
}

// An upload that fails makes the helper push with send-pack, and the next
// push still works. bbclient's TestUploaderKeepsFinishedUploadsAfterAFailure
// shows without timing that a finished upload is kept; this is the
// end-to-end smoke test.
func TestHandlePush_BB_RepushAfterFailedUpload(t *testing.T) {
	// No t.Parallel(): t.Chdir.
	p := newBBPair(t)
	p.commitBlob("a.bin", 200<<10, 30)
	p.commitBlob("b.bin", 200<<10, 31)
	f := p.bb.fake

	var once sync.Once
	firstDone := make(chan struct{})
	f.mu.Lock()
	var first xethash.Hash
	f.failPut = func(h xethash.Hash) int {
		f.mu.Lock()
		if first == (xethash.Hash{}) {
			first = h
		}
		isFirst := first == h
		f.mu.Unlock()
		if isFirst {
			return 0
		}
		<-firstDone
		return http.StatusBadRequest
	}
	f.putDone = func(xethash.Hash) { once.Do(func() { close(firstDone) }) }
	f.failReceive = http.StatusInternalServerError
	f.mu.Unlock()
	_, err := p.bb.push(t, noOptions(), "HEAD:refs/heads/main")
	require.Error(t, err, "the send-pack fallback meets a failing receive-pack")
	require.Contains(t, p.bb.log, "upload refused by the test")

	f.mu.Lock()
	f.failPut, f.putDone, f.failReceive = nil, nil, 0
	f.puts = nil
	f.mu.Unlock()
	out, err := p.bb.push(t, noOptions(), "HEAD:refs/heads/main")
	require.NoError(t, err)
	require.Contains(t, out, "ok refs/heads/main\n")
	p.requireHelperPush(t)
	f.mu.Lock()
	defer f.mu.Unlock()
	require.Equal(t, 2, f.chunkedEntries)
}

// Every node answers each upload with HTTP 500. The answers do not remove
// the nodes from the replica set, so the send-pack fallback still reaches
// one.
func TestHandlePush_BB_UploadServerErrorsKeepReplicas(t *testing.T) {
	// No t.Parallel(): t.Chdir.
	p := newBBPair(t)
	p.commitBlob("big.bin", 200<<10, 60)
	f := p.bb.fake
	f.mu.Lock()
	f.failPut = func(xethash.Hash) int { return http.StatusInternalServerError }
	f.mu.Unlock()
	second := httptest.NewServer(f)
	defer second.Close()
	var mu sync.Mutex
	var failed []string
	tr := transport.New(transport.Config{
		Nodes: replicas.NodeConfig{
			InitialNodes: []string{p.bb.server.URL, second.URL},
			EntryURL:     p.bb.server.URL,
			ClusterHost:  hostOf(p.bb.server.URL),
		},
		Path:         "/et/owner/repo",
		OnNodeFailed: func(n string) { mu.Lock(); failed = append(failed, n); mu.Unlock() },
	})

	out, err := p.bb.handlePush(t, tr, noOptions(), []string{"HEAD:refs/heads/main"})
	require.NoError(t, err)
	require.Contains(t, out, "ok refs/heads/main\n")
	p.requireSendPack(t, "HTTP 500")
	mu.Lock()
	defer mu.Unlock()
	require.Empty(t, failed, "no node is marked failed")
	f.mu.Lock()
	defer f.mu.Unlock()
	require.Equal(t, 1, f.receives)
	require.Zero(t, f.bbReceives)
}

// A "missing xorb" refusal makes the helper query its xorbs, upload the
// missing one from the local store and post the same request once more.
func TestHandlePush_BB_MissingXorbRetry(t *testing.T) {
	// No t.Parallel(): t.Chdir.
	p := newBBPair(t)
	p.commitBlob("big.bin", 300<<10, 40)
	f := p.bb.fake
	f.mu.Lock()
	f.dropPushes = 1
	f.mu.Unlock()

	out, err := p.bb.push(t, noOptions(), "HEAD:refs/heads/main")
	require.NoError(t, err)
	require.Equal(t, "ok refs/heads/main\nerror refs/heads/other no match\n\n", out)
	puts, _, receives, chunked := f.counts()
	require.Equal(t, 2, receives, "one refusal, one retry")
	require.Equal(t, 2, puts, "the dropped xorb uploads again")
	require.Equal(t, 1, chunked)
	require.Equal(t, p.run("rev-parse", "HEAD"), runGit(t, "-C", p.bb.bare, "rev-parse", testRefMain))
}

// The helper retries a "missing xorb" refusal once. A second refusal is the
// answer: the status lines report it, and the ref stays.
func TestHandlePush_BB_RefusedAgainAfterRetry(t *testing.T) {
	// No t.Parallel(): t.Chdir.
	p := newBBPair(t)
	p.commitBlob("big.bin", 300<<10, 41)
	f := p.bb.fake
	f.mu.Lock()
	f.dropPushes = 2
	f.mu.Unlock()
	before := runGit(t, "-C", p.bb.bare, "rev-parse", testRefMain)

	out, err := p.bb.push(t, noOptions(), "HEAD:refs/heads/main")
	require.Error(t, err)
	require.Equal(t, "error refs/heads/main unpacker error\nerror refs/heads/other no match\n\n", out)
	_, _, receives, chunked := f.counts()
	require.Equal(t, 2, receives, "one retry only")
	require.Zero(t, chunked)
	require.Equal(t, before, runGit(t, "-C", p.bb.bare, "rev-parse", testRefMain))
}

// splitMix64Bytes is create_random_data from xet-core's chunking tests. The
// seeds below give 4 MiB inputs with known anchor chunks.
func splitMix64Bytes(n int, seed uint64) []byte {
	out := make([]byte, 0, n+7)
	state := seed
	for len(out) < n {
		state += 0x9E3779B97F4A7C15
		z := state
		z = (z ^ (z >> 30)) * 0xBF58476D1CE4E5B9
		z = (z ^ (z >> 27)) * 0x94D049BB133111EB
		z ^= z >> 31
		for range 8 {
			out = append(out, byte(z))
			z >>= 8
		}
	}
	return out[:n]
}

// Without a local store, the helper finds the repo's xorbs through anchor
// chunks. The server answers the anchors of one xorb per query here, so the
// helper asks again for the anchors that got no answer.
func TestHandlePush_BB_DedupsAgainstServerAnchors(t *testing.T) {
	// No t.Parallel(): t.Chdir.
	p := newBBPair(t)
	const size = 4 << 20
	a, b := splitMix64Bytes(size, 42), splitMix64Bytes(size, 45)
	p.commitData("a.bin", a)
	p.commitData("b.bin", b)
	_, err := p.push(t, noOptions, "HEAD:refs/heads/main")
	require.NoError(t, err)
	p.requireChunked(t, 2)

	require.NoError(t, os.RemoveAll(filepath.Join(p.dir, ".git", "entire-xorbs")))
	f := p.bb.fake
	f.mu.Lock()
	f.footerCap = 1
	f.dedups, f.putBytes = 0, 0
	f.mu.Unlock()
	copy(a[5<<19:], "an edit after the anchors")
	copy(b[3<<20:], "another edit after the anchors")
	p.commitData("a.bin", a)
	p.commitData("b.bin", b)
	_, err = p.push(t, noOptions, "HEAD:refs/heads/main")
	require.NoError(t, err)
	p.requireChunked(t, 4)

	f.mu.Lock()
	defer f.mu.Unlock()
	require.Equal(t, 2, f.dedups, "a second query for the anchors that the first one dropped")
	require.Less(t, f.putBytes, int64(1<<20), "only the edited chunks upload")
}

// ENTIRE_HELPER_CHUNKING=0 turns the client path off. The helper then
// does not ask for entire-bb, so the server does not advertise it, and
// send-pack sends the large blob whole.
func TestHandlePush_BB_KillSwitch(t *testing.T) {
	// No t.Parallel(): t.Chdir and t.Setenv.
	p := newBBPair(t)
	p.commitBlob("big.bin", 200<<10, 80)
	t.Setenv(helperChunkingEnv, "0")

	out, err := p.bb.push(t, noOptions(), "HEAD:refs/heads/main")
	require.NoError(t, err)
	require.Contains(t, out, "ok refs/heads/main\n")
	f := p.bb.fake
	f.mu.Lock()
	defer f.mu.Unlock()
	require.Zero(t, f.bbAdvertised, "no X-Entire-BB, no entire-bb")
	require.Zero(t, f.bbReceives)
	require.Equal(t, 1, f.receives)
	require.Empty(t, f.puts)
}

// ENTIRE_BB_CLIENT_CHUNKING names entiredb's own switch. A client that
// read it would follow a server setting that leaked into its environment.
func TestHelperChunkingOff(t *testing.T) {
	// No t.Parallel(): t.Setenv.
	t.Setenv("ENTIRE_BB_CLIENT_CHUNKING", "0")
	for value, off := range map[string]bool{"0": true, "false": true, "FALSE": true, "1": false, "true": false, "junk": false} {
		t.Setenv(helperChunkingEnv, value)
		require.Equal(t, off, helperChunkingOff(), "%s=%q", helperChunkingEnv, value)
	}
	require.NoError(t, os.Unsetenv(helperChunkingEnv))
	require.False(t, helperChunkingOff(), "only %s turns chunking off", helperChunkingEnv)
}

// The server advertises entire-bb only on a request with X-Entire-BB. A
// plain receive-pack advertisement leads to send-pack.
func TestHandlePush_BB_AdvertisementNeedsHeader(t *testing.T) {
	// No t.Parallel(): t.Chdir.
	p := newBBPair(t)
	p.commitBlob("big.bin", 200<<10, 81)
	tr := testTransport(p.bb.server)
	rc, err := tr.InfoRefs(t.Context(), serviceReceivePack)
	require.NoError(t, err)
	t.Cleanup(func() { _ = rc.Close() })
	var stdout, stderr bytes.Buffer
	plain, err := gitproto.ReadPostServiceAdvertisement(rc)
	require.NoError(t, err)
	pushed, err := helperPush(t.Context(), tr, plain, []string{"HEAD:refs/heads/main"}, noOptions(), &stdout, &stderr)
	require.NoError(t, err)
	require.False(t, pushed, "no entire-bb, no helper-built push")
	require.Empty(t, stderr.String())

	adv, err := pushAdvertisement(t.Context(), tr, &refAdvCache{})
	require.NoError(t, err)
	pushed, err = helperPush(t.Context(), tr, adv, []string{"HEAD:refs/heads/main"}, noOptions(), &stdout, &stderr)
	require.NoError(t, err)
	require.True(t, pushed)
	require.Contains(t, stdout.String(), "ok refs/heads/main\n")
	_, _, _, chunked := p.bb.fake.counts()
	require.Equal(t, 1, chunked)
}

// An unexpected fallback prints one warning, because the large blobs then
// go whole. A push without large blobs prints nothing.
func TestHelperPush_WarnsOnUnexpectedFallback(t *testing.T) {
	// No t.Parallel(): t.Chdir.
	p := newBBPair(t)
	tr := testTransport(p.bb.server)
	adv, err := pushAdvertisement(t.Context(), tr, &refAdvCache{})
	require.NoError(t, err)

	p.commitBlob("small.bin", 1<<10, 91)
	var stdout, stderr bytes.Buffer
	pushed, err := helperPush(t.Context(), tr, adv, []string{"HEAD:refs/heads/main"}, noOptions(), &stdout, &stderr)
	require.NoError(t, err)
	require.False(t, pushed)
	require.Empty(t, stderr.String(), "no large blobs is not a surprise")

	p.commitBlob("big.bin", 200<<10, 90)
	pushed, err = helperPush(t.Context(), tr, adv, []string{"nosuchref:refs/heads/main"}, noOptions(), &stdout, &stderr)
	require.NoError(t, err)
	require.False(t, pushed)
	require.Empty(t, stdout.String())
	lines := strings.Split(strings.TrimSuffix(stderr.String(), "\n"), "\n")
	require.Len(t, lines, 1, "one warning line: %q", stderr.String())
	require.True(t, strings.HasPrefix(lines[0], "warning: client-chunked push not used: "), lines[0])
	require.True(t, strings.HasSuffix(lines[0], "; sending large blobs whole"), lines[0])
}

// A client-chunked push prints one summary line on stderr: the large
// blobs, the xorbs that it uploaded or reused, and the share of chunk
// bytes that it did not upload. git push -q (verbosity 0) stops it.
func TestHelperPush_BB_Summary(t *testing.T) {
	// No t.Parallel(): t.Chdir.
	p := newBBPair(t)
	tr := testTransport(p.bb.server)
	push := func(opts *Options) string {
		t.Helper()
		adv, err := pushAdvertisement(t.Context(), tr, &refAdvCache{})
		require.NoError(t, err)
		var stdout, stderr bytes.Buffer
		pushed, err := helperPush(t.Context(), tr, adv, []string{"HEAD:refs/heads/main"}, opts, &stdout, &stderr)
		require.NoError(t, err)
		require.True(t, pushed)
		require.Contains(t, stdout.String(), "ok refs/heads/main\n")
		return stderr.String()
	}
	quiet := func() *Options {
		o := &Options{}
		o.Set("verbosity", "0")
		return o
	}

	a := splitMix64Bytes(3<<19, 101)
	p.commitData("a.bin", a)
	require.Equal(t, "Chunked 1 large blob (1.50 MiB): 1 of 1 xorb uploaded (1.50 MiB); 0% of chunk bytes deduplicated\n", push(noOptions()))

	p.commitData("b.bin", append(bytes.Clone(a), "a tail"...))
	require.Empty(t, push(quiet()), "git push -q prints no summary")

	p.commitData("c.bin", append(bytes.Clone(a), "another tail"...))
	require.Equal(t, "Chunked 1 large blob (1.50 MiB): 1 of 2 xorbs uploaded (9.43 KiB), 1 reused from the server; 99% of chunk bytes deduplicated\n", push(noOptions()))
}

// Real git drives the helper through the remote-helper protocol: options,
// the push batch and the status lines.
func TestGitPush_BB(t *testing.T) {
	// No t.Parallel(): newRealGitRepo sets process env.
	if runtime.GOOS == goosWindows {
		t.Skip("shell-script helper stub is POSIX-only")
	}
	p := newBBPair(t)
	const scheme = "entirebbtest"
	path := helperStub(t, scheme, p.bb.server.URL, 2)
	p.run("remote", "add", "origin", scheme+"://host/et/owner/repo")
	gitPush := func(t *testing.T, args ...string) {
		t.Helper()
		cmd := execx.NonInteractive(t.Context(), "git", append([]string{"-c", "protocol.version=2", "push", "origin"}, args...)...)
		cmd.Dir = p.dir
		cmd.Env = append(os.Environ(), "PATH="+path)
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "%s", out)
		require.NotContains(t, string(out), "unexpected status", "%s", out)
		require.NotContains(t, string(out), "warning:", "%s", out)
	}

	p.commitBlob("big.bin", 300<<10, 50)
	gitPush(t, "--atomic", "--push-option=ci.skip", "HEAD:refs/heads/main", "HEAD:refs/heads/feature")
	head := p.run("rev-parse", "HEAD")
	require.Equal(t, head, runGit(t, "-C", p.bb.bare, "rev-parse", "refs/heads/main"))
	require.Equal(t, head, runGit(t, "-C", p.bb.bare, "rev-parse", "refs/heads/feature"))
	_, _, _, chunked := p.bb.fake.counts()
	require.Equal(t, 1, chunked)

	p.run("reset", "-q", "--hard", "HEAD~1")
	p.commitBlob("other.bin", 300<<10, 51)
	gitPush(t, "--force-with-lease", "--force-if-includes", "HEAD:refs/heads/main")
	require.Equal(t, p.run("rev-parse", "HEAD"), runGit(t, "-C", p.bb.bare, "rev-parse", "refs/heads/main"))
	_, _, _, chunked = p.bb.fake.counts()
	require.Equal(t, 2, chunked)
	runGit(t, "-C", p.bb.bare, "fsck", "--strict")
}
