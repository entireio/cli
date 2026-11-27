package bbchunk

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"math/rand/v2"
	"slices"
	"testing"
	"testing/iotest"

	"github.com/jmwoliver/xet-go/xethash"
	"github.com/jmwoliver/xet-go/xorb"
	"github.com/stretchr/testify/require"
)

// builtXorb is a xorb that xorb.Builder made, with the data of each chunk.
type builtXorb struct {
	x      *Xorb
	chunks [][]byte
}

// buildXorb splits data and adds every chunk to one xorb.
func buildXorb(t *testing.T, data []byte) builtXorb {
	t.Helper()
	var b xorb.Builder
	var chunks [][]byte
	require.NoError(t, Split(t.Context(), bytes.NewReader(data), func(c Chunk) error {
		chunks = append(chunks, c.Data)
		_, err := b.AddHashed(c.Data, c.Hash, xorb.Auto)
		return err
	}))
	x, err := Seal(&b)
	require.NoError(t, err)
	return builtXorb{x: x, chunks: chunks}
}

// cloneFooter returns a deep copy of f, for a test that changes it.
func cloneFooter(f *Footer) *Footer {
	c := *f
	c.ChunkHashes = slices.Clone(f.ChunkHashes)
	c.ChunkBoundaryOffsets = slices.Clone(f.ChunkBoundaryOffsets)
	c.UnpackedChunkOffsets = slices.Clone(f.UnpackedChunkOffsets)
	return &c
}

// mixedData returns n bytes with random runs and repeated runs, so the
// chunks use both stored and compressed schemes.
func mixedData(seed uint64, n int) []byte {
	rng := rand.New(rand.NewPCG(seed, seed))
	out := make([]byte, 0, n)
	for len(out) < n {
		run := min(1+rng.IntN(40_000), n-len(out))
		if rng.IntN(2) == 0 {
			out = append(out, bytes.Repeat([]byte{byte(rng.Uint32())}, run)...)
			continue
		}
		for range run {
			out = append(out, byte(rng.Uint32()))
		}
	}
	return out
}

func TestCompleteMatchesEncodeComplete(t *testing.T) {
	t.Parallel()
	bx := buildXorb(t, mixedData(1, 600_000))
	obj, err := xorb.Decode(bx.x.Network)
	require.NoError(t, err)
	want, err := xorb.EncodeComplete(obj, [4]byte{})
	require.NoError(t, err)
	require.Equal(t, want, bx.x.Complete())
}

func TestCheckComplete(t *testing.T) {
	t.Parallel()
	bx := buildXorb(t, mixedData(2, 500_000))
	body := bx.x.Complete()
	for _, size := range []int64{int64(len(body)), -1} {
		f, err := CheckComplete(iotest.OneByteReader(bytes.NewReader(body)), bx.x.Hash, size)
		require.NoError(t, err)
		require.Equal(t, bx.x.Footer, f)
		require.Equal(t, int64(len(body)), f.Size())
	}
}

func TestCheckCompleteRefuses(t *testing.T) {
	t.Parallel()
	bx := buildXorb(t, mixedData(3, 300_000))
	other := buildXorb(t, mixedData(4, 300_000))
	body := bx.x.Complete()
	n := bx.x.Footer.Chunks()
	footerStart := len(bx.x.Network)
	// The first stored end follows the footer header (52 bytes), the chunk
	// hashes, and the bounds magic and chunk count (12 bytes).
	storedEnds := footerStart + 52 + 32*n + 12
	named := cloneFooter(other.x.Footer)
	named.XorbHash = bx.x.Hash
	edit := func(fn func(b []byte) []byte) []byte { return fn(bytes.Clone(body)) }
	cases := map[string]struct {
		body []byte
		want xethash.Hash
		size int64
	}{
		"empty":            {body: nil, want: bx.x.Hash, size: -1},
		"network form":     {body: bx.x.Network, want: bx.x.Hash, size: -1},
		"truncated footer": {body: body[:len(body)-1], want: bx.x.Hash, size: -1},
		"truncated chunk":  {body: body[:100], want: bx.x.Hash, size: -1},
		"trailing byte":    {body: append(bytes.Clone(body), 0), want: bx.x.Hash, size: -1},
		"other xorb":       {body: other.x.Complete(), want: bx.x.Hash, size: -1},
		"other xorb with a footer that names this one": {
			body: xorb.AppendFooter(slices.Clone(other.x.Network), (*xorb.Footer)(named)), want: bx.x.Hash, size: -1,
		},
		"size differs": {body: body, want: bx.x.Hash, size: int64(len(body)) + 1},
		"size short":   {body: body, want: bx.x.Hash, size: int64(len(body)) - 1},
		"flipped payload": {body: edit(func(b []byte) []byte {
			b[xorb.HeaderSize+10] ^= 1
			return b
		}), want: bx.x.Hash, size: -1},
		"nonce set": {body: edit(func(b []byte) []byte {
			b[len(b)-20] = 1
			return b
		}), want: bx.x.Hash, size: -1},
		"reserved byte set": {body: edit(func(b []byte) []byte {
			b[len(b)-10] = 1
			return b
		}), want: bx.x.Hash, size: -1},
		"footer names other xorb": {body: edit(func(b []byte) []byte {
			b[footerStart+8] ^= 1
			return b
		}), want: bx.x.Hash, size: -1},
		"footer stored end": {body: edit(func(b []byte) []byte {
			binary.LittleEndian.PutUint32(b[storedEnds:], binary.LittleEndian.Uint32(b[storedEnds:])+1)
			return b
		}), want: bx.x.Hash, size: -1},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := CheckComplete(bytes.NewReader(tc.body), tc.want, tc.size)
			require.ErrorIs(t, err, ErrMalformedXorb)
		})
	}
}

// TestCheckCompleteLargeXorbByteByByte reads a xorb longer than twice the
// largest footer in one-byte reads, so tailReader trims its tail many
// times. The canonical-footer check must still see the footer.
func TestCheckCompleteLargeXorbByteByByte(t *testing.T) {
	t.Parallel()
	bx := buildXorb(t, mixedData(11, 2_000_000))
	body := bx.x.Complete()
	require.Greater(t, len(body), 2*maxFooterSize+100_000)
	f, err := CheckComplete(iotest.OneByteReader(bytes.NewReader(body)), bx.x.Hash, int64(len(body)))
	require.NoError(t, err)
	require.Equal(t, bx.x.Footer, f)
	for name, pos := range map[string]int{"nonce set": len(body) - 20, "reserved byte set": len(body) - 10} {
		bad := bytes.Clone(body)
		bad[pos] = 1
		_, err := CheckComplete(iotest.OneByteReader(bytes.NewReader(bad)), bx.x.Hash, int64(len(bad)))
		require.ErrorIs(t, err, ErrMalformedXorb, name)
	}
}

func TestCheckCompleteKeepsReadErrors(t *testing.T) {
	t.Parallel()
	bx := buildXorb(t, mixedData(5, 200_000))
	want := errors.New("connection reset")
	r := io.MultiReader(bytes.NewReader(bx.x.Complete()[:5000]), iotest.ErrReader(want))
	_, err := CheckComplete(r, bx.x.Hash, -1)
	require.ErrorIs(t, err, want)
	require.NotErrorIs(t, err, ErrMalformedXorb, "a failed read says nothing about the xorb")
}

func TestReadFooter(t *testing.T) {
	t.Parallel()
	bx := buildXorb(t, mixedData(6, 400_000))
	body := bx.x.Complete()
	size := int64(len(body))
	f, err := ReadFooter(bytes.NewReader(body), size, bx.x.Hash)
	require.NoError(t, err)
	require.Equal(t, bx.x.Footer, f)

	other := buildXorb(t, mixedData(7, 100_000))
	_, err = ReadFooter(bytes.NewReader(body), size, other.x.Hash)
	require.ErrorIs(t, err, ErrMalformedXorb, "footer of another xorb")
	_, err = ReadFooter(bytes.NewReader(body[1:]), size-1, bx.x.Hash)
	require.ErrorIs(t, err, ErrMalformedXorb, "chunks must end at the footer")
	nonce := bytes.Clone(body)
	nonce[len(nonce)-20] = 1
	_, err = ReadFooter(bytes.NewReader(nonce), size, bx.x.Hash)
	require.ErrorIs(t, err, ErrMalformedXorb, "nonce set")
	reserved := bytes.Clone(body)
	reserved[len(reserved)-10] = 1
	_, err = ReadFooter(bytes.NewReader(reserved), size, bx.x.Hash)
	require.ErrorIs(t, err, ErrMalformedXorb, "reserved byte set")
	disk := errors.New("disk failure")
	_, err = ReadFooter(errReaderAt{disk}, size, bx.x.Hash)
	require.ErrorIs(t, err, disk)
	require.NotErrorIs(t, err, ErrMalformedXorb, "a failed read says nothing about the xorb")
}

type errReaderAt struct{ err error }

func (e errReaderAt) ReadAt([]byte, int64) (int, error) { return 0, e.err }

// shortReaderAt serves the first reads of r and then returns one byte short
// and no error, as a file that shrank after its size was taken can.
type shortReaderAt struct {
	r    io.ReaderAt
	full int
}

func (s *shortReaderAt) ReadAt(p []byte, off int64) (int, error) {
	if s.full == 0 {
		n, _ := s.r.ReadAt(p[:len(p)-1], off) //nolint:errcheck // the short read is the point
		return n, nil
	}
	s.full--
	return s.r.ReadAt(p, off)
}

// TestReadFooterShortRead covers a short read of the footer bytes that
// ReadFooter reads after xorb.ReadFooter. It is a read error, not a bad
// xorb.
func TestReadFooterShortRead(t *testing.T) {
	t.Parallel()
	bx := buildXorb(t, mixedData(12, 200_000))
	body := bx.x.Complete()
	// xorb.ReadFooter reads the trailer and then the footer.
	ra := &shortReaderAt{r: bytes.NewReader(body), full: 2}
	_, err := ReadFooter(ra, int64(len(body)), bx.x.Hash)
	require.ErrorIs(t, err, io.ErrUnexpectedEOF)
	require.NotErrorIs(t, err, ErrMalformedXorb)
}

func TestTermReaderReturnsTheChunks(t *testing.T) {
	t.Parallel()
	bx := buildXorb(t, mixedData(8, 1_500_000))
	n := len(bx.chunks)
	require.Greater(t, n, 8)
	body := bx.x.Complete()
	f, err := CheckComplete(bytes.NewReader(body), bx.x.Hash, int64(len(body)))
	require.NoError(t, err)
	for _, r := range [][2]int{{0, n}, {0, 1}, {n - 1, n}, {2, 5}, {1, n - 1}} {
		tr, err := NewTermReader(bytes.NewReader(body), f, r[0], r[1])
		require.NoError(t, err)
		got, err := io.ReadAll(iotest.HalfReader(tr))
		require.NoError(t, err)
		require.Equal(t, bytes.Join(bx.chunks[r[0]:r[1]], nil), got, "chunks [%d, %d)", r[0], r[1])
		size, err := f.UnpackedSize(r[0], r[1])
		require.NoError(t, err)
		require.Equal(t, uint64(len(got)), size)
		first, last, err := f.StoredRange(r[0], r[1])
		require.NoError(t, err)
		start := uint64(0)
		if r[0] > 0 {
			start = uint64(f.ChunkBoundaryOffsets[r[0]-1])
		}
		require.Equal(t, start, first)
		require.Equal(t, uint64(f.ChunkBoundaryOffsets[r[1]-1]-1), last)
	}
}

func TestTermReaderRefusesBadRange(t *testing.T) {
	t.Parallel()
	bx := buildXorb(t, mixedData(9, 100_000))
	f := bx.x.Footer
	n := len(bx.chunks)
	for _, r := range [][2]int{{-1, 1}, {1, 1}, {2, 1}, {0, n + 1}} {
		_, err := NewTermReader(bytes.NewReader(bx.x.Complete()), f, r[0], r[1])
		require.Error(t, err, "chunks [%d, %d)", r[0], r[1])
		require.NotErrorIs(t, err, ErrCorruptXorb)
	}
}

func TestTermReaderDetectsCorruption(t *testing.T) {
	t.Parallel()
	bx := buildXorb(t, mixedData(10, 400_000))
	second := int(bx.x.Footer.ChunkBoundaryOffsets[0])
	cases := map[string]func(b []byte, f *Footer){
		"payload byte": func(b []byte, _ *Footer) { b[second+xorb.HeaderSize+3] ^= 0x40 },
		"header length": func(b []byte, _ *Footer) {
			b[second+1]++
		},
		"header version": func(b []byte, _ *Footer) { b[second] = 1 },
		"stored end":     func(_ []byte, f *Footer) { f.ChunkBoundaryOffsets[1]++ },
	}
	for name, corrupt := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			body := bx.x.Complete()
			f := cloneFooter(bx.x.Footer)
			corrupt(body, f)
			tr, err := NewTermReader(bytes.NewReader(body), f, 0, len(bx.chunks))
			require.NoError(t, err)
			got, err := io.ReadAll(tr)
			require.ErrorIs(t, err, ErrCorruptXorb)
			require.Equal(t, bx.chunks[0], got, "the chunk before the damage reads")
			_, err = tr.Read(make([]byte, 10))
			require.ErrorIs(t, err, ErrCorruptXorb, "the error sticks")
		})
	}
}
