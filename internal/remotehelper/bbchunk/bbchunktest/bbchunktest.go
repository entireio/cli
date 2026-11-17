// Package bbchunktest encodes the stored chunked entries that entiredb sends
// in a fetch. A client never sends this kind; tests and fake servers use it
// to build fetch responses.
package bbchunktest

import (
	"encoding/binary"
	"errors"
	"slices"

	"github.com/entireio/cli/internal/remotehelper/bbchunk"
)

// AppendChunkedEntry appends the type-5 pack entry with meta
// bbchunk.MetaChunked, the logical size size and the recipe c to dst. It
// refuses a recipe that bbchunk.ParseChunkedPayload refuses.
func AppendChunkedEntry(dst []byte, size int64, c *bbchunk.ChunkedPayload) ([]byte, error) {
	if c == nil {
		return dst, errors.New("bbchunktest: nil chunked payload")
	}
	payload := slices.Clone(c.OID)
	payload = append(payload, c.FileHash[:]...)
	payload = binary.AppendUvarint(payload, uint64(len(c.Xorbs)))
	for _, h := range c.Xorbs {
		payload = append(payload, h[:]...)
	}
	payload = binary.AppendUvarint(payload, uint64(len(c.Terms)))
	for _, t := range c.Terms {
		payload = binary.AppendUvarint(payload, uint64(t.Xorb))
		payload = binary.AppendUvarint(payload, uint64(t.ChunkStart))
		payload = binary.AppendUvarint(payload, uint64(t.ChunkEnd))
		payload = binary.AppendUvarint(payload, t.UnpackedBytes)
		payload = binary.AppendUvarint(payload, t.FirstByte)
		payload = binary.AppendUvarint(payload, t.LastByte)
	}
	if _, err := bbchunk.ParseChunkedPayload(payload, len(c.OID), size); err != nil {
		return dst, err //nolint:wrapcheck // the parser names the fault
	}
	dst = bbchunk.AppendObjectHeader(dst, bbchunk.ExternalObject, size)
	dst = append(dst, bbchunk.MetaChunked)
	dst = binary.AppendUvarint(dst, uint64(len(payload)))
	return append(dst, payload...), nil
}
