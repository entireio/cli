package bbpush

import (
	"bytes"
	"math/rand/v2"
	"testing"

	"github.com/jmwoliver/xet-go/xethash"
	"github.com/jmwoliver/xet-go/xorb"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/internal/remotehelper/bbchunk"
)

func randomData(n int, seed uint64) []byte {
	rng := rand.New(rand.NewPCG(seed, seed+1))
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(rng.Uint32())
	}
	return b
}

func splitData(t *testing.T, data []byte) []bbchunk.Chunk {
	t.Helper()
	var chunks []bbchunk.Chunk
	require.NoError(t, bbchunk.Split(t.Context(), bytes.NewReader(data), func(c bbchunk.Chunk) error {
		chunks = append(chunks, c)
		return nil
	}))
	return chunks
}

func chunkList(chunks []bbchunk.Chunk) *blobChunks {
	c := &blobChunks{}
	for _, ch := range chunks {
		c.hashes = append(c.hashes, ch.Hash)
		c.sizes = append(c.sizes, uint32(len(ch.Data)))
	}
	return c
}

// buildFromChunks runs the second read of a blob over chunks, and returns
// the recipe and the new xorbs.
func buildFromChunks(t *testing.T, ix *chunkIndex, chunks []bbchunk.Chunk) (*bbchunk.Recipe, []*bbchunk.Xorb) {
	t.Helper()
	var sealed []*bbchunk.Xorb
	b := &blobBuilder{ix: ix, chunks: chunkList(chunks), open: -1, sealed: func(x *bbchunk.Xorb) error {
		sealed = append(sealed, x)
		return nil
	}}
	var size int64
	for _, c := range chunks {
		require.NoError(t, b.add(c))
		size += int64(len(c.Data))
	}
	require.NoError(t, b.sealOpen())
	r, err := b.recipe(size)
	require.NoError(t, err)
	nodes := make([]xethash.Node, len(chunks))
	for i, c := range chunks {
		nodes[i] = xethash.Node{Hash: c.Hash, Size: uint64(len(c.Data))}
	}
	require.Equal(t, xethash.File(nodes), r.FileHash, "the file hash covers every chunk in order")
	_, err = bbchunk.AppendEntry(nil, make([]byte, 20), r)
	require.NoError(t, err, "the recipe passes the entry checks")
	return r, sealed
}

// rebuildBlob returns the bytes of r from the chunk data of every xorb.
func rebuildBlob(t *testing.T, r *bbchunk.Recipe, xorbs map[xethash.Hash][][]byte) []byte {
	t.Helper()
	var out []byte
	for _, term := range r.Terms {
		x, ok := xorbs[r.Xorbs[term.Xorb]]
		require.True(t, ok, "xorb %s", r.Xorbs[term.Xorb])
		for _, c := range x[term.ChunkStart:term.ChunkEnd] {
			out = append(out, c...)
		}
	}
	return out
}

func addSealed(t *testing.T, all map[xethash.Hash][][]byte, xs []*bbchunk.Xorb) {
	t.Helper()
	for _, x := range xs {
		obj, err := xorb.Decode(x.Network)
		require.NoError(t, err)
		var chunks [][]byte
		for _, c := range obj.Chunks {
			chunks = append(chunks, c.Data)
		}
		all[x.Hash] = chunks
	}
}

func TestBlobBuilderTakesLongRunsFromKnownXorbs(t *testing.T) {
	t.Parallel()
	data := randomData(2<<20, 1)
	chunks := splitData(t, data)
	require.Greater(t, len(chunks), 12)

	stored := xethash.Hash{1}
	ix := newChunkIndex()
	var storedHashes []xethash.Hash
	all := map[xethash.Hash][][]byte{stored: {}}
	for _, c := range chunks[2:8] {
		storedHashes = append(storedHashes, c.Hash)
		all[stored] = append(all[stored], c.Data)
	}
	ix.addStored(stored, storedHashes)
	short := xethash.Hash{2}
	ix.addStored(short, []xethash.Hash{chunks[10].Hash, chunks[11].Hash})
	all[short] = [][]byte{chunks[10].Data, chunks[11].Data}

	r, sealed := buildFromChunks(t, ix, chunks)
	require.Len(t, sealed, 1, "the new chunks of a blob go into one xorb")
	addSealed(t, all, sealed)
	newX := sealed[0].Hash
	require.Equal(t, []xethash.Hash{newX, stored}, r.Xorbs)
	require.Equal(t, bbchunk.Term{Xorb: 0, ChunkStart: 0, ChunkEnd: 2, UnpackedBytes: r.Terms[0].UnpackedBytes}, r.Terms[0])
	require.Equal(t, 1, r.Terms[1].Xorb)
	require.Equal(t, [2]int{0, 6}, [2]int{r.Terms[1].ChunkStart, r.Terms[1].ChunkEnd}, "a run of 6 known chunks")
	require.Len(t, r.Terms, 3, "a run of 2 known chunks is new data")
	require.Len(t, sealed[0].Footer.ChunkHashes, len(chunks)-6)
	require.Equal(t, data, rebuildBlob(t, r, all))
	require.Equal(t, int64(len(data)), r.Size)
}

func TestBlobBuilderTakesAShortRunAtTheEnd(t *testing.T) {
	t.Parallel()
	data := randomData(1<<20, 2)
	chunks := splitData(t, data)
	n := len(chunks)
	tail := xethash.Hash{3}
	ix := newChunkIndex()
	ix.addStored(tail, []xethash.Hash{chunks[n-2].Hash, chunks[n-1].Hash})

	r, sealed := buildFromChunks(t, ix, chunks)
	all := map[xethash.Hash][][]byte{tail: {chunks[n-2].Data, chunks[n-1].Data}}
	addSealed(t, all, sealed)
	require.Equal(t, tail, r.Xorbs[r.Terms[len(r.Terms)-1].Xorb], "a run that ends the blob counts")
	require.Equal(t, data, rebuildBlob(t, r, all))
}

// Repeated content refers back to the open xorb of the same blob.
func TestBlobBuilderReusesItsOwnChunks(t *testing.T) {
	t.Parallel()
	data := bytes.Repeat([]byte{0}, 3<<20)
	chunks := splitData(t, data)
	r, sealed := buildFromChunks(t, newChunkIndex(), chunks)
	require.Len(t, sealed, 1)
	require.Len(t, sealed[0].Footer.ChunkHashes, minRun, "zeros need minRun chunks of new data")
	all := map[xethash.Hash][][]byte{}
	addSealed(t, all, sealed)
	require.Equal(t, data, rebuildBlob(t, r, all))
}

// A later blob of the push refers to the xorbs of an earlier one.
func TestBlobBuilderDedupsAcrossBlobs(t *testing.T) {
	t.Parallel()
	a := randomData(1<<20, 3)
	b := bytes.Clone(a)
	copy(b[1<<19:], "an edit")
	ix := newChunkIndex()
	ra, sa := buildFromChunks(t, ix, splitData(t, a))
	rb, sb := buildFromChunks(t, ix, splitData(t, b))
	all := map[xethash.Hash][][]byte{}
	addSealed(t, all, append(sa, sb...))
	require.Equal(t, a, rebuildBlob(t, ra, all))
	require.Equal(t, b, rebuildBlob(t, rb, all))
	require.Less(t, len(sb[0].Footer.ChunkHashes), len(sa[0].Footer.ChunkHashes)/2)
	require.Contains(t, rb.Xorbs, sa[0].Hash)
}

func TestBlobBuilderSealsAFullXorb(t *testing.T) {
	t.Parallel()
	// Random 128 KiB pieces fill the 64 MiB of a xorb with few chunks.
	data := randomData(65<<20+5, 4)
	chunks := splitData(t, data)
	r, sealed := buildFromChunks(t, newChunkIndex(), chunks)
	require.Len(t, sealed, 2)
	all := map[xethash.Hash][][]byte{}
	addSealed(t, all, sealed)
	require.Equal(t, data, rebuildBlob(t, r, all))
}

func TestBlobBuilderRefusesAChangedBlob(t *testing.T) {
	t.Parallel()
	chunks := splitData(t, randomData(300<<10, 5))
	b := &blobBuilder{ix: newChunkIndex(), chunks: chunkList(chunks), open: -1}
	other := splitData(t, randomData(300<<10, 6))
	require.ErrorContains(t, b.add(other[0]), "changed")
}
