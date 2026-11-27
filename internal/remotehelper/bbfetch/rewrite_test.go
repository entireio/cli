package bbfetch

import (
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"math/rand/v2"
	"strings"
	"sync"
	"testing"
	"testing/iotest"
	"time"

	"github.com/jmwoliver/xet-go/xethash"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/internal/remotehelper/bbchunk"
	"github.com/entireio/cli/internal/remotehelper/bbchunk/bbchunktest"
	"github.com/entireio/cli/internal/remotehelper/gitproto"
)

var objectFormats = []string{"sha1", "sha256"}

func packHash(format string) hash.Hash {
	if format == "sha256" {
		return sha256.New()
	}
	return sha1.New()
}

// rewrite runs ReadPack on in and appends blobs[i] for the i-th type-5
// entry. It checks each blob's object ID against its recipe.
func rewrite(t *testing.T, format string, in io.Reader, blobs [][]byte) ([]byte, []ChunkedEntry, error) {
	t.Helper()
	var out bytes.Buffer
	rw, err := NewRewriter(&out, format)
	require.NoError(t, err)
	var entries []ChunkedEntry
	if err := rw.ReadPack(in, func(e ChunkedEntry) error {
		entries = append(entries, e)
		return nil
	}); err != nil {
		return out.Bytes(), entries, err
	}
	hashSize := packHash(format).Size()
	for i, e := range entries {
		require.Less(t, i, len(blobs), "more type-5 entries than blobs")
		oid, err := rw.AppendBlob(int64(len(blobs[i])), bytes.NewReader(blobs[i]))
		require.NoError(t, err)
		require.Equal(t, hex.EncodeToString(e.Payload[:hashSize]), hex.EncodeToString(oid))
	}
	err = rw.Finish()
	return out.Bytes(), entries, err
}

// withTrailer appends the trailer of pack to it.
func withTrailer(format string, pack []byte) []byte {
	h := packHash(format)
	h.Write(pack)
	return h.Sum(pack)
}

// chunkedPack builds a 0xe002 pack: the entries of base, a git pack, then one
// type-5 entry for each blob.
func chunkedPack(t *testing.T, format string, base []byte, blobs [][]byte) []byte {
	t.Helper()
	hashSize := packHash(format).Size()
	count := binary.BigEndian.Uint32(base[8:12])
	var b bytes.Buffer
	b.WriteString("PACK")
	b.Write(binary.BigEndian.AppendUint32(nil, bbchunk.PackVersionExternal))
	b.Write(binary.BigEndian.AppendUint32(nil, count+uint32(len(blobs))))
	b.Write(base[12 : len(base)-hashSize])
	for _, blob := range blobs {
		b.Write(chunkedEntry(t, format, blob))
	}
	return withTrailer(format, b.Bytes())
}

// chunkedEntry returns a meta 0x02 entry for blob. The recipe splits the
// blob into terms over two xorbs; nothing reads the xorbs here.
func chunkedEntry(t *testing.T, format string, blob []byte) []byte {
	t.Helper()
	half := uint64(len(blob) / 2)
	c := &bbchunk.ChunkedPayload{
		OID:      blobOID(format, blob),
		FileHash: xethash.Hash{1},
		Xorbs:    []xethash.Hash{{2}, {3}},
		Terms: []bbchunk.ChunkedTerm{
			{Xorb: 0, ChunkStart: 0, ChunkEnd: 3, UnpackedBytes: half, FirstByte: 0, LastByte: 99},
			{Xorb: 1, ChunkStart: 1, ChunkEnd: 2, UnpackedBytes: uint64(len(blob)) - half, FirstByte: 50, LastByte: 70},
		},
	}
	e, err := bbchunktest.AppendChunkedEntry(nil, int64(len(blob)), c)
	require.NoError(t, err)
	return e
}

func blobOID(format string, blob []byte) []byte {
	h := packHash(format)
	fmt.Fprintf(h, "blob %d\x00", len(blob))
	h.Write(blob)
	return h.Sum(nil)
}

func randomBlob(n int, seed uint64) []byte {
	r := rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(r.Uint32())
	}
	return b
}

// TestRewritePassesGitPacks checks that a pack without type-5 entries comes
// out unchanged, for every reader shape, object format and delta kind.
func TestRewritePassesGitPacks(t *testing.T) {
	t.Parallel()
	for _, format := range objectFormats {
		for _, ofs := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/ofs=%v", format, ofs), func(t *testing.T) {
				t.Parallel()
				repo := newTestRepo(t, format)
				objects := repo.history(6, map[string]string{"random": repo.blob(randomBlob(300_000, 1))})
				pack := repo.pack(objects, ofs)
				for name, in := range map[string]io.Reader{
					"whole":    bytes.NewReader(pack),
					"one byte": iotest.OneByteReader(bytes.NewReader(pack)),
					"half":     iotest.HalfReader(bytes.NewReader(pack)),
				} {
					out, entries, err := rewrite(t, format, in, nil)
					require.NoError(t, err, name)
					require.Empty(t, entries)
					require.Equal(t, pack, out, name)
				}
				dst, verify := indexPack(t, format, pack)
				require.Equal(t, repo.objectList(), dst.objectList())
				require.Contains(t, verify, "chain length = 1", "the pack has deltas")
			})
		}
	}
}

// TestRewriteAcceptsE002WithoutChunks: a server may send version 0xe002 for
// a pack with no type-5 entry. The rewriter writes it as version 2.
func TestRewriteAcceptsE002WithoutChunks(t *testing.T) {
	t.Parallel()
	repo := newTestRepo(t, "sha1")
	pack := repo.pack(repo.history(2, nil), true)
	e002 := bytes.Clone(pack[:len(pack)-sha1.Size])
	binary.BigEndian.PutUint32(e002[4:], bbchunk.PackVersionExternal)
	out, _, err := rewrite(t, "sha1", bytes.NewReader(withTrailer("sha1", e002)), nil)
	require.NoError(t, err)
	require.Equal(t, pack, out)
}

// TestRewriteReplacesChunkedEntries builds 0xe002 packs whose last entries
// are recipes, replaces them with blobs, and checks the result with git.
func TestRewriteReplacesChunkedEntries(t *testing.T) {
	t.Parallel()
	for _, format := range objectFormats {
		for _, ofs := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/ofs=%v", format, ofs), func(t *testing.T) {
				t.Parallel()
				repo := newTestRepo(t, format)
				blobs := [][]byte{
					randomBlob(200_000, 2),
					bytes.Repeat([]byte("large file\n"), 2*maxStoredBlock/11+1),
					randomBlob(2*maxStoredBlock, 3),
					{'x', 'y'},
				}
				extra := map[string]string{}
				var chunked []string
				for i, b := range blobs {
					oid := repo.blob(b)
					extra[fmt.Sprintf("big%d", i)] = oid
					chunked = append(chunked, oid)
				}
				objects := repo.history(5, extra)
				// The base pack holds every object but the chunked blobs.
				base := repo.pack(withoutAny(objects, chunked), ofs)
				in := chunkedPack(t, format, base, blobs)

				out, entries, err := rewrite(t, format, iotest.HalfReader(bytes.NewReader(in)), blobs)
				require.NoError(t, err)
				require.Len(t, entries, len(blobs))
				for i, e := range entries {
					require.Equal(t, int64(len(blobs[i])), e.Size)
					_, err := bbchunk.ParseChunkedPayload(e.Payload, packHash(format).Size(), e.Size)
					require.NoError(t, err)
				}
				hashSize := packHash(format).Size()
				require.Equal(t, base[12:len(base)-hashSize], out[12:len(base)-hashSize], "ordinary entries keep their bytes and offsets")
				require.Equal(t, binary.BigEndian.Uint32(in[8:12]), binary.BigEndian.Uint32(out[8:12]))

				dst, _ := indexPack(t, format, out)
				require.Equal(t, repo.objectList(), dst.objectList())
				for i, oid := range chunked {
					require.Equal(t, blobs[i], dst.gitIn(nil, dst.dir, "cat-file", "blob", oid))
				}
			})
		}
	}
}

func withoutAny(all, drop []string) []string {
	var out []string
	for _, o := range all {
		keep := true
		for _, d := range drop {
			keep = keep && o != d
		}
		if keep {
			out = append(out, o)
		}
	}
	return out
}

// TestRewriteThroughFetchResponse runs the rewriter inside
// gitproto.CopyFetchResponse, as the helper will, and checks the pack that
// git would get.
func TestRewriteThroughFetchResponse(t *testing.T) {
	t.Parallel()
	repo := newTestRepo(t, "sha1")
	blob := randomBlob(150_000, 4)
	oid := repo.blob(blob)
	objects := repo.history(3, map[string]string{"big": oid})
	in := chunkedPack(t, "sha1", repo.pack(withoutAny(objects, []string{oid}), true), [][]byte{blob})

	var resp bytes.Buffer
	resp.WriteString("000e\x01packfile\n")
	mux := gitproto.NewSidebandWriter(&resp)
	for i := 0; i < len(in); i += 1000 {
		_, err := mux.Write(in[i:min(i+1000, len(in))])
		require.NoError(t, err)
		require.NoError(t, mux.Flush())
		if i == len(in)/2000*1000 {
			require.NoError(t, mux.Progress([]byte("Counting objects\r")))
		}
	}
	resp.WriteString("0000")

	var git bytes.Buffer
	err := gitproto.CopyFetchResponse(&git, &resp, true, func(data io.Reader, out *gitproto.SidebandWriter) error {
		rw, err := NewRewriter(out, "sha1")
		require.NoError(t, err)
		var entries []ChunkedEntry
		if err := rw.ReadPack(data, func(e ChunkedEntry) error {
			entries = append(entries, e)
			return nil
		}); err != nil {
			return err
		}
		require.Len(t, entries, 1)
		if err := out.Progress([]byte("Rebuilding large files\n")); err != nil {
			return err
		}
		if _, err := rw.AppendBlob(entries[0].Size, bytes.NewReader(blob)); err != nil {
			return err
		}
		return rw.Finish()
	})
	require.NoError(t, err)

	f := gitproto.NewFetchResponseReader(&git, true)
	var pack []byte
	var progress []string
	for {
		p, err := f.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		require.NoError(t, err)
		if p.Kind == gitproto.FetchPackData {
			pack = append(pack, p.Payload...)
		}
		if p.Kind == gitproto.FetchProgress {
			progress = append(progress, string(p.Payload))
		}
	}
	require.Equal(t, []string{"Counting objects\r", "Rebuilding large files\n"}, progress)
	dst, _ := indexPack(t, "sha1", pack)
	require.Equal(t, repo.objectList(), dst.objectList())
}

// signalWriter reports each write on a channel.
type signalWriter struct {
	mu    sync.Mutex
	buf   bytes.Buffer
	wrote chan struct{}
}

func (w *signalWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf.Write(p)
	select {
	case w.wrote <- struct{}{}:
	default:
	}
	return len(p), nil
}

func (w *signalWriter) len() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Len()
}

// TestRewriteStreams checks that ordinary entries reach the output while
// the rest of the pack has not arrived.
func TestRewriteStreams(t *testing.T) {
	t.Parallel()
	repo := newTestRepo(t, "sha1")
	pack := repo.pack(repo.history(4, map[string]string{"r": repo.blob(randomBlob(400_000, 5))}), true)
	pr, pw := io.Pipe()
	out := &signalWriter{wrote: make(chan struct{}, 1)}
	rw, err := NewRewriter(out, "sha1")
	require.NoError(t, err)
	done := make(chan error, 1)
	go func() { done <- rw.ReadPack(pr, func(ChunkedEntry) error { return nil }) }()

	half := len(pack) / 2
	_, err = pw.Write(pack[:half])
	require.NoError(t, err)
	deadline := time.After(10 * time.Second)
	for out.len() < half-readBufferSize {
		select {
		case <-out.wrote:
		case <-deadline:
			t.Fatalf("%d of the first %d bytes reached the output", out.len(), half)
		}
	}
	_, err = pw.Write(pack[half:])
	require.NoError(t, err)
	require.NoError(t, pw.Close())
	require.NoError(t, <-done)
	require.NoError(t, rw.Finish())
	require.Equal(t, pack, out.buf.Bytes())
}

func TestAppendBlobStoredDeflate(t *testing.T) {
	t.Parallel()
	for _, n := range []int{0, 1, maxStoredBlock - 1, maxStoredBlock, maxStoredBlock + 1, 3*maxStoredBlock + 7} {
		var out bytes.Buffer
		rw := &Rewriter{out: &out, newHash: sha1.New, hashSize: sha1.Size, outHash: sha1.New(), state: stateAppending, externals: 1}
		data := randomBlob(n, uint64(n))
		oid, err := rw.AppendBlob(int64(n), bytes.NewReader(data))
		require.NoError(t, err)
		require.Equal(t, blobOID("sha1", data), oid)

		header := bbchunk.AppendObjectHeader(nil, objBlob, int64(n))
		require.Equal(t, header, out.Bytes()[:len(header)])
		zr, err := zlib.NewReader(bytes.NewReader(out.Bytes()[len(header):]))
		require.NoError(t, err)
		got, err := io.ReadAll(zr)
		require.NoError(t, err)
		require.Equal(t, data, got)
		blocks := max(1, (n+maxStoredBlock-1)/maxStoredBlock)
		require.Equal(t, len(header)+2+5*blocks+n+4, out.Len(), "stored blocks only")
	}
}

func TestRewriterCallOrder(t *testing.T) {
	t.Parallel()
	repo := newTestRepo(t, "sha1")
	base := repo.pack(repo.history(1, nil), true)
	blob := []byte("some large file")
	in := chunkedPack(t, "sha1", base, [][]byte{blob})

	rw, err := NewRewriter(io.Discard, "sha1")
	require.NoError(t, err)
	_, err = rw.AppendBlob(1, strings.NewReader("x"))
	require.Error(t, err, "AppendBlob before ReadPack")
	require.Error(t, rw.Finish(), "Finish before ReadPack")
	require.NoError(t, rw.ReadPack(bytes.NewReader(in), func(ChunkedEntry) error { return nil }))
	require.Error(t, rw.ReadPack(bytes.NewReader(in), nil), "ReadPack twice")
	require.ErrorContains(t, rw.Finish(), "0 of 1 type-5 entries replaced")
	_, err = rw.AppendBlob(int64(len(blob)), bytes.NewReader(blob))
	require.Error(t, err, "the rewriter refuses every call after an error")

	rw, err = NewRewriter(io.Discard, "sha1")
	require.NoError(t, err)
	require.NoError(t, rw.ReadPack(bytes.NewReader(in), func(ChunkedEntry) error { return nil }))
	_, err = rw.AppendBlob(int64(len(blob)), strings.NewReader("short"))
	require.ErrorIs(t, err, io.ErrUnexpectedEOF)
	require.Error(t, rw.Finish())

	rw, err = NewRewriter(io.Discard, "sha1")
	require.NoError(t, err)
	require.NoError(t, rw.ReadPack(bytes.NewReader(in), func(ChunkedEntry) error { return nil }))
	_, err = rw.AppendBlob(int64(len(blob)), bytes.NewReader(blob))
	require.NoError(t, err)
	_, err = rw.AppendBlob(1, strings.NewReader("x"))
	require.ErrorContains(t, err, "all replaced")

	_, err = NewRewriter(io.Discard, "md5")
	require.Error(t, err)
}

// deflate returns the zlib stream of data.
func deflate(data []byte) []byte {
	var b bytes.Buffer
	zw := zlib.NewWriter(&b)
	if _, err := zw.Write(data); err != nil {
		panic(err)
	}
	if err := zw.Close(); err != nil {
		panic(err)
	}
	return b.Bytes()
}

// rawPack builds a pack of version with the given entries and its trailer.
func rawPack(version uint32, count int, entries ...[]byte) []byte {
	b := []byte("PACK")
	b = binary.BigEndian.AppendUint32(b, version)
	b = binary.BigEndian.AppendUint32(b, uint32(count))
	for _, e := range entries {
		b = append(b, e...)
	}
	return withTrailer("sha1", b)
}

func blobEntry(data []byte) []byte {
	return append(bbchunk.AppendObjectHeader(nil, objBlob, int64(len(data))), deflate(data)...)
}

func TestReadPackRefuses(t *testing.T) {
	t.Parallel()
	blob := []byte("a large file that the server keeps in chunks")
	chunked := chunkedEntry(t, "sha1", blob)
	refDelta := append(bbchunk.AppendObjectHeader(nil, objRefDelta, 4), blobOID("sha1", blob)...)
	refDelta = append(refDelta, deflate([]byte{1, 1, 0x91, 0})...)
	withMeta := func(meta byte) []byte {
		e := bytes.Clone(chunked)
		i := 0
		for e[i]&0x80 != 0 {
			i++
		}
		e[i+1] = meta
		return e
	}
	small := blobEntry([]byte("hello"))
	ok := rawPack(bbchunk.PackVersionExternal, 2, small, chunked)
	_, _, err := rewrite(t, "sha1", bytes.NewReader(ok), [][]byte{blob})
	require.NoError(t, err)

	badTrailer := bytes.Clone(ok)
	badTrailer[len(badTrailer)-1] ^= 1
	truncatedZlib := rawPack(2, 1, small[:len(small)-3])
	wrongSize := rawPack(2, 1, append(bbchunk.AppendObjectHeader(nil, objBlob, 6), deflate([]byte("hello"))...))
	bigPayload := append(bbchunk.AppendObjectHeader(nil, bbchunk.ExternalObject, 10), bbchunk.MetaChunked)
	bigPayload = binary.AppendUvarint(bigPayload, bbchunk.MaxPayloadSize+1)

	tests := []struct {
		name string
		in   []byte
		want string
	}{
		{"empty", nil, "read header"},
		{"short header", []byte("PACK\x00\x00"), "read header"},
		{"signature", append([]byte("KCAP"), ok[4:]...), "header"},
		{"version 3", rawPack(3, 1, small), "header"},
		{"type 5 in a version-2 pack", rawPack(2, 2, small, chunked), "version-2 pack"},
		{"an entry after a type-5 entry", rawPack(bbchunk.PackVersionExternal, 2, chunked, small), "after a type-5 entry"},
		{"meta 0x01", rawPack(bbchunk.PackVersionExternal, 1, withMeta(0x01)), "kind 0x1"},
		{"meta 0x03", rawPack(bbchunk.PackVersionExternal, 1, withMeta(bbchunk.MetaClientChunked)), "kind 0x3"},
		{"payload over the limit", rawPack(bbchunk.PackVersionExternal, 1, bigPayload), "is over 1048576"},
		{"REF_DELTA base is a type-5 entry", rawPack(bbchunk.PackVersionExternal, 2, refDelta, chunked), "REF_DELTA base"},
		{"trailer", badTrailer, "trailer"},
		{"data after the trailer", append(bytes.Clone(ok), 0), "after the trailer"},
		{"missing trailer", ok[:len(ok)-1], "read trailer"},
		{"truncated zlib", truncatedZlib, "inflate"},
		{"size mismatch", wrongSize, "inflates to 5 bytes"},
		{"type 0", rawPack(2, 1, []byte{0x00}), "entry type 0"},
		{"OFS_DELTA offset overflow", rawPack(2, 1, append([]byte{0x60}, bytes.Repeat([]byte{0xff}, 10)...)), "overflows"},
		{"header overflow", rawPack(2, 1, bytes.Repeat([]byte{0xff}, 12)), "overflows"},
		{"truncated entry header", []byte("PACK\x00\x00\x00\x02\x00\x00\x00\x01\x95"), "truncated entry header"},
		{"count past the end", rawPack(2, 2, small), "entry 1"},
		{"not zlib", rawPack(2, 1, []byte{0x35, 0xff, 0xff}), "zlib header"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, _, err := rewrite(t, "sha1", bytes.NewReader(tc.in), [][]byte{blob, blob})
			require.ErrorContains(t, err, tc.want)
		})
	}

	boom := errors.New("boom")
	rw, err := NewRewriter(io.Discard, "sha1")
	require.NoError(t, err)
	require.ErrorIs(t, rw.ReadPack(bytes.NewReader(ok), func(ChunkedEntry) error { return boom }), boom)
}

// FuzzReadPack checks that ReadPack never panics.
func FuzzReadPack(f *testing.F) {
	blob := []byte("a large file")
	c := &bbchunk.ChunkedPayload{OID: blobOID("sha1", blob), Xorbs: []xethash.Hash{{1}}, Terms: []bbchunk.ChunkedTerm{{ChunkEnd: 1, UnpackedBytes: uint64(len(blob))}}}
	chunked, err := bbchunktest.AppendChunkedEntry(nil, int64(len(blob)), c)
	require.NoError(f, err)
	f.Add(rawPack(bbchunk.PackVersionExternal, 2, blobEntry([]byte("hello")), chunked))
	f.Add(rawPack(2, 1, blobEntry([]byte("hello"))))
	f.Fuzz(func(t *testing.T, in []byte) {
		rw, err := NewRewriter(io.Discard, "sha1")
		require.NoError(t, err)
		entries := 0
		if rw.ReadPack(bytes.NewReader(in), func(ChunkedEntry) error { entries++; return nil }) == nil && entries == 0 {
			require.NoError(t, rw.Finish())
		}
	})
}

// failingWriter fails every write after the first n bytes.
type failingWriter struct {
	n   int
	err error
}

func (w *failingWriter) Write(p []byte) (int, error) {
	if len(p) > w.n {
		k := w.n
		w.n = 0
		return k, w.err
	}
	w.n -= len(p)
	return len(p), nil
}

// A failure of the stream around the pack is a *StreamError and never wraps
// ErrMalformedPack: a broken connection, a response that ends inside a
// packet, a band-3 error of the server, and a failed write to git. A pack
// that ends cleanly before its trailer is malformed.
func TestReadPackStreamErrors(t *testing.T) {
	t.Parallel()
	blob := randomBlob(70_000, 9)
	head := blobEntry(randomBlob(5_000, 10))
	pack := rawPack(bbchunk.PackVersionExternal, 2, head, chunkedEntry(t, "sha1", blob))
	chunkedAt := packHeaderSize + len(head)
	offsets := map[string]int{
		"in the header":             6,
		"before the first entry":    packHeaderSize,
		"in an entry header":        packHeaderSize + 1,
		"in a zlib stream":          packHeaderSize + len(head)/2,
		"in a type-5 frame":         chunkedAt + 3,
		"in a type-5 payload":       chunkedAt + 40,
		"in the trailer":            len(pack) - 5,
		"after the trailer arrived": len(pack),
	}
	requireStream := func(t *testing.T, err error) {
		t.Helper()
		var se *StreamError
		require.ErrorAs(t, err, &se)
		require.NotErrorIs(t, err, ErrMalformedPack)
	}
	reset := errors.New("connection reset by peer")
	for name, off := range offsets {
		t.Run("reset "+name, func(t *testing.T) {
			t.Parallel()
			in := io.MultiReader(bytes.NewReader(pack[:off]), iotest.ErrReader(reset))
			_, _, err := rewrite(t, "sha1", in, [][]byte{blob})
			requireStream(t, err)
			require.ErrorIs(t, err, reset)
		})
		t.Run("truncated response "+name, func(t *testing.T) {
			t.Parallel()
			resp := fetchResponse(t, pack)
			// "000dpackfile\n", then 5 header bytes before each 1000 bytes.
			cut := 13 + off/1000*1005 + 5 + off%1000
			err := copyThroughRewriter(t, resp[:cut], io.Discard, blob)
			requireStream(t, err)
			require.ErrorIs(t, err, io.ErrUnexpectedEOF)
		})
		t.Run("band 3 "+name, func(t *testing.T) {
			t.Parallel()
			resp := fetchResponse(t, pack[:off])
			resp = append(resp[:len(resp)-4], "0016\x03upload-pack: boom"...)
			err := copyThroughRewriter(t, resp, io.Discard, blob)
			requireStream(t, err)
			var remote *gitproto.RemoteError
			require.ErrorAs(t, err, &remote)
			require.Equal(t, "upload-pack: boom", remote.Message)
		})
		if off < len(pack) {
			t.Run("pack ends early "+name, func(t *testing.T) {
				t.Parallel()
				_, _, err := rewrite(t, "sha1", bytes.NewReader(pack[:off]), [][]byte{blob})
				require.ErrorIs(t, err, ErrMalformedPack)
				var se *StreamError
				require.NotErrorAs(t, err, &se)
			})
		}
	}
	for _, n := range []int{0, packHeaderSize, packHeaderSize + 100, len(pack) - 30} {
		t.Run(fmt.Sprintf("write to git fails after %d bytes", n), func(t *testing.T) {
			t.Parallel()
			full := randomBlob(70_000, 9)
			rw, err := NewRewriter(&failingWriter{n: n, err: errors.New("broken pipe")}, "sha1")
			require.NoError(t, err)
			err = rw.ReadPack(bytes.NewReader(pack), func(ChunkedEntry) error { return nil })
			if err == nil {
				_, err = rw.AppendBlob(int64(len(full)), bytes.NewReader(full))
			}
			if err == nil {
				err = rw.Finish()
			}
			requireStream(t, err)
		})
	}
}

// fetchResponse returns a v2 fetch response whose packfile section carries
// pack in one band-1 packet per 1000 bytes.
func fetchResponse(t *testing.T, pack []byte) []byte {
	t.Helper()
	var resp bytes.Buffer
	resp.WriteString("000dpackfile\n")
	mux := gitproto.NewSidebandWriter(&resp)
	for i := 0; i < len(pack); i += 1000 {
		_, err := mux.Write(pack[i:min(i+1000, len(pack))])
		require.NoError(t, err)
		require.NoError(t, mux.Flush())
	}
	resp.WriteString("0000")
	return resp.Bytes()
}

// copyThroughRewriter runs the rewriter on the fetch response resp inside
// gitproto.CopyFetchResponse, with blob in place of the type-5 entry.
func copyThroughRewriter(t *testing.T, resp []byte, git io.Writer, blob []byte) error {
	t.Helper()
	return gitproto.CopyFetchResponse(git, bytes.NewReader(resp), false, func(data io.Reader, out *gitproto.SidebandWriter) error {
		rw, err := NewRewriter(out, "sha1")
		require.NoError(t, err)
		if err := rw.ReadPack(data, func(ChunkedEntry) error { return nil }); err != nil {
			return err
		}
		if _, err := rw.AppendBlob(int64(len(blob)), bytes.NewReader(blob)); err != nil {
			return err
		}
		return rw.Finish()
	})
}
