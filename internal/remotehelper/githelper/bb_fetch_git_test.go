package githelper

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/jmwoliver/xet-go/xethash"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/execx"
	"github.com/entireio/cli/internal/remotehelper/bbcache"
)

// fetchEnv is one fake entiredb with client-chunked push and fetch, and a
// git-remote-<scheme> stub that runs this test binary as the helper. Real
// git clones, fetches and pushes through it.
type fetchEnv struct {
	fake         *fakeBB
	bare         string
	server       *httptest.Server
	path         string
	url          string
	objectFormat string
}

const fetchScheme = "entirefetchtest"

func newFetchEnv(t *testing.T, objectFormat string, sidebandAll bool) *fetchEnv {
	t.Helper()
	if runtime.GOOS == goosWindows {
		t.Skip("shell-script helper stub is POSIX-only")
	}
	backend, bare := gitBackend(t)
	if objectFormat != "sha1" {
		require.NoError(t, os.RemoveAll(bare))
		runGit(t, "init", "-q", "--bare", "-b", "main", "--object-format="+objectFormat, bare)
		runGit(t, "-C", bare, "config", "http.receivepack", "true")
	}
	runGit(t, "-C", bare, "config", "uploadpack.allowSidebandAll", map[bool]string{true: "true", false: "false"}[sidebandAll])
	// The fallback record is in the per-user cache. The helper processes
	// inherit this environment, so they share the test's record.
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	fake := newFakeBB(t, backend, true)
	fake.setFetch(fetchAdvertise, fetchAdvertise)
	server := httptest.NewServer(fake)
	t.Cleanup(server.Close)
	return &fetchEnv{
		fake:         fake,
		bare:         bare,
		server:       server,
		path:         helperStub(t, fetchScheme, server.URL, 2),
		url:          fetchScheme + "://host/et/owner/repo",
		objectFormat: objectFormat,
	}
}

// git runs git in dir with the helper stub on PATH and extra environment.
func (e *fetchEnv) git(t *testing.T, dir string, env []string, args ...string) (string, error) {
	t.Helper()
	base := []string{"-c", "protocol.version=2", "-c", "transfer.fsckObjects=true",
		"-c", "user.name=test", "-c", "user.email=test@example.com", "-c", "commit.gpgsign=false"}
	cmd := execx.NonInteractive(t.Context(), "git", append(base, args...)...)
	cmd.Dir = dir
	cmd.Env = append(append(os.Environ(), "PATH="+e.path), env...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func (e *fetchEnv) mustGit(t *testing.T, dir string, env []string, args ...string) string {
	t.Helper()
	out, err := e.git(t, dir, env, args...)
	require.NoError(t, err, "git %v:\n%s", args, out)
	require.NotContains(t, out, "warning:", "git %v", args)
	return out
}

// newRepo makes an empty repository with origin at the fake.
func (e *fetchEnv) newRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	runGit(t, "init", "-q", "-b", "main", "--object-format="+e.objectFormat, dir)
	runGit(t, "-C", dir, "remote", "add", "origin", e.url)
	return dir
}

// clone clones the fake into a new directory.
func (e *fetchEnv) clone(t *testing.T, env ...string) string {
	t.Helper()
	dir, _ := e.cloneOut(t, "-q", env...)
	return dir
}

// cloneOut clones the fake with the clone option opt and returns the
// directory and git's output.
func (e *fetchEnv) cloneOut(t *testing.T, opt string, env ...string) (string, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "clone")
	out := e.mustGit(t, filepath.Dir(dir), env, "clone", opt, e.url, dir)
	return dir, out
}

// writeAndCommit writes data at name in dir and commits it.
func (e *fetchEnv) writeAndCommit(t *testing.T, dir, name string, data []byte) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(dir, name), data, 0o600))
	e.mustGit(t, dir, nil, "add", name)
	e.mustGit(t, dir, nil, "commit", "-q", "-m", "write "+name)
}

// store opens the helper's cache of the fake's repo in the repository dir.
func (e *fetchEnv) store(t *testing.T, dir string) *bbcache.Store {
	t.Helper()
	s, err := bbcache.Open(filepath.Join(dir, ".git"), e.server.URL, "/et/owner/repo")
	require.NoError(t, err)
	return s
}

// requireFallback checks the fetch fallback record of the fake's repo.
func (e *fetchEnv) requireFallback(t *testing.T, want bool) {
	t.Helper()
	r, err := bbcache.OpenFallbackRecord(e.server.URL, "/et/owner/repo")
	require.NoError(t, err)
	_, ok := r.Get()
	require.Equal(t, want, ok, "fetch fallback record")
}

// objectsDigest returns the number of objects in dir and a digest of their
// IDs, types and bytes.
func objectsDigest(t *testing.T, dir string) (int, string) {
	t.Helper()
	ids := runGit(t, "-C", dir, "cat-file", "--batch-all-objects", "--batch-check")
	out, err := execx.NonInteractive(t.Context(), "git", "-C", dir, "cat-file", "--batch-all-objects", "--batch").Output()
	require.NoError(t, err)
	sum := sha256.Sum256(out)
	return len(strings.Split(ids, "\n")), hex.EncodeToString(sum[:])
}

// requireSameObjects checks that two repositories hold the same objects,
// byte for byte, and that got passes fsck.
func requireSameObjects(t *testing.T, want, got string) {
	t.Helper()
	wn, wd := objectsDigest(t, want)
	gn, gd := objectsDigest(t, got)
	require.Equal(t, wn, gn, "object count")
	require.Equal(t, wd, gd, "object bytes")
	runGit(t, "-C", got, "fsck", "--strict", "--no-dangling")
}

// seed makes the writer repository, commits two large blobs and a small
// one, and pushes them with the client-chunked push.
func (e *fetchEnv) seed(t *testing.T) (dir string, big []byte) {
	t.Helper()
	dir = e.newRepo(t)
	big = splitMix64Bytes(1<<20, 7)
	e.writeAndCommit(t, dir, "big.bin", big)
	e.writeAndCommit(t, dir, "other.bin", splitMix64Bytes(300<<10, 8))
	e.writeAndCommit(t, dir, "small.txt", []byte("small\n"))
	e.mustGit(t, dir, nil, "push", "-q", "origin", "main")
	_, _, _, chunked := e.fake.counts()
	require.Equal(t, 2, chunked, "the push sent both large blobs as recipes")
	return dir, big
}

func (e *fetchEnv) gets(t *testing.T) []xethash.Hash {
	t.Helper()
	e.fake.mu.Lock()
	defer e.fake.mu.Unlock()
	return slices.Clone(e.fake.fetch.gets)
}

// TestGitFetch_BB drives the client-chunked fetch with real git: a clone
// that downloads every xorb, a push from it that deduplicates against the
// downloaded xorbs, an incremental fetch that downloads only the new xorbs
// and reuses the xorbs that the fetching clone pushed, and a fetch with
// every xorb cached. Each result must equal a clone without the opt-in.
func TestGitFetch_BB(t *testing.T) {
	// No t.Parallel(): gitBackend isolates the process git config.
	for _, tc := range []struct {
		name         string
		objectFormat string
		sidebandAll  bool
	}{
		{"sha1", "sha1", false},
		{"sha1 sideband-all", "sha1", true},
		{"sha256", "sha256", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			testGitFetchBB(t, tc.objectFormat, tc.sidebandAll)
		})
	}
}

func testGitFetchBB(t *testing.T, objectFormat string, sidebandAll bool) {
	e := newFetchEnv(t, objectFormat, sidebandAll)
	writer, big := e.seed(t)

	reader, out := e.cloneOut(t, "--progress")
	require.Contains(t, out, "Downloading large files: 2/2 xorbs, 1.3 MiB, done.")
	require.Contains(t, out, "Rebuilding large files: 2/2, done.")
	plain := e.clone(t, helperChunkingEnv+"=0")
	requireSameObjects(t, plain, reader)
	c := e.fake.fetchCounts()
	require.Equal(t, 2, c.sentRecipes, "the clone got two recipes")
	require.Equal(t, 2, c.gets, "one xorb for each large blob")
	t.Logf("clone: downloaded %d bytes for %d logical bytes", c.getBytes, len(big)+300<<10)
	e.requireFallback(t, false)
	e.fake.mu.Lock()
	for _, p := range e.fake.fetch.posts {
		require.Equal(t, sidebandAll, p.HasArgument("sideband-all"), "sideband-all")
	}
	e.fake.mu.Unlock()

	// The reader edits the large blob and pushes. Its cache holds the
	// downloaded xorbs as permanent, so the push asks for none of their
	// anchors and uploads only the edited chunks.
	downloaded := e.gets(t)
	_, putBytesBefore, _, _ := e.fake.counts()
	edited := slices.Clone(big)
	copy(edited[512<<10:], "an edit in the middle of the large blob")
	e.writeAndCommit(t, reader, "big.bin", edited)
	e.mustGit(t, reader, nil, "push", "-q", "origin", "main")
	_, putBytes, _, chunked := e.fake.counts()
	require.Equal(t, 3, chunked)
	require.Less(t, putBytes-putBytesBefore, int64(300<<10), "only the edited chunks upload")
	e.fake.mu.Lock()
	lastAnchors := e.fake.dedupAnchors[len(e.fake.dedupAnchors)-1]
	lastXorbs := e.fake.dedupXorbs[len(e.fake.dedupXorbs)-1]
	e.fake.mu.Unlock()
	held := map[xethash.Hash]bool{}
	for _, h := range downloaded {
		x, ok := e.store(t, reader).Record(h)
		require.True(t, ok, "downloaded xorb %s has a record", h)
		for _, ch := range x.Chunks {
			held[ch.Hash] = true
		}
	}
	require.True(t, slices.ContainsFunc(lastXorbs, func(h xethash.Hash) bool { return slices.Contains(downloaded, h) }),
		"the push names a downloaded xorb from its local record")
	for _, a := range lastAnchors {
		require.False(t, held[a], "the push asked for anchor %s of a downloaded xorb", a)
	}

	// The writer fetches the edit. The recipe names the writer's own pushed
	// xorb, which it converts, and the reader's new xorb, which it downloads.
	getsBefore := e.fake.fetchCounts()
	e.mustGit(t, writer, nil, "fetch", "-q", "origin")
	after := e.fake.fetchCounts()
	require.Equal(t, getsBefore.sentRecipes+1, after.sentRecipes)
	newGets := after.gets - getsBefore.gets
	require.Equal(t, 1, newGets, "only the reader's new xorb downloads")
	t.Logf("incremental fetch: downloaded %d bytes for %d logical bytes", after.getBytes-getsBefore.getBytes, len(edited))
	require.Equal(t, runGit(t, "-C", reader, "rev-parse", "HEAD:big.bin"), runGit(t, "-C", writer, "rev-parse", "origin/main:big.bin"))
	runGit(t, "-C", writer, "fsck", "--strict", "--no-dangling")

	// A new repository with the reader's cache fetches without a download.
	cached := e.newRepo(t)
	require.NoError(t, os.CopyFS(filepath.Join(cached, ".git", "entire-xorbs"), os.DirFS(filepath.Join(reader, ".git", "entire-xorbs"))))
	before := e.fake.fetchCounts()
	out = e.mustGit(t, cached, nil, "fetch", "-q", "origin")
	require.NotContains(t, out, "large files", "no progress under -q")
	after = e.fake.fetchCounts()
	require.Equal(t, before.gets, after.gets, "every xorb is in the cache")
	require.Greater(t, after.sentRecipes, before.sentRecipes)
	plainAgain := e.clone(t, helperChunkingEnv+"=0")
	requireSameObjects(t, plainAgain, cached)
}

// A cached xorb whose chunk fails its check is removed and downloaded once
// more. The fetch succeeds.
func TestGitFetch_BB_CorruptCachedXorb(t *testing.T) {
	// No t.Parallel(): gitBackend isolates the process git config.
	e := newFetchEnv(t, "sha1", false)
	e.seed(t)
	reader := e.clone(t)

	again := e.newRepo(t)
	cache := filepath.Join(again, ".git", "entire-xorbs")
	require.NoError(t, os.CopyFS(cache, os.DirFS(filepath.Join(reader, ".git", "entire-xorbs"))))
	files, err := filepath.Glob(filepath.Join(cache, "*", "complete", "*"))
	require.NoError(t, err)
	require.Len(t, files, 2)
	b, err := os.ReadFile(files[0])
	require.NoError(t, err)
	b[100] ^= 0xff
	require.NoError(t, os.WriteFile(files[0], b, 0o600))

	before := e.fake.fetchCounts()
	e.mustGit(t, again, nil, "fetch", "-q", "origin")
	after := e.fake.fetchCounts()
	require.Equal(t, before.gets+1, after.gets, "the corrupt xorb downloads again")
	require.Equal(t, filepath.Base(files[0]), e.gets(t)[after.gets-1].String())
	e.requireFallback(t, false)
	requireSameObjects(t, reader, again)
}

// A download that fails after the pack started fails the fetch with a clear
// message and records a fallback. The next fetch gets whole blobs.
func TestGitFetch_BB_DownloadFailure(t *testing.T) {
	// No t.Parallel(): gitBackend isolates the process git config.
	e := newFetchEnv(t, "sha1", false)
	e.seed(t)
	e.fake.mu.Lock()
	e.fake.fetch.failGet = func(xethash.Hash) int { return http.StatusNotFound }
	e.fake.mu.Unlock()

	reader := e.newRepo(t)
	out, err := e.git(t, reader, nil, "fetch", "origin")
	require.Error(t, err, "%s", out)
	require.Contains(t, out, "entire: large file download failed:")
	require.Contains(t, out, "Clones and fetches of this repository get whole files for the next 24 hours")
	require.Contains(t, out, helperChunkingEnv+"=0")
	require.Equal(t, 1, strings.Count(out, "HTTP 404"), "the helper's own error does not repeat the report:\n%s", out)
	e.requireFallback(t, true)
	require.Empty(t, runGit(t, "-C", reader, "for-each-ref"), "no ref moved")

	before := e.fake.fetchCounts()
	e.mustGit(t, reader, nil, "fetch", "-q", "origin")
	after := e.fake.fetchCounts()
	require.Equal(t, before.withArg, after.withArg, "the next fetch does not opt in")
	require.Equal(t, before.sentRecipes, after.sentRecipes)
	requireSameObjects(t, e.clone(t, helperChunkingEnv+"=0"), reader)
}

// The fallback record of a failed clone outlives the clone, which git
// removes. The next clone gets whole blobs.
func TestGitFetch_BB_FailedCloneFallsBack(t *testing.T) {
	// No t.Parallel(): gitBackend isolates the process git config.
	e := newFetchEnv(t, "sha1", false)
	e.seed(t)
	e.fake.mu.Lock()
	e.fake.fetch.failGet = func(xethash.Hash) int { return http.StatusNotFound }
	e.fake.mu.Unlock()

	dir := filepath.Join(t.TempDir(), "clone")
	out, err := e.git(t, filepath.Dir(dir), nil, "clone", e.url, dir)
	require.Error(t, err, "%s", out)
	require.Contains(t, out, "entire: large file download failed:")
	require.NoDirExists(t, dir, "git removes the failed clone")
	e.requireFallback(t, true)

	before := e.fake.fetchCounts()
	reader := e.clone(t)
	after := e.fake.fetchCounts()
	require.Equal(t, before.withArg, after.withArg, "the next clone does not opt in")
	requireSameObjects(t, e.clone(t, helperChunkingEnv+"=0"), reader)
}

// A response that breaks off inside the pack is a failure of the stream, not
// of the recipes. The fetch fails, records no fallback, and the next fetch
// opts in again.
func TestGitFetch_BB_BrokenStream(t *testing.T) {
	// No t.Parallel(): gitBackend isolates the process git config.
	e := newFetchEnv(t, "sha1", false)
	e.seed(t)
	e.fake.mu.Lock()
	e.fake.fetch.truncate = true
	e.fake.mu.Unlock()

	reader := e.newRepo(t)
	out, err := e.git(t, reader, nil, "fetch", "origin")
	require.Error(t, err, "%s", out)
	require.NotContains(t, out, "large file download failed")
	e.requireFallback(t, false)

	e.fake.mu.Lock()
	e.fake.fetch.truncate = false
	e.fake.mu.Unlock()
	before := e.fake.fetchCounts()
	e.mustGit(t, reader, nil, "fetch", "-q", "origin")
	after := e.fake.fetchCounts()
	require.Equal(t, before.withArg+1, after.withArg, "the next fetch opts in")
	require.Greater(t, after.sentRecipes, before.sentRecipes)
	requireSameObjects(t, e.clone(t, helperChunkingEnv+"=0"), reader)
}

// A cache that the helper cannot write keeps the fetch from opting in. The
// fetch gets whole blobs.
func TestGitFetch_BB_ReadOnlyCache(t *testing.T) {
	// No t.Parallel(): gitBackend isolates the process git config.
	if os.Getuid() == 0 {
		t.Skip("root writes to a read-only directory")
	}
	e := newFetchEnv(t, "sha1", false)
	e.seed(t)
	reader := e.newRepo(t)
	cache := filepath.Join(reader, ".git", "entire-xorbs")
	require.NoError(t, os.Mkdir(cache, 0o500))
	t.Cleanup(func() { require.NoError(t, os.Chmod(cache, 0o700)) })

	before := e.fake.fetchCounts()
	e.mustGit(t, reader, nil, "fetch", "-q", "origin")
	after := e.fake.fetchCounts()
	require.Equal(t, before.withArg, after.withArg, "no argument")
	require.Equal(t, before.sentRecipes, after.sentRecipes)
	requireSameObjects(t, e.clone(t, helperChunkingEnv+"=0"), reader)
	e.requireFallback(t, false)
}

// A node that refuses the argument in-band (switch off) refuses before any
// byte reaches git. The helper sends the request again without the
// argument and records a fallback. A node that answers HTTP 500 (a node
// from before the fetch spec) gets the same resend, but only the session
// stops opting in: the next fetch asks again.
func TestGitFetch_BB_ArgumentRefused(t *testing.T) {
	// No t.Parallel(): gitBackend isolates the process git config.
	for _, postSwitch := range []string{fetchOff, fetchOld} {
		t.Run(postSwitch, func(t *testing.T) {
			e := newFetchEnv(t, "sha1", false)
			writer, _ := e.seed(t)
			e.fake.setFetch(fetchAdvertise, postSwitch)

			reader := e.clone(t)
			c := e.fake.fetchCounts()
			require.Equal(t, 1, c.withArg, "one try with the argument")
			require.Equal(t, 1, c.refused)
			require.Zero(t, c.sentRecipes)
			require.Zero(t, c.gets)
			e.requireFallback(t, postSwitch == fetchOff)
			requireSameObjects(t, e.clone(t, helperChunkingEnv+"=0"), reader)

			// A fallback record keeps the next fetch from opting in, even
			// when the node accepts the argument again.
			e.fake.setFetch(fetchAdvertise, fetchAdvertise)
			e.writeAndCommit(t, writer, "new.bin", splitMix64Bytes(200<<10, 9))
			e.mustGit(t, writer, nil, "push", "-q", "origin", "main")
			e.mustGit(t, reader, nil, "fetch", "-q", "origin")
			c = e.fake.fetchCounts()
			if postSwitch == fetchOff {
				require.Equal(t, 1, c.withArg)
				require.Zero(t, c.sentRecipes)
			} else {
				require.Equal(t, 2, c.withArg, "a 500 records no fallback")
				require.Equal(t, 1, c.sentRecipes)
			}
			require.Equal(t, runGit(t, "-C", writer, "rev-parse", "HEAD"), runGit(t, "-C", reader, "rev-parse", "origin/main"))
		})
	}
}

// The kill switch sends no X-Entire-BB and no argument. A server that does
// not advertise the feature gets no argument either.
func TestGitFetch_BB_NoOptIn(t *testing.T) {
	// No t.Parallel(): gitBackend isolates the process git config.
	t.Run("kill switch", func(t *testing.T) {
		e := newFetchEnv(t, "sha1", false)
		e.seed(t)
		before := e.fake.fetchCounts()
		e.clone(t, helperChunkingEnv+"=false")
		after := e.fake.fetchCounts()
		require.Equal(t, before.bbHeaders, after.bbHeaders, "no X-Entire-BB")
		require.Zero(t, after.withArg)
		require.Zero(t, after.gets)
		e.requireFallback(t, false)
	})
	t.Run("no advertisement", func(t *testing.T) {
		e := newFetchEnv(t, "sha1", false)
		e.seed(t)
		e.fake.setFetch(fetchAccept, fetchAccept)
		before := e.fake.fetchCounts()
		reader := e.clone(t)
		after := e.fake.fetchCounts()
		require.Greater(t, after.bbHeaders, before.bbHeaders, "the helper asks")
		require.Zero(t, after.advertised)
		require.Zero(t, after.withArg)
		require.Zero(t, after.gets)
		requireSameObjects(t, e.clone(t, helperChunkingEnv+"=0"), reader)
	})
}
