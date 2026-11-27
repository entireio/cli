package bbchunk

import (
	"bytes"
	"encoding/binary"
	"testing"

	"github.com/jmwoliver/xet-go/xethash"
	"github.com/jmwoliver/xet-go/xorb"
	"github.com/stretchr/testify/require"
)

func sampleChunked(hashSize int) (*ChunkedPayload, int64) {
	return &ChunkedPayload{
		OID:      bytes.Repeat([]byte{0xaa}, hashSize),
		FileHash: filledHash(0xf0),
		Xorbs:    []xethash.Hash{filledHash(0x01), filledHash(0x02)},
		Terms: []ChunkedTerm{
			{Xorb: 0, ChunkStart: 0, ChunkEnd: 2, UnpackedBytes: 200, FirstByte: 0, LastByte: 150},
			{Xorb: 1, ChunkStart: 5, ChunkEnd: 200, UnpackedBytes: 100, FirstByte: 1000, LastByte: 1099},
			{Xorb: 0, ChunkStart: 2, ChunkEnd: 3, UnpackedBytes: 1, FirstByte: 151, LastByte: 159},
		},
	}, 301
}

// chunkedPayloadBytes encodes the payload of c without checks, so a test can
// build payloads that the encoder refuses.
func chunkedPayloadBytes(c *ChunkedPayload, terms [][6]uint64) []byte {
	b := append([]byte(nil), c.OID...)
	b = append(b, c.FileHash[:]...)
	b = binary.AppendUvarint(b, uint64(len(c.Xorbs)))
	for _, h := range c.Xorbs {
		b = append(b, h[:]...)
	}
	b = binary.AppendUvarint(b, uint64(len(terms)))
	for _, t := range terms {
		for _, v := range t {
			b = binary.AppendUvarint(b, v)
		}
	}
	return b
}

func TestParseChunkedPayloadRefuses(t *testing.T) {
	t.Parallel()
	base, _ := sampleChunked(20)
	oneXorb := &ChunkedPayload{OID: base.OID, FileHash: base.FileHash, Xorbs: base.Xorbs[:1]}
	term := [6]uint64{0, 0, 1, 10, 0, 9}
	valid := chunkedPayloadBytes(oneXorb, [][6]uint64{term})
	with := func(f func(*[6]uint64)) []byte {
		tm := term
		f(&tm)
		return chunkedPayloadBytes(oneXorb, [][6]uint64{tm})
	}
	_, err := ParseChunkedPayload(valid, 20, 10)
	require.NoError(t, err)

	tests := []struct {
		name     string
		payload  []byte
		hashSize int
		size     int64
		want     string
	}{
		{"hash size", valid, 24, 10, "unsupported hash size"},
		{"short for the OID", valid[:19], 20, 10, "payload of 19 bytes is shorter"},
		{"over the payload limit", make([]byte, MaxPayloadSize+1), 20, 10, "is over 1048576"},
		{"negative size", valid, 20, -1, "negative size"},
		{"truncated file hash", valid[:20+31], 20, 10, "truncated file hash"},
		{"no xorb count", valid[:20+32], 20, 10, "invalid xorb count"},
		{"no xorbs", chunkedPayloadBytes(&ChunkedPayload{OID: base.OID}, [][6]uint64{term}), 20, 10, "no xorbs"},
		{"xorbs past the end", valid[:20+32+1+31], 20, 10, "do not fit"},
		{"duplicate xorb", chunkedPayloadBytes(&ChunkedPayload{OID: base.OID, Xorbs: []xethash.Hash{filledHash(1), filledHash(1)}}, [][6]uint64{term}), 20, 10, "more than once"},
		{"no terms", chunkedPayloadBytes(oneXorb, nil), 20, 10, "no terms"},
		{"terms past the end", valid[:len(valid)-1], 20, 10, "do not fit"},
		{"truncated term", bytes.Join([][]byte{valid[:len(valid)-1], {0x80}}, nil), 20, 10, "truncated or invalid"},
		{"xorb index", with(func(t *[6]uint64) { t[0] = 1 }), 20, 10, "xorb index 1"},
		{"empty range", with(func(t *[6]uint64) { t[1], t[2] = 3, 3 }), 20, 10, "is empty"},
		{"chunk end", with(func(t *[6]uint64) { t[2] = xorb.MaxChunks + 1 }), 20, 10, "chunk end"},
		{"zero unpacked", with(func(t *[6]uint64) { t[3] = 0 }), 20, 0, "unpacked bytes is 0"},
		{"unpacked over a xorb", with(func(t *[6]uint64) { t[3] = xorb.MaxRawSize + 1 }), 20, xorb.MaxRawSize + 1, "over the"},
		{"byte range reversed", with(func(t *[6]uint64) { t[4], t[5] = 5, 4 }), 20, 10, "after last byte"},
		{"last byte", with(func(t *[6]uint64) { t[5] = xorb.MaxNetworkSize }), 20, 10, "past the"},
		{"over the size", valid, 20, 9, "more than the logical size"},
		{"under the size", valid, 20, 11, "unpack to 10 bytes"},
		{"trailing byte", bytes.Join([][]byte{valid, {0}}, nil), 20, 10, "after the last term"},
		{"unused xorb", chunkedPayloadBytes(&ChunkedPayload{OID: base.OID, Xorbs: base.Xorbs}, [][6]uint64{term}), 20, 10, "no term uses xorb 1"},
		{"overlong uvarint", bytes.Join([][]byte{valid[:20+32], bytes.Repeat([]byte{0xff}, 11)}, nil), 20, 10, "invalid xorb count"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := ParseChunkedPayload(tc.payload, tc.hashSize, tc.size)
			require.ErrorIs(t, err, ErrMalformedChunked)
			require.ErrorContains(t, err, tc.want)
		})
	}
}

func TestCheckChunkedFrame(t *testing.T) {
	t.Parallel()
	require.NoError(t, CheckChunkedFrame(MetaChunked, 20, 20))
	require.NoError(t, CheckChunkedFrame(MetaChunked, MaxPayloadSize, 32))
	for _, meta := range []byte{0, 0x01, MetaClientChunked, 0x09, 0xff} {
		require.ErrorIs(t, CheckChunkedFrame(meta, 100, 20), ErrMalformedChunked, "meta %#x", meta)
	}
	require.ErrorIs(t, CheckChunkedFrame(MetaChunked, 31, 32), ErrMalformedChunked)
	require.ErrorIs(t, CheckChunkedFrame(MetaChunked, MaxPayloadSize+1, 20), ErrMalformedChunked)
	require.ErrorIs(t, CheckChunkedFrame(MetaChunked, 100, 0), ErrMalformedChunked)
}
