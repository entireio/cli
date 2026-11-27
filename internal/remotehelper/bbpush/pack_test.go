package bbpush

import (
	"bytes"
	"crypto/sha1"
	"encoding/binary"
	"io"
	"testing"
	"testing/iotest"

	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/internal/remotehelper/bbchunk"
	"github.com/entireio/cli/internal/remotehelper/gitproto"
)

func TestTailReader(t *testing.T) {
	t.Parallel()
	data := randomData(200_000, 7)
	for name, r := range map[string]io.Reader{
		"whole":    bytes.NewReader(data),
		"one byte": iotest.OneByteReader(bytes.NewReader(data)),
		"half":     iotest.HalfReader(bytes.NewReader(data)),
	} {
		tr := &tailReader{r: r, n: 20}
		got, err := io.ReadAll(tr)
		require.NoError(t, err, name)
		require.Equal(t, data[:len(data)-20], got, name)
		require.Equal(t, data[len(data)-20:], tr.held, name)
	}
	_, err := io.ReadAll(&tailReader{r: bytes.NewReader(data[:10]), n: 20})
	require.ErrorIs(t, err, io.ErrUnexpectedEOF)
}

func TestWritePack(t *testing.T) {
	t.Parallel()
	objects := randomData(5000, 8)
	var src bytes.Buffer
	src.WriteString("PACK")
	src.Write(binary.BigEndian.AppendUint32(nil, 2))
	src.Write(binary.BigEndian.AppendUint32(nil, 3))
	src.Write(objects)
	sum := sha1.Sum(src.Bytes())
	src.Write(sum[:])
	entries := [][]byte{[]byte("entry one"), []byte("entry two")}

	body := &gitproto.RequestBody{}
	body.Append([]byte("commands"))
	require.NoError(t, writePack(body, &src, packSpec{entries: entries, newHash: newPackHash("sha1")}))
	got, err := io.ReadAll(body.Reader())
	require.NoError(t, err)

	want := bytes.NewBufferString("PACK")
	want.Write(binary.BigEndian.AppendUint32(nil, bbchunk.PackVersionExternal))
	want.Write(binary.BigEndian.AppendUint32(nil, 5))
	want.Write(objects)
	want.WriteString("entry oneentry two")
	newSum := sha1.Sum(want.Bytes())
	want.Write(newSum[:])
	require.Equal(t, append([]byte("commands"), want.Bytes()...), got)

	err = writePack(&gitproto.RequestBody{}, bytes.NewReader([]byte("PACK\x00\x00\x00\x03\x00\x00\x00\x00")), packSpec{newHash: newPackHash("sha1")})
	require.ErrorContains(t, err, "header")
}
