package bbchunk_test

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/internal/remotehelper/bbchunk"
	"github.com/entireio/cli/internal/remotehelper/bbchunk/bbchunktest"
)

// TestAppendChunkedEntryLayout pins the byte layout of a stored entry.
func TestAppendChunkedEntryLayout(t *testing.T) {
	t.Parallel()
	c, size := bbchunk.SampleChunked(20)
	got, err := bbchunktest.AppendChunkedEntry([]byte{0x99}, size, c)
	require.NoError(t, err)

	var payload []byte
	payload = append(payload, bytes.Repeat([]byte{0xaa}, 20)...)
	payload = append(payload, bytes.Repeat([]byte{0xf0}, 32)...)
	payload = append(payload, 2)
	payload = append(payload, bytes.Repeat([]byte{0x01}, 32)...)
	payload = append(payload, bytes.Repeat([]byte{0x02}, 32)...)
	payload = append(payload, 3)
	payload = append(payload, 0, 0, 2, 0xc8, 0x01, 0, 0x96, 0x01)
	payload = append(payload, 1, 5, 0xc8, 0x01, 100, 0xe8, 0x07, 0xcb, 0x08)
	payload = append(payload, 0, 2, 3, 1, 0x97, 0x01, 0x9f, 0x01)
	want := []byte{0x99}
	// Type 5, size 301 = 0x12d: low nibble 0xd with the more bit, then 0x12.
	want = append(want, 0x80|5<<4|0x0d, 0x12, bbchunk.MetaChunked)
	want = binary.AppendUvarint(want, uint64(len(payload)))
	want = append(want, payload...)
	require.Equal(t, want, got)
}

func TestParseChunkedPayloadRoundTrip(t *testing.T) {
	t.Parallel()
	for _, hashSize := range []int{20, 32} {
		c, size := bbchunk.SampleChunked(hashSize)
		entry, err := bbchunktest.AppendChunkedEntry(nil, size, c)
		require.NoError(t, err)
		payload := chunkedEntryPayload(t, entry, hashSize)
		got, err := bbchunk.ParseChunkedPayload(payload, hashSize, size)
		require.NoError(t, err)
		require.Equal(t, c, got)
		payload[0] ^= 0xff
		require.Equal(t, c.OID, got.OID, "the result does not share the payload")
	}
}

// chunkedEntryPayload strips the header, meta byte and payload size from a
// stored entry.
func chunkedEntryPayload(t testing.TB, entry []byte, hashSize int) []byte {
	t.Helper()
	i := 0
	for entry[i]&0x80 != 0 {
		i++
	}
	require.Equal(t, bbchunk.MetaChunked, entry[i+1])
	n, k := binary.Uvarint(entry[i+2:])
	require.Positive(t, k)
	require.NoError(t, bbchunk.CheckChunkedFrame(bbchunk.MetaChunked, n, hashSize))
	payload := entry[i+2+k:]
	require.Len(t, payload, int(n))
	return payload
}

func TestAppendChunkedEntryRefuses(t *testing.T) {
	t.Parallel()
	c, size := bbchunk.SampleChunked(20)
	dst := []byte{1, 2}
	got, err := bbchunktest.AppendChunkedEntry(dst, size+1, c)
	require.ErrorIs(t, err, bbchunk.ErrMalformedChunked)
	require.Equal(t, dst, got)
	_, err = bbchunktest.AppendChunkedEntry(nil, size, nil)
	require.Error(t, err)
	c.OID = c.OID[:19]
	_, err = bbchunktest.AppendChunkedEntry(nil, size, c)
	require.ErrorIs(t, err, bbchunk.ErrMalformedChunked)
}

// TestFetchVectorsEncode checks that the encoder writes each valid vector
// of entiredb's fetch_vectors.json byte for byte. Fake servers build fetch
// responses with it, so a drift would test the client against wrong bytes.
func TestFetchVectorsEncode(t *testing.T) {
	t.Parallel()
	for _, e := range bbchunk.ValidFetchEntries(t) {
		encoded, err := bbchunktest.AppendChunkedEntry(nil, e.Size, e.Recipe)
		require.NoError(t, err, e.Name)
		require.Equal(t, e.Hex, hex.EncodeToString(encoded), e.Name)
	}
}

// FuzzParseChunkedPayload checks that the parser never panics, and that a
// payload it accepts encodes back to the same bytes when its uvarints are
// minimal.
func FuzzParseChunkedPayload(f *testing.F) {
	c, size := bbchunk.SampleChunked(20)
	entry, err := bbchunktest.AppendChunkedEntry(nil, size, c)
	require.NoError(f, err)
	f.Add(chunkedEntryPayload(f, entry, 20), 20, size)
	f.Add([]byte{}, 20, int64(0))
	f.Add(bytes.Repeat([]byte{0x80}, 80), 32, int64(1))
	f.Fuzz(func(t *testing.T, payload []byte, hashSize int, size int64) {
		got, err := bbchunk.ParseChunkedPayload(payload, hashSize, size)
		if err != nil {
			require.ErrorIs(t, err, bbchunk.ErrMalformedChunked)
			return
		}
		again, err := bbchunktest.AppendChunkedEntry(nil, size, got)
		require.NoError(t, err)
		reparsed, err := bbchunk.ParseChunkedPayload(chunkedEntryPayload(t, again, hashSize), hashSize, size)
		require.NoError(t, err)
		require.Equal(t, got, reparsed)
	})
}
