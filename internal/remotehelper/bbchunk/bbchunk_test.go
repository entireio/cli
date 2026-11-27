package bbchunk

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"io"
	"testing"
	"testing/iotest"

	"github.com/jmwoliver/xet-go/chunker"
	"github.com/jmwoliver/xet-go/xethash"
	"github.com/jmwoliver/xet-go/xorb"
	"github.com/stretchr/testify/require"
)

// xorb.Encode rejects chunks above chunker.MaxSize, and the server checks
// stored chunks against xet-go's constants.
func TestChunkerConfigMatchesXetGo(t *testing.T) {
	t.Parallel()
	require.Equal(t, chunker.Config{
		TargetSize: chunker.TargetSize,
		MinSize:    chunker.MinSize,
		MaxSize:    chunker.MaxSize,
	}, chunkerConfig())
	require.Equal(t, 8192, xorb.MaxChunks)
	require.Equal(t, 64<<20, xorb.MaxRawSize)
}

// A push reads a blob in pieces of any size. The chunks must not depend on
// them.
func TestSplitIgnoresReadSizes(t *testing.T) {
	t.Parallel()
	data := splitMix64Bytes(1_000_000, 0)
	want := splitAll(t, bytes.NewReader(data))
	for name, r := range map[string]io.Reader{
		"one byte": iotest.OneByteReader(bytes.NewReader(data)),
		"half":     iotest.HalfReader(bytes.NewReader(data)),
		"data err": iotest.DataErrReader(bytes.NewReader(data)),
	} {
		require.Equal(t, want, splitAll(t, r), name)
	}
}

func TestSplitEmpty(t *testing.T) {
	t.Parallel()
	require.Empty(t, splitAll(t, bytes.NewReader(nil)))
}

func TestSplitStopsOnEmitError(t *testing.T) {
	t.Parallel()
	stop := io.ErrClosedPipe
	err := Split(t.Context(), bytes.NewReader(splitMix64Bytes(1_000_000, 1)), func(Chunk) error { return stop })
	require.ErrorIs(t, err, stop)
}

func TestSplitChunkSizes(t *testing.T) {
	t.Parallel()
	chunks := splitAll(t, bytes.NewReader(xorshift32Bytes(3<<20, 0x9e3779b9)))
	for i, c := range chunks {
		require.LessOrEqual(t, len(c.Data), maxChunkSize, "chunk %d", i)
		if i < len(chunks)-1 {
			require.GreaterOrEqual(t, len(c.Data), minChunkSize, "chunk %d", i)
		}
		require.Equal(t, xethash.Chunk(c.Data), c.Hash, "chunk %d", i)
	}
}

// hashWithTail returns a hash whose last 8 bytes hold tail in little-endian
// order and whose other bytes are fill.
func hashWithTail(fill byte, tail uint64) xethash.Hash {
	var h xethash.Hash
	for i := range h {
		h[i] = fill
	}
	binary.LittleEndian.PutUint64(h[xethash.Size-8:], tail)
	return h
}

// TestIsAnchorRule pins the rule that entiredb's internal/xet uses. A
// different answer makes every anchor lookup of a client miss.
func TestIsAnchorRule(t *testing.T) {
	t.Parallel()
	byteAt := func(i int, v byte) xethash.Hash {
		var h xethash.Hash
		h[i] = v
		return h
	}
	tests := []struct {
		name string
		h    xethash.Hash
		want bool
	}{
		{name: "zero tail", h: hashWithTail(0xff, 0), want: true},
		{name: "tail 64", h: hashWithTail(0x00, 64), want: true},
		{name: "tail 1024", h: hashWithTail(0x5a, 1024), want: true},
		{name: "high bit only", h: hashWithTail(0x00, 1<<63), want: true},
		{name: "tail 1", h: hashWithTail(0x00, 1), want: false},
		{name: "tail 63", h: hashWithTail(0x00, 63), want: false},
		{name: "tail 65", h: hashWithTail(0x00, 65), want: false},
		{name: "top byte 64", h: byteAt(xethash.Size-1, 64), want: true},
		{name: "low byte 0x40", h: byteAt(xethash.Size-8, 0x40), want: true},
		{name: "low byte 0x41", h: byteAt(xethash.Size-8, 0x41), want: false},
		{name: "prefix bytes do not count", h: byteAt(23, 7), want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tt.want, IsAnchor(tt.h))
		})
	}
}

// TestIsAnchorChunkVectors holds the values that entiredb's
// TestIsAnchorChunkVectors holds, for real chunk hashes.
func TestIsAnchorChunkVectors(t *testing.T) {
	t.Parallel()
	var selected []int
	for i := range 4096 {
		var data [8]byte
		binary.LittleEndian.PutUint64(data[:], uint64(i))
		if IsAnchor(xethash.Chunk(data[:])) {
			selected = append(selected, i)
		}
	}
	require.Len(t, selected, 81)
	require.Equal(t, []int{6, 21, 25, 35, 37, 184, 247, 249}, selected[:8])
}

// Every chunk that Xet samples for global deduplication is an anchor.
func TestIsAnchorSuperset(t *testing.T) {
	t.Parallel()
	for range 1000 {
		var h xethash.Hash
		_, _ = rand.Read(h[:])
		binary.LittleEndian.PutUint64(h[24:], h.LastUint64()&^1023)
		require.True(t, IsAnchor(h))
	}
}

func splitAll(t *testing.T, r io.Reader) []Chunk {
	t.Helper()
	var chunks []Chunk
	require.NoError(t, Split(t.Context(), r, func(c Chunk) error {
		chunks = append(chunks, c)
		return nil
	}))
	return chunks
}
