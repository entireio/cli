package bbcache

import (
	"errors"
	"math/rand/v2"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/jmwoliver/xet-go/xethash"
	"github.com/jmwoliver/xet-go/xorb"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/gitdir"
	"github.com/entireio/cli/internal/remotehelper/bbchunk"
	"github.com/entireio/cli/internal/remotehelper/bbclient"
)

const (
	server = "https://cluster.example"
	repo   = "/et/alice/repo"
)

// fakeClock is a settable clock for a Store.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func openStore(t *testing.T, commonDir string, opts ...option) (*Store, *fakeClock) {
	t.Helper()
	s, err := Open(commonDir, server, repo, opts...)
	require.NoError(t, err)
	clock := &fakeClock{t: time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)}
	s.now = clock.now
	return s, clock
}

func testXorb(t *testing.T, seed uint64) *bbchunk.Xorb {
	t.Helper()
	rng := rand.New(rand.NewPCG(seed, seed))
	var b xorb.Builder
	for range 3 {
		data := make([]byte, 3000)
		for i := range data {
			data[i] = byte(rng.Uint32())
		}
		_, err := b.Add(data, xorb.Auto)
		require.NoError(t, err)
	}
	x, err := bbchunk.Seal(&b)
	require.NoError(t, err)
	return x
}

func footer(x *bbchunk.Xorb) []bbclient.FooterChunk {
	out := make([]bbclient.FooterChunk, len(x.Footer.ChunkHashes))
	f := x.Footer
	for i, h := range f.ChunkHashes {
		out[i] = bbclient.FooterChunk{Hash: h, UnpackedEnd: uint64(f.UnpackedChunkOffsets[i]), StoredEnd: uint64(f.ChunkBoundaryOffsets[i])}
	}
	return out
}

func TestRepoKey(t *testing.T) {
	t.Parallel()
	k := repoKey(server, repo)
	require.Len(t, k, 64)
	require.Equal(t, k, repoKey(server+"/", "et/alice/repo/"))
	require.NotEqual(t, k, repoKey(server, "/et/alice/other"))
	require.NotEqual(t, k, repoKey("https://other.example", repo))
}

func TestOpenCreatesPrivateDirectories(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	_, err := Open(dir, server, repo)
	require.NoError(t, err)
	base := filepath.Join(dir, gitdir.XorbStoreDirName, repoKey(server, repo))
	for _, sub := range subdirs {
		info, err := os.Stat(filepath.Join(base, sub))
		require.NoError(t, err)
		require.True(t, info.IsDir())
		if runtime.GOOS != "windows" {
			require.Equal(t, os.FileMode(0o700), info.Mode().Perm())
		}
	}

	_, err = Open(dir, server, "/")
	require.Error(t, err)
	_, err = Open(dir, "", repo)
	require.Error(t, err)
}

func TestXorbRoundTrip(t *testing.T) {
	t.Parallel()
	s, _ := openStore(t, t.TempDir())
	x := testXorb(t, 1)

	_, ok := s.Xorb(x.Hash)
	require.False(t, ok)
	require.NoError(t, s.PutXorb(x))
	got, ok := s.Xorb(x.Hash)
	require.True(t, ok)
	require.Equal(t, x, got)
}

func TestXorbDropsCorruptFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s, _ := openStore(t, dir)
	x := testXorb(t, 2)
	require.NoError(t, s.PutXorb(x))
	path := filepath.Join(dir, filepath.FromSlash(s.name(xorbsDir, x.Hash.String())))
	require.NoError(t, os.WriteFile(path, x.Network[:len(x.Network)-1], 0o600))

	_, ok := s.Xorb(x.Hash)
	require.False(t, ok)
	require.NoFileExists(t, path)
}

func TestStagedRecordTrustExpires(t *testing.T) {
	t.Parallel()
	s, clock := openStore(t, t.TempDir())
	x := testXorb(t, 3)
	require.NoError(t, s.RecordUpload(&bbclient.UploadResult{Xorb: x.Hash, State: bbclient.StateStaged, Chunks: footer(x)}))

	r, ok := s.Record(x.Hash)
	require.True(t, ok)
	require.Equal(t, bbclient.StateStaged, r.State)
	require.Equal(t, footer(x), r.Chunks)
	require.Equal(t, []xethash.Hash{x.Hash}, s.TrustedXorbs())

	clock.advance(stagedTrust - time.Minute)
	_, ok = s.Record(x.Hash)
	require.True(t, ok)

	clock.advance(time.Minute)
	_, ok = s.Record(x.Hash)
	require.False(t, ok)
	require.Empty(t, s.TrustedXorbs())
	_, ok = s.readRecord(x.Hash)
	require.False(t, ok, "TrustedXorbs removes an expired record")
}

func TestStagedRecordFromTheFutureIsNotTrusted(t *testing.T) {
	t.Parallel()
	s, clock := openStore(t, t.TempDir())
	x := testXorb(t, 4)
	require.NoError(t, s.RecordUpload(&bbclient.UploadResult{Xorb: x.Hash, State: bbclient.StateStaged, Chunks: footer(x)}))
	clock.advance(-time.Hour)
	_, ok := s.Record(x.Hash)
	require.False(t, ok)
}

func TestPermanentRecordLastsUntilForget(t *testing.T) {
	t.Parallel()
	s, clock := openStore(t, t.TempDir())
	x := testXorb(t, 5)
	require.NoError(t, s.PutXorb(x))
	require.NoError(t, s.RecordUpload(&bbclient.UploadResult{Xorb: x.Hash, State: bbclient.StatePermanent, Chunks: footer(x)}))

	clock.advance(365 * 24 * time.Hour)
	_, ok := s.Record(x.Hash)
	require.True(t, ok)

	require.NoError(t, s.Forget(x.Hash))
	_, ok = s.Record(x.Hash)
	require.False(t, ok)
	require.NoError(t, s.Forget(x.Hash), "forgetting an absent record is not an error")
	_, ok = s.Xorb(x.Hash)
	require.True(t, ok, "Forget keeps the xorb for a new upload")
}

func TestRecordDedup(t *testing.T) {
	t.Parallel()
	s, clock := openStore(t, t.TempDir())
	staged, stagedNew, missing, permanent, unknown, anchored := testXorb(t, 10), testXorb(t, 11), testXorb(t, 12), testXorb(t, 13), testXorb(t, 15), testXorb(t, 14)
	uploaded := clock.now()
	for _, x := range []*bbchunk.Xorb{staged, missing, permanent} {
		require.NoError(t, s.RecordUpload(&bbclient.UploadResult{Xorb: x.Hash, State: bbclient.StateStaged, Chunks: footer(x)}))
	}
	clock.advance(time.Hour)

	// The answer carries footers for anchor hits only.
	require.NoError(t, s.RecordDedup(&bbclient.DedupResult{
		Anchors: []bbclient.AnchorHit{{Chunk: anchored.Footer.ChunkHashes[0], Xorb: anchored.Hash, Index: 0}},
		Xorbs: []bbclient.XorbState{
			{Xorb: staged.Hash, State: bbclient.StateStaged},
			{Xorb: stagedNew.Hash, State: bbclient.StateStaged},
			{Xorb: missing.Hash, State: bbclient.StateMissing},
			{Xorb: permanent.Hash, State: bbclient.StatePermanent},
			{Xorb: unknown.Hash, State: bbclient.StatePermanent},
		},
		Footers: map[xethash.Hash][]bbclient.FooterChunk{
			anchored.Hash: footer(anchored),
		},
	}))

	r, ok := s.Record(staged.Hash)
	require.True(t, ok)
	require.True(t, r.UploadedAt.Equal(uploaded), "a staged answer keeps the upload time")
	_, ok = s.Record(stagedNew.Hash)
	require.False(t, ok, "a staged xorb without an upload time gets no record")
	_, ok = s.Record(missing.Hash)
	require.False(t, ok)
	r, ok = s.Record(permanent.Hash)
	require.True(t, ok)
	require.Equal(t, bbclient.StatePermanent, r.State)
	require.Equal(t, footer(permanent), r.Chunks, "a permanent answer keeps the recorded footer")
	_, ok = s.Record(unknown.Hash)
	require.False(t, ok, "a permanent xorb without a footer gets no record")
	r, ok = s.Record(anchored.Hash)
	require.True(t, ok)
	require.Equal(t, bbclient.StatePermanent, r.State)
	require.Equal(t, footer(anchored), r.Chunks)
	require.ElementsMatch(t, []xethash.Hash{staged.Hash, permanent.Hash, anchored.Hash}, s.TrustedXorbs())
}

func TestRecordsArePerRepo(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	a, _ := openStore(t, dir)
	b, err := Open(dir, server, "/et/alice/other")
	require.NoError(t, err)
	x := testXorb(t, 20)
	require.NoError(t, a.RecordUpload(&bbclient.UploadResult{Xorb: x.Hash, State: bbclient.StatePermanent, Chunks: footer(x)}))
	require.NoError(t, a.PutXorb(x))

	_, ok := b.Record(x.Hash)
	require.False(t, ok)
	_, ok = b.Xorb(x.Hash)
	require.False(t, ok)
	require.Empty(t, b.TrustedXorbs())
}

func TestCorruptRecordIsDropped(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s, _ := openStore(t, dir)
	x := testXorb(t, 21)
	path := filepath.Join(dir, filepath.FromSlash(s.name(recordsDir, x.Hash.String()+".json")))
	require.NoError(t, os.WriteFile(path, []byte(`{"version":1,"state":"permanent","chunks":[{"hash":"zz"}]}`), 0o600))

	_, ok := s.Record(x.Hash)
	require.False(t, ok)
	require.NoFileExists(t, path)
}

// Trim removes the least recently used files of earlier pushes. Reading a
// xorb counts as a use.
func TestTrimRemovesLeastRecentlyUsed(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	xs := []*bbchunk.Xorb{testXorb(t, 40), testXorb(t, 41), testXorb(t, 42), testXorb(t, 43)}
	size := int64(len(xs[0].Network))
	earlier, clock := openStore(t, dir, withMaxBytes(3*size))
	for _, x := range xs[:3] {
		require.NoError(t, earlier.PutXorb(x))
		clock.advance(time.Second)
	}
	earlier.Trim()

	s, _ := openStore(t, dir, withMaxBytes(3*size))
	s.now = clock.now
	_, ok := s.Xorb(xs[0].Hash)
	require.True(t, ok)
	clock.advance(time.Second)
	require.NoError(t, s.PutXorb(xs[3]))
	entries, err := os.ReadDir(filepath.Join(dir, filepath.FromSlash(s.base), xorbsDir))
	require.NoError(t, err)
	require.Len(t, entries, len(xs), "a write does not evict")
	clock.advance(time.Second)
	s.Trim()

	for i, want := range []bool{true, false, true, true} {
		_, ok := s.Xorb(xs[i].Hash)
		require.Equal(t, want, ok, "xorb %d", i)
	}
}

// A push that writes more than the cap keeps its own xorbs: a "missing
// xorb" refusal may need to upload them again.
func TestTrimKeepsTheFilesOfThePush(t *testing.T) {
	t.Parallel()
	xs := []*bbchunk.Xorb{testXorb(t, 44), testXorb(t, 45), testXorb(t, 46)}
	s, clock := openStore(t, t.TempDir(), withMaxBytes(int64(len(xs[0].Network))))
	for _, x := range xs {
		require.NoError(t, s.PutXorb(x))
		clock.advance(time.Second)
	}
	s.Trim()
	for i, x := range xs {
		_, ok := s.Xorb(x.Hash)
		require.True(t, ok, "xorb %d", i)
	}
}

func TestTrimRemovesStaleTempFiles(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s, clock := openStore(t, dir)
	clock.t = time.Now()
	recDir := filepath.Join(dir, filepath.FromSlash(s.base), recordsDir)
	stale := filepath.Join(recDir, ".stale.json.0123456789abcdef.tmp")
	fresh := filepath.Join(recDir, ".fresh.json.0123456789abcdef.tmp")
	require.NoError(t, os.WriteFile(stale, []byte("x"), 0o600))
	require.NoError(t, os.WriteFile(fresh, []byte("x"), 0o600))
	old := clock.t.Add(-2 * staleTemp)
	require.NoError(t, os.Chtimes(stale, old, old))

	s.Trim()
	require.NoFileExists(t, stale)
	require.FileExists(t, fresh)
}

// Store is safe for concurrent use. Writers, readers and Trim run at once
// against a cap that every file passes. Another Store on the same directory
// writes as a second process, and Trim evicts its files. Run with -race. A
// file that this Store wrote must stay, because Trim keeps it.
func TestConcurrentPutReadTrim(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s, err := Open(dir, server, repo, withMaxBytes(1))
	require.NoError(t, err)
	other, err := Open(dir, server, repo, withMaxBytes(1))
	require.NoError(t, err)

	const writers = 8
	xs := make([]*bbchunk.Xorb, 2*writers)
	for i := range xs {
		xs[i] = testXorb(t, uint64(100+i))
	}
	done := make(chan struct{})
	var trims sync.WaitGroup
	for range 2 {
		trims.Go(func() {
			for {
				select {
				case <-done:
					return
				default:
					s.Trim()
				}
			}
		})
	}

	var wg sync.WaitGroup
	errs := make([]error, 2*writers)
	for i, x := range xs {
		st := s
		if i >= writers {
			st = other
		}
		wg.Go(func() {
			for range 20 {
				if err := st.PutXorb(x); err != nil {
					errs[i] = err
					return
				}
				if err := st.RecordUpload(&bbclient.UploadResult{Xorb: x.Hash, State: bbclient.StatePermanent, Chunks: footer(x)}); err != nil {
					errs[i] = err
					return
				}
				got, ok := st.Xorb(x.Hash)
				if st == s && (!ok || got.Hash != x.Hash) {
					errs[i] = errors.New("xorb written by this store was trimmed")
					return
				}
				_, _ = st.Record(x.Hash)
				_ = st.TrustedXorbs()
			}
		})
	}
	wg.Wait()
	close(done)
	trims.Wait()
	for i, err := range errs {
		require.NoError(t, err, "writer %d", i)
	}
	for _, x := range xs[:writers] {
		_, ok := s.Xorb(x.Hash)
		require.True(t, ok)
		_, ok = s.Record(x.Hash)
		require.True(t, ok)
	}
}
