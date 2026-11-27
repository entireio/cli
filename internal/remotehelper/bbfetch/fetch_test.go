package bbfetch

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/iotest"

	"github.com/jmwoliver/xet-go/xethash"
	"github.com/jmwoliver/xet-go/xorb"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/internal/remotehelper/bbcache"
	"github.com/entireio/cli/internal/remotehelper/bbchunk"
	"github.com/entireio/cli/internal/remotehelper/bbchunk/bbchunktest"
	"github.com/entireio/cli/internal/remotehelper/bbclient"
	"github.com/entireio/cli/internal/remotehelper/gitproto"
)

// chunkBlob chunks blob into xorbs and returns its stored recipe, with the
// byte range of each term.
func chunkBlob(t *testing.T, format string, blob []byte) (*bbchunk.ChunkedPayload, []*bbchunk.Xorb) {
	t.Helper()
	c := &bbchunk.ChunkedPayload{OID: blobOID(format, blob)}
	var xorbs []*bbchunk.Xorb
	var nodes []xethash.Node
	var b xorb.Builder
	seal := func() {
		x, err := bbchunk.Seal(&b)
		require.NoError(t, err)
		first, last, err := x.Footer.StoredRange(0, len(x.Footer.ChunkHashes))
		require.NoError(t, err)
		unpacked, err := x.Footer.UnpackedSize(0, len(x.Footer.ChunkHashes))
		require.NoError(t, err)
		c.Terms = append(c.Terms, bbchunk.ChunkedTerm{
			Xorb: uint32(len(c.Xorbs)), ChunkStart: 0, ChunkEnd: uint32(len(x.Footer.ChunkHashes)),
			UnpackedBytes: unpacked, FirstByte: first, LastByte: last,
		})
		c.Xorbs = append(c.Xorbs, x.Hash)
		xorbs = append(xorbs, x)
	}
	require.NoError(t, bbchunk.Split(t.Context(), bytes.NewReader(blob), func(ch bbchunk.Chunk) error {
		nodes = append(nodes, xethash.Node{Hash: ch.Hash, Size: uint64(len(ch.Data))})
		_, err := b.Add(ch.Data, xorb.Auto)
		return err
	}))
	seal()
	c.FileHash = xethash.File(nodes)
	return c, xorbs
}

// xorbServer is a bbclient.Doer that serves complete xorbs.
type xorbServer struct {
	mu       sync.Mutex
	complete map[xethash.Hash][]byte
	status   int
	gets     int
}

func (s *xorbServer) Do(_ context.Context, method, suffix string, _ io.ReadSeeker, _ func(*http.Request)) (*http.Response, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gets++
	h, err := xethash.Parse(strings.TrimPrefix(suffix, "/bb/xorbs/"))
	body, ok := s.complete[h]
	status := s.status
	switch {
	case method != http.MethodGet || err != nil:
		status = http.StatusBadRequest
	case status == 0 && !ok:
		status = http.StatusNotFound
	case status == 0:
		status = http.StatusOK
	}
	if status != http.StatusOK {
		body = []byte("refused")
	}
	return &http.Response{
		StatusCode:    status,
		Header:        http.Header{"Content-Length": {strconv.Itoa(len(body))}},
		ContentLength: int64(len(body)),
		Body:          io.NopCloser(bytes.NewReader(body)),
	}, nil
}

// fetchCase is a pack of two commits with two large blobs as recipes, the
// xorbs of the blobs, and a cache.
type fetchCase struct {
	repo    *testRepo
	pack    []byte
	recipes []*bbchunk.ChunkedPayload
	xorbs   [][]*bbchunk.Xorb
	dir     string
	store   *bbcache.Store
	server  *xorbServer
}

func newFetchCase(t *testing.T, edit func(recipes []*bbchunk.ChunkedPayload)) *fetchCase {
	t.Helper()
	fc := &fetchCase{repo: newTestRepo(t, "sha1"), dir: t.TempDir(), server: &xorbServer{complete: map[xethash.Hash][]byte{}}}
	blobs := [][]byte{randomBlob(200_000, 31), randomBlob(90_000, 32)}
	extra := map[string]string{}
	var oids []string
	for i, b := range blobs {
		oid := fc.repo.blob(b)
		oids = append(oids, oid)
		extra["big"+strconv.Itoa(i)] = oid
		r, xs := chunkBlob(t, "sha1", b)
		fc.recipes = append(fc.recipes, r)
		fc.xorbs = append(fc.xorbs, xs)
		for _, x := range xs {
			fc.server.complete[x.Hash] = x.Complete()
		}
	}
	if edit != nil {
		edit(fc.recipes)
	}
	base := fc.repo.pack(withoutAny(fc.repo.history(2, extra), oids), true)
	count := binary.BigEndian.Uint32(base[8:12])
	var b bytes.Buffer
	b.WriteString("PACK")
	b.Write(binary.BigEndian.AppendUint32(nil, bbchunk.PackVersionExternal))
	b.Write(binary.BigEndian.AppendUint32(nil, count+uint32(len(blobs))))
	b.Write(base[12 : len(base)-20])
	for i, r := range fc.recipes {
		e, err := bbchunktest.AppendChunkedEntry(nil, int64(len(blobs[i])), r)
		require.NoError(t, err)
		b.Write(e)
	}
	fc.pack = withTrailer("sha1", b.Bytes())
	var err error
	fc.store, err = bbcache.Open(fc.dir, "https://entire.example", "/et/owner/repo")
	require.NoError(t, err)
	return fc
}

// run rewrites the pack and returns the stats, the pack that git gets, and
// the progress text.
func (fc *fetchCase) run(ctx context.Context, t *testing.T) (*Stats, []byte, string, error) {
	t.Helper()
	var out bytes.Buffer
	sw := gitproto.NewSidebandWriter(&out)
	stats, err := RewritePack(ctx, Config{Store: fc.store, Client: bbclient.New(fc.server), ObjectFormat: "sha1"}, bytes.NewReader(fc.pack), sw)
	require.NoError(t, sw.Flush())
	var pack []byte
	var progress strings.Builder
	for b := out.Bytes(); len(b) > 0; {
		n, err := strconv.ParseUint(string(b[:4]), 16, 16)
		require.NoError(t, err)
		switch b[4] {
		case gitproto.BandData:
			pack = append(pack, b[5:n]...)
		case gitproto.BandProgress:
			progress.Write(b[5:n])
		}
		b = b[n:]
	}
	return stats, pack, progress.String(), err
}

func TestRewritePackRebuildsBlobs(t *testing.T) {
	t.Parallel()
	fc := newFetchCase(t, nil)
	// A push of this clone built the first blob's xorb, so the fetch
	// converts it instead of downloading it.
	require.NoError(t, fc.store.PutXorb(fc.xorbs[0][0]))

	stats, pack, progress, err := fc.run(t.Context(), t)
	require.NoError(t, err)
	require.Equal(t, &Stats{Recipes: 2, LogicalBytes: 290_000, Xorbs: 2, Converted: 1, Downloaded: 1,
		DownloadedBytes: int64(len(fc.xorbs[1][0].Complete()))}, stats)
	require.Equal(t, 1, fc.server.gets)
	require.Contains(t, progress, "Downloading large files: 0/1 xorbs, 0.1 MiB\r", "progress counts bytes as they arrive")
	require.Contains(t, progress, "Downloading large files: 1/1 xorbs, 0.1 MiB, done.\n")
	require.Contains(t, progress, "Rebuilding large files: 2/2, done.\n")
	dst, _ := indexPack(t, "sha1", pack)
	require.Equal(t, fc.repo.objectList(), dst.objectList())

	stats, pack, _, err = fc.run(t.Context(), t)
	require.NoError(t, err)
	require.Equal(t, 2, stats.Cached, "every xorb is cached")
	require.Equal(t, 1, fc.server.gets)
	indexPack(t, "sha1", pack)
}

// A cached xorb whose chunk fails its check is removed and downloaded once
// more.
func TestRewritePackDownloadsCorruptXorbAgain(t *testing.T) {
	t.Parallel()
	fc := newFetchCase(t, nil)
	_, _, _, err := fc.run(t.Context(), t)
	require.NoError(t, err)
	h := fc.xorbs[0][0].Hash
	files, err := filepath.Glob(filepath.Join(fc.dir, "*", "*", "complete", h.String()))
	require.NoError(t, err)
	require.Len(t, files, 1)
	b, err := os.ReadFile(files[0])
	require.NoError(t, err)
	b[20] ^= 0xff
	require.NoError(t, os.WriteFile(files[0], b, 0o600))

	stats, pack, _, err := fc.run(t.Context(), t)
	require.NoError(t, err)
	require.Equal(t, 1, stats.Corrupt)
	require.Equal(t, 1, stats.Downloaded)
	require.Equal(t, 1, stats.Cached)
	require.Equal(t, 3, fc.server.gets)
	indexPack(t, "sha1", pack)
}

// Each failure in the recipe path is a RecipeError, and RewritePack writes
// no trailer after it. A cancelled fetch is not a RecipeError: it must not
// record a fallback.
func TestRewritePackRecipeErrors(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		edit  func([]*bbchunk.ChunkedPayload)
		setup func(*fetchCase)
		want  string
	}{
		{
			name: "object ID mismatch",
			edit: func(rs []*bbchunk.ChunkedPayload) { rs[1].OID[0] ^= 0xff },
			want: "the rebuilt content hashes to",
		},
		{
			name: "byte range mismatch",
			edit: func(rs []*bbchunk.ChunkedPayload) { rs[0].Terms[0].LastByte-- },
			want: "the recipe says",
		},
		{
			name:  "download refused",
			setup: func(fc *fetchCase) { fc.server.status = http.StatusNotFound },
			want:  "404",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fc := newFetchCase(t, tc.edit)
			if tc.setup != nil {
				tc.setup(fc)
			}
			_, pack, _, err := fc.run(t.Context(), t)
			var re *RecipeError
			require.ErrorAs(t, err, &re)
			require.ErrorContains(t, err, tc.want)
			sum := packHash("sha1")
			sum.Write(pack[:len(pack)-20])
			require.NotEqual(t, sum.Sum(nil), pack[len(pack)-20:], "no trailer after a failure")
		})
	}

	t.Run("malformed recipe", func(t *testing.T) {
		t.Parallel()
		fc := newFetchCase(t, nil)
		fc.pack = withTrailer("sha1", rawPack(bbchunk.PackVersionExternal, 1, append(bbchunk.AppendObjectHeader(nil, bbchunk.ExternalObject, 10), 0x01, 0)))
		_, _, _, err := fc.run(t.Context(), t)
		var re *RecipeError
		require.ErrorAs(t, err, &re)
	})

	t.Run("cancelled", func(t *testing.T) {
		t.Parallel()
		fc := newFetchCase(t, nil)
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		_, _, _, err := fc.run(ctx, t)
		require.ErrorIs(t, err, context.Canceled)
		var re *RecipeError
		require.NotErrorAs(t, err, &re, "a cancelled fetch is not a recipe failure")
	})
}

// packOf builds a 0xe002 pack that holds only the given type-5 entries.
func packOf(t *testing.T, sizes []int64, recipes []*bbchunk.ChunkedPayload) []byte {
	t.Helper()
	var entries [][]byte
	for i, r := range recipes {
		e, err := bbchunktest.AppendChunkedEntry(nil, sizes[i], r)
		require.NoError(t, err)
		entries = append(entries, e)
	}
	return rawPack(bbchunk.PackVersionExternal, len(entries), entries...)
}

// requireMalformed checks that err is a RecipeError that wraps
// ErrMalformedPack and that nothing was downloaded.
func requireMalformed(t *testing.T, fc *fetchCase, err error, want string) {
	t.Helper()
	var re *RecipeError
	require.ErrorAs(t, err, &re)
	require.ErrorIs(t, err, ErrMalformedPack)
	require.ErrorContains(t, err, want)
	require.Zero(t, fc.server.gets, "no download for a bad recipe")
}

// A fetch refuses a recipe of a blob under 64 KiB or over 1 GiB, and two
// recipes for one blob, before it downloads anything.
func TestRewritePackRefusesBadRecipeSets(t *testing.T) {
	t.Parallel()
	t.Run("small", func(t *testing.T) {
		t.Parallel()
		fc := newFetchCase(t, nil)
		blob := randomBlob(minChunkedSize-1, 40)
		r, _ := chunkBlob(t, "sha1", blob)
		fc.pack = packOf(t, []int64{int64(len(blob))}, []*bbchunk.ChunkedPayload{r})
		_, _, _, err := fc.run(t.Context(), t)
		requireMalformed(t, fc, err, "logical size 65535 is outside")
	})
	t.Run("large", func(t *testing.T) {
		t.Parallel()
		fc := newFetchCase(t, nil)
		const size = bbchunk.MaxObjectSize + 1
		r := &bbchunk.ChunkedPayload{OID: make([]byte, 20), Xorbs: []xethash.Hash{{1}}}
		for left := uint64(size); left > 0; {
			n := min(left, 60<<20)
			r.Terms = append(r.Terms, bbchunk.ChunkedTerm{ChunkEnd: 1, UnpackedBytes: n, LastByte: 10})
			left -= n
		}
		fc.pack = packOf(t, []int64{size}, []*bbchunk.ChunkedPayload{r})
		_, _, _, err := fc.run(t.Context(), t)
		requireMalformed(t, fc, err, "is outside")
	})
	t.Run("two recipes for one blob", func(t *testing.T) {
		t.Parallel()
		fc := newFetchCase(t, nil)
		size := int64(fc.recipes[0].Terms[0].UnpackedBytes)
		fc.pack = packOf(t, []int64{size, size}, []*bbchunk.ChunkedPayload{fc.recipes[0], fc.recipes[0]})
		_, _, _, err := fc.run(t.Context(), t)
		requireMalformed(t, fc, err, "two recipes for blob")
	})
}

// A failure of the stream is not a RecipeError: the fetch must not fall
// back for a broken connection, a band-3 error, or a failed write to git.
func TestRewritePackStreamErrors(t *testing.T) {
	t.Parallel()
	requireStream := func(t *testing.T, err error) {
		t.Helper()
		var se *StreamError
		require.ErrorAs(t, err, &se)
		var re *RecipeError
		require.NotErrorAs(t, err, &re)
	}
	t.Run("connection reset in the pack", func(t *testing.T) {
		t.Parallel()
		fc := newFetchCase(t, nil)
		for _, off := range []int{20, len(fc.pack) / 2, len(fc.pack) - 30} {
			in := io.MultiReader(bytes.NewReader(fc.pack[:off]), iotest.ErrReader(errors.New("connection reset")))
			_, err := RewritePack(t.Context(), Config{Store: fc.store, Client: bbclient.New(fc.server), ObjectFormat: "sha1"},
				in, gitproto.NewSidebandWriter(io.Discard))
			requireStream(t, err)
		}
	})
	t.Run("write to git fails during the rebuild", func(t *testing.T) {
		t.Parallel()
		fc := newFetchCase(t, nil)
		w := &failingWriter{n: len(fc.pack) - 1000, err: errors.New("broken pipe")}
		_, err := RewritePack(t.Context(), Config{Store: fc.store, Client: bbclient.New(fc.server), ObjectFormat: "sha1"},
			bytes.NewReader(fc.pack), gitproto.NewSidebandWriter(w))
		requireStream(t, err)
	})
}

// evictingWriter removes the cached xorb file when the rebuild of the
// first blob starts, as a trim of another process can.
type evictingWriter struct {
	bytes.Buffer

	path    string
	evicted bool
}

func (w *evictingWriter) Write(p []byte) (int, error) {
	if !w.evicted && bytes.Contains(p, []byte("Rebuilding large files: 0/")) {
		w.evicted = true
		if err := os.Remove(w.path); err != nil {
			return 0, err
		}
	}
	return w.Buffer.Write(p)
}

// A cached xorb that leaves the cache between the check and the rebuild is
// downloaded again, once.
func TestRewritePackDownloadsEvictedXorbAgain(t *testing.T) {
	t.Parallel()
	fc := newFetchCase(t, nil)
	_, _, _, err := fc.run(t.Context(), t)
	require.NoError(t, err)
	gets := fc.server.gets
	h := fc.xorbs[0][0].Hash
	files, err := filepath.Glob(filepath.Join(fc.dir, "*", "*", "complete", h.String()))
	require.NoError(t, err)
	require.Len(t, files, 1)

	w := &evictingWriter{path: files[0]}
	stats, err := RewritePack(t.Context(), Config{Store: fc.store, Client: bbclient.New(fc.server), ObjectFormat: "sha1"},
		bytes.NewReader(fc.pack), gitproto.NewSidebandWriter(w))
	require.NoError(t, err)
	require.True(t, w.evicted)
	require.Equal(t, 2, stats.Cached)
	require.Equal(t, 1, stats.Downloaded)
	require.Equal(t, gets+1, fc.server.gets)
}

// The check of a cached xorb reads only the chunks that the recipes name.
// A damaged chunk outside them costs no download.
func TestRewritePackChecksOnlyTheNamedChunks(t *testing.T) {
	t.Parallel()
	blob := randomBlob(150_000, 41)
	var chunks [][]byte
	require.NoError(t, bbchunk.Split(t.Context(), bytes.NewReader(blob), func(c bbchunk.Chunk) error {
		chunks = append(chunks, c.Data)
		return nil
	}))
	var b xorb.Builder
	var nodes []xethash.Node
	for _, c := range chunks {
		_, err := b.Add(c, xorb.Auto)
		require.NoError(t, err)
	}
	for _, c := range chunks {
		h := xethash.Chunk(c)
		nodes = append(nodes, xethash.Node{Hash: h, Size: uint64(len(c))})
	}
	extra := randomBlob(20_000, 42)
	_, err := b.Add(extra, xorb.Auto)
	require.NoError(t, err)
	x, err := bbchunk.Seal(&b)
	require.NoError(t, err)
	f := x.Footer
	first, last, err := f.StoredRange(0, len(chunks))
	require.NoError(t, err)
	r := &bbchunk.ChunkedPayload{
		OID: blobOID("sha1", blob), FileHash: xethash.File(nodes), Xorbs: []xethash.Hash{x.Hash},
		Terms: []bbchunk.ChunkedTerm{{ChunkEnd: uint32(len(chunks)), UnpackedBytes: uint64(len(blob)), FirstByte: first, LastByte: last}},
	}

	fc := newFetchCase(t, nil)
	fc.server.complete[x.Hash] = x.Complete()
	fc.pack = packOf(t, []int64{int64(len(blob))}, []*bbchunk.ChunkedPayload{r})
	_, _, _, err = fc.run(t.Context(), t)
	require.NoError(t, err)
	files, err := filepath.Glob(filepath.Join(fc.dir, "*", "*", "complete", x.Hash.String()))
	require.NoError(t, err)
	require.Len(t, files, 1)
	data, err := os.ReadFile(files[0])
	require.NoError(t, err)
	data[f.ChunkBoundaryOffsets[len(chunks)]-10] ^= 0xff
	require.NoError(t, os.WriteFile(files[0], data, 0o600))

	stats, pack, _, err := fc.run(t.Context(), t)
	require.NoError(t, err)
	require.Equal(t, 1, stats.Cached)
	require.Zero(t, stats.Corrupt)
	require.Equal(t, 1, fc.server.gets, "no download for the damaged chunk that no recipe names")
	sum := packHash("sha1")
	sum.Write(pack[:len(pack)-20])
	require.Equal(t, sum.Sum(nil), pack[len(pack)-20:])
}
