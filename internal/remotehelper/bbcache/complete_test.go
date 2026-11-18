package bbcache

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jmwoliver/xet-go/xethash"
	"github.com/jmwoliver/xet-go/xorb"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/internal/remotehelper/bbchunk"
	"github.com/entireio/cli/internal/remotehelper/bbclient"
	"github.com/entireio/cli/internal/remotehelper/replicas"
	"github.com/entireio/cli/internal/remotehelper/transport"
)

// putComplete stores the complete form of x as a download does.
func putComplete(t *testing.T, s *Store, x *bbchunk.Xorb) {
	t.Helper()
	body := x.Complete()
	require.NoError(t, s.PutComplete(x.Hash, func(w io.Writer) (*bbchunk.Footer, error) {
		return bbchunk.CheckComplete(io.TeeReader(bytes.NewReader(body), w), x.Hash, int64(len(body)))
	}))
}

func (s *Store) completePath(dir string, h xethash.Hash) string {
	return filepath.Join(dir, filepath.FromSlash(s.name(completeDir, h.String())))
}

// readAll reads chunks [0, n) of the cached xorb h.
func readAll(t *testing.T, s *Store, h xethash.Hash) ([]byte, error) {
	t.Helper()
	cx, ok := s.OpenComplete(h)
	require.True(t, ok)
	defer func() { require.NoError(t, cx.Close()) }()
	r, err := cx.Term(0, cx.Footer().Chunks())
	require.NoError(t, err)
	return io.ReadAll(r)
}

// chunkData returns the unpacked data of x.
func chunkData(t *testing.T, x *bbchunk.Xorb) []byte {
	t.Helper()
	obj, err := xorb.Decode(x.Network)
	require.NoError(t, err)
	var out []byte
	for _, c := range obj.Chunks {
		out = append(out, c.Data...)
	}
	return out
}

func TestCompleteRoundTrip(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s, _ := openStore(t, dir)
	x := testXorb(t, 60)
	require.False(t, s.HasComplete(x.Hash))
	putComplete(t, s, x)

	require.True(t, s.HasComplete(x.Hash))
	b, err := os.ReadFile(s.completePath(dir, x.Hash))
	require.NoError(t, err)
	require.Equal(t, x.Complete(), b)
	got, err := readAll(t, s, x.Hash)
	require.NoError(t, err)
	require.Equal(t, chunkData(t, x), got)

	cx, ok := s.OpenComplete(x.Hash)
	require.True(t, ok)
	r, err := cx.Term(1, 2)
	require.NoError(t, err)
	got, err = io.ReadAll(r)
	require.NoError(t, err)
	require.Equal(t, chunkData(t, x)[x.Footer.UnpackedChunkOffsets[0]:x.Footer.UnpackedChunkOffsets[1]], got)
	require.NoError(t, cx.Close())

	rec, ok := s.Record(x.Hash)
	require.True(t, ok)
	require.Equal(t, bbclient.StatePermanent, rec.State)
	require.Equal(t, footer(x), rec.Chunks, "the record feeds the anchors of a later push")
	require.Contains(t, s.TrustedXorbs(), x.Hash)
}

// Another Store on the same directory, as in another process, finds the
// xorb.
func TestCompleteIsSharedAcrossStores(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	a, _ := openStore(t, dir)
	x := testXorb(t, 61)
	putComplete(t, a, x)
	b, _ := openStore(t, dir)
	require.True(t, b.HasComplete(x.Hash))
	got, err := readAll(t, b, x.Hash)
	require.NoError(t, err)
	require.Equal(t, chunkData(t, x), got)
}

func TestPutCompleteFailureLeavesNoFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s, _ := openStore(t, dir)
	x := testXorb(t, 62)
	want := errors.New("connection reset")

	err := s.PutComplete(x.Hash, func(w io.Writer) (*bbchunk.Footer, error) {
		_, _ = w.Write(x.Complete()[:100]) //nolint:errcheck // test
		return nil, want
	})
	require.ErrorIs(t, err, want)
	err = s.PutComplete(x.Hash, func(w io.Writer) (*bbchunk.Footer, error) {
		_, _ = w.Write(x.Complete()[:100]) //nolint:errcheck // test
		return x.Footer, nil
	})
	require.ErrorContains(t, err, "do not match the footer")

	require.False(t, s.HasComplete(x.Hash))
	entries, err := os.ReadDir(filepath.Join(dir, filepath.FromSlash(s.base), completeDir))
	require.NoError(t, err)
	require.Empty(t, entries, "no temp file stays")
	_, ok := s.Record(x.Hash)
	require.False(t, ok)
}

func TestOpenCompleteDropsBadFooter(t *testing.T) {
	t.Parallel()
	cases := map[string]func(b []byte) []byte{
		"flipped footer hash": func(b []byte) []byte {
			b[len(b)-xorb.FooterSize(3)+8] ^= 1
			return b
		},
		"truncated":  func(b []byte) []byte { return b[:len(b)-1] },
		"empty":      func([]byte) []byte { return nil },
		"no trailer": func(b []byte) []byte { return b[:2] },
	}
	for name, edit := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			s, _ := openStore(t, dir)
			x := testXorb(t, 63)
			putComplete(t, s, x)
			path := s.completePath(dir, x.Hash)
			require.NoError(t, os.WriteFile(path, edit(x.Complete()), 0o600))

			_, ok := s.OpenComplete(x.Hash)
			require.False(t, ok)
			_, err := os.Stat(path)
			require.ErrorIs(t, err, os.ErrNotExist)
		})
	}
}

// downloadServer serves the complete form of x and counts the downloads.
func downloadServer(t *testing.T, x *bbchunk.Xorb) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var downloads atomic.Int32
	body := x.Complete()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != repo+"/bb/xorbs/"+x.Hash.String() {
			http.NotFound(w, r)
			return
		}
		downloads.Add(1)
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.Header().Set("ETag", `"`+x.Hash.String()+`"`)
		_, _ = w.Write(body) //nolint:errcheck // test
	})), &downloads
}

// A chunk that fails its hash check on read removes the cached xorb, and a
// second download brings it back.
func TestCorruptChunkIsDroppedAndDownloadedAgain(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s, _ := openStore(t, dir)
	x := testXorb(t, 64)
	srv, downloads := downloadServer(t, x)
	defer srv.Close()
	client := bbclient.New(transport.New(transport.Config{
		Nodes: replicas.NodeConfig{InitialNodes: []string{srv.URL}, EntryURL: srv.URL},
		Path:  repo,
	}))

	require.NoError(t, client.DownloadAll(t.Context(), []xethash.Hash{x.Hash}, s, nil))
	require.Equal(t, int32(1), downloads.Load())
	path := s.completePath(dir, x.Hash)
	b, err := os.ReadFile(path)
	require.NoError(t, err)
	b[x.Footer.ChunkBoundaryOffsets[0]+xorb.HeaderSize+7] ^= 0x10
	require.NoError(t, os.WriteFile(path, b, 0o600))

	got, err := readAll(t, s, x.Hash)
	require.ErrorIs(t, err, bbchunk.ErrCorruptXorb)
	require.Len(t, got, int(x.Footer.UnpackedChunkOffsets[0]), "the first chunk is intact")
	_, err = os.Stat(path)
	require.ErrorIs(t, err, os.ErrNotExist, "Close removes the corrupt xorb")
	require.False(t, s.HasComplete(x.Hash))

	require.NoError(t, client.DownloadAll(t.Context(), []xethash.Hash{x.Hash}, s, nil))
	require.Equal(t, int32(2), downloads.Load())
	got, err = readAll(t, s, x.Hash)
	require.NoError(t, err)
	require.Equal(t, chunkData(t, x), got)
}

// Trim keeps the complete xorbs that the current fetch found or wrote, even
// over the cap, and removes the others.
func TestTrimKeepsTheXorbsOfTheFetch(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	xs := []*bbchunk.Xorb{testXorb(t, 65), testXorb(t, 66), testXorb(t, 67), testXorb(t, 68)}
	earlier, clock := openStore(t, dir)
	for _, x := range xs[:3] {
		putComplete(t, earlier, x)
		clock.advance(time.Second)
	}

	s, _ := openStore(t, dir, withMaxBytes(1))
	s.now = clock.now
	require.True(t, s.HasComplete(xs[0].Hash), "the fetch needs the oldest xorb")
	clock.advance(time.Second)
	putComplete(t, s, xs[3])
	clock.advance(recentComplete)
	s.Trim()

	for i, want := range []bool{true, false, false, true} {
		require.Equal(t, want, s.HasComplete(xs[i].Hash), "xorb %d", i)
	}
}

// After ReleaseComplete, Trim may remove the xorbs that the fetch used.
func TestReleaseCompleteLetsTrimRemove(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s, clock := openStore(t, dir, withMaxBytes(1))
	x := testXorb(t, 73)
	putComplete(t, s, x)
	clock.advance(recentComplete)
	s.Trim()
	require.FileExists(t, s.completePath(dir, x.Hash), "pinned")
	s.ReleaseComplete([]xethash.Hash{x.Hash})
	s.Trim()
	require.NoFileExists(t, s.completePath(dir, x.Hash))
}

// Trim keeps a complete xorb used in the last 30 minutes, which a fetch in
// another process can be reading, and removes it once it is older.
func TestTrimKeepsRecentCompleteXorbs(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	xs := []*bbchunk.Xorb{testXorb(t, 69), testXorb(t, 70)}
	other, clock := openStore(t, dir)
	for _, x := range xs {
		putComplete(t, other, x)
	}

	s, _ := openStore(t, dir, withMaxBytes(1))
	s.now = clock.now
	clock.advance(recentComplete - time.Second)
	s.Trim()
	for i, x := range xs {
		require.FileExists(t, s.completePath(dir, x.Hash), "xorb %d", i)
	}
	_, ok := s.Record(xs[0].Hash)
	require.False(t, ok, "a record is not a complete xorb")

	clock.advance(time.Second)
	s.Trim()
	for i, x := range xs {
		require.NoFileExists(t, s.completePath(dir, x.Hash), "xorb %d", i)
	}
}

// On Windows, the rename of a downloaded xorb fails when another process
// holds the same xorb open. That process stored it, so PutComplete succeeds.
func TestPutCompleteAcceptsAXorbThatAnotherProcessStored(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s, _ := openStore(t, dir)
	x := testXorb(t, 71)
	other, _ := openStore(t, dir)
	refused := errors.New("access is denied")
	s.rename = func(*os.Root, string, string) error {
		putComplete(t, other, x)
		return refused
	}
	putComplete(t, s, x)
	got, err := readAll(t, s, x.Hash)
	require.NoError(t, err)
	require.Equal(t, chunkData(t, x), got)
	entries, err := os.ReadDir(filepath.Join(dir, filepath.FromSlash(s.base), completeDir))
	require.NoError(t, err)
	require.Len(t, entries, 1, "no temp file stays")

	y := testXorb(t, 72)
	err = s.PutComplete(y.Hash, func(w io.Writer) (*bbchunk.Footer, error) {
		return bbchunk.CheckComplete(io.TeeReader(bytes.NewReader(y.Complete()), w), y.Hash, -1)
	})
	require.ErrorIs(t, err, refused, "without the xorb in place, the failure stays")
	require.False(t, s.HasComplete(y.Hash))
}

func TestCheckWritable(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s, _ := openStore(t, dir)
	require.NoError(t, s.CheckWritable())
	entries, err := os.ReadDir(filepath.Join(dir, filepath.FromSlash(s.base), completeDir))
	require.NoError(t, err)
	require.Empty(t, entries, "the check leaves no file")

	if os.Getuid() == 0 {
		t.Skip("root writes to a read-only directory")
	}
	ro := filepath.Join(dir, filepath.FromSlash(s.base), completeDir)
	require.NoError(t, os.Chmod(ro, 0o500))
	t.Cleanup(func() { require.NoError(t, os.Chmod(ro, 0o700)) })
	require.Error(t, s.CheckWritable())
}
