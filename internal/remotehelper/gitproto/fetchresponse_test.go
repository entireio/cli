package gitproto

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/go-git/go-git/v6/plumbing/format/pktline"
	"github.com/stretchr/testify/require"
)

func pkt(s string) string { return fmt.Sprintf("%04x%s", len(s)+4, s) }

const (
	flushPkt = "0000"
	delimPkt = "0001"
)

func copyPack(data io.Reader, out *SidebandWriter) error {
	_, err := io.Copy(out, data)
	return err
}

func copyResponse(t *testing.T, in string, sidebandAll bool) (string, error) {
	t.Helper()
	var out bytes.Buffer
	err := CopyFetchResponse(&out, strings.NewReader(in), sidebandAll, copyPack)
	return out.String(), err
}

// fullResponse has every section. band prefixes each line outside the
// packfile section: "" for the plain framing, "\x01" for sideband-all.
func fullResponse(band string) string {
	return pkt(band+"acknowledgments\n") + pkt(band+"ACK 1111111111111111111111111111111111111111\n") + pkt(band+"ready\n") + delimPkt +
		pkt(band+"shallow-info\n") + pkt(band+"shallow 2222222222222222222222222222222222222222\n") + delimPkt +
		pkt(band+"wanted-refs\n") + pkt(band+"3333333333333333333333333333333333333333 refs/heads/main\n") + delimPkt +
		pkt(band+"packfile-uris\n") + pkt(band+"4444444444444444444444444444444444444444 https://example.com/p\n") + delimPkt +
		pkt(band+"packfile\n") + pkt("\x01PACK") + pkt("\x02Counting objects: 1\r") + pkt("\x01rest of the pack") + pkt("\x02done.\n") + flushPkt
}

func TestCopyFetchResponsePassesThrough(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		in          string
		sidebandAll bool
	}{
		"negotiation round":  {pkt("acknowledgments\n") + pkt("NAK\n") + flushPkt, false},
		"packfile only":      {pkt("packfile\n") + pkt("\x01PACK") + flushPkt, false},
		"headers without LF": {pkt("acknowledgments") + pkt("ready") + delimPkt + pkt("packfile") + pkt("\x01PACK") + flushPkt, false},
		"all sections":       {fullResponse(""), false},
		"sideband-all":       {fullResponse("\x01"), true},
		"sideband-all progress between sections": {
			pkt("\x02Enumerating\n") + pkt("\x01acknowledgments\n") + pkt("\x02x\n") + pkt("\x01ready\n") + delimPkt + pkt("\x02y\n") +
				pkt("\x01packfile\n") + pkt("\x01PACK") + flushPkt, true,
		},
		"empty band-1 packets": {pkt("packfile\n") + pkt("\x01") + pkt("\x01PACK") + pkt("\x01") + flushPkt, false},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got, err := copyResponse(t, tc.in, tc.sidebandAll)
			require.NoError(t, err)
			want := strings.ReplaceAll(tc.in, pkt("\x01"), "")
			require.Equal(t, want, got)
		})
	}
}

func TestCopyFetchResponseRewritesPack(t *testing.T) {
	t.Parallel()
	in := pkt("packfile\n") + pkt("\x01abc") + pkt("\x02p\n") + pkt("\x01def") + flushPkt
	var out bytes.Buffer
	err := CopyFetchResponse(&out, iotest.OneByteReader(strings.NewReader(in)), false, func(data io.Reader, w *SidebandWriter) error {
		b, err := io.ReadAll(data)
		require.NoError(t, err)
		require.Equal(t, "abcdef", string(b))
		_, err = w.Write(bytes.ToUpper(b))
		require.NoError(t, err)
		return w.Progress([]byte("rebuilt\n"))
	})
	require.NoError(t, err)
	require.Equal(t, pkt("packfile\n")+pkt("\x02p\n")+pkt("\x01ABCDEF")+pkt("\x02rebuilt\n")+flushPkt, out.String())
}

func TestCopyFetchResponseSanitizes(t *testing.T) {
	t.Parallel()
	in := pkt("packfile\n") + pkt("\x02a\x1b]0;x\x07 \x1b[1mb\x1b[m\r\n") + flushPkt
	got, err := copyResponse(t, in, false)
	require.NoError(t, err)
	require.Equal(t, pkt("packfile\n")+pkt("\x02a^[]0;x^G \x1b[1mb\x1b[m\r\n")+flushPkt, got)

	in = pkt("ERR bad\x1b[2Jrepo\n")
	got, err = copyResponse(t, in, false)
	var remote *RemoteError
	require.ErrorAs(t, err, &remote)
	require.Equal(t, "bad^[[2Jrepo", remote.Message)
	require.Equal(t, pkt("ERR bad^[[2Jrepo\n"), got)
}

func TestCopyFetchResponseRemoteErrors(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		in, want    string
		sidebandAll bool
	}{
		"ERR first":           {pkt("ERR no such repo\n"), pkt("ERR no such repo\n"), false},
		"ERR after a section": {pkt("acknowledgments\n") + pkt("ERR boom"), pkt("acknowledgments\n") + pkt("ERR boom"), false},
		"band 3 in the pack":  {pkt("packfile\n") + pkt("\x01PACK") + pkt("\x03boom\n") + flushPkt, pkt("packfile\n") + pkt("\x01PACK") + pkt("\x03boom\n"), false},
		"band 3 sideband-all": {pkt("\x01acknowledgments\n") + pkt("\x03boom\n") + flushPkt, pkt("\x01acknowledgments\n") + pkt("\x03boom\n"), true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got, err := copyResponse(t, tc.in, tc.sidebandAll)
			var remote *RemoteError
			require.ErrorAs(t, err, &remote)
			require.Contains(t, []string{"no such repo", "boom"}, remote.Message)
			require.Equal(t, tc.want, got)
		})
	}
}

func TestCopyFetchResponseRefuses(t *testing.T) {
	t.Parallel()
	pack := pkt("packfile\n") + pkt("\x01PACK") + flushPkt
	tests := []struct {
		name, in, want string
		sidebandAll    bool
	}{
		{"empty", "", "unexpected EOF", false},
		{"flush only", flushPkt, "unexpected flush", false},
		{"unknown section", pkt("bundles\n") + flushPkt, "expected a section header", false},
		{"sections out of order", pkt("packfile-uris\n") + delimPkt + pkt("shallow-info\n") + delimPkt + pack, "expected a section header", false},
		{"section twice", pkt("shallow-info\n") + delimPkt + pkt("shallow-info\n"), "expected a section header", false},
		{"flush after shallow-info", pkt("shallow-info\n") + flushPkt, "unexpected flush", false},
		{"delim in the packfile section", pkt("packfile\n") + delimPkt, "unexpected delim", false},
		{"delim before a section", delimPkt, "unexpected delim", false},
		{"response-end", pkt("acknowledgments\n") + "0002", "response-end", false},
		{"missing band", pkt("packfile\n") + "0004", "missing side-band designator", false},
		{"bad band", pkt("packfile\n") + pkt("\x04x"), "bad side-band 4", false},
		{"plain line under sideband-all", pkt("acknowledgments\n"), "bad side-band", true},
		{"truncated packet", pkt("packfile\n") + "0010\x01PA", "unexpected EOF", false},
		{"truncated length", pkt("packfile\n") + "00", "unexpected EOF", false},
		{"no final flush", pkt("packfile\n") + pkt("\x01PACK"), "unexpected EOF", false},
		{"bad length", "zzzz", "invalid pkt-len", false},
		{"length 3", "0003", "invalid pkt-len", false},
		{"too long", "fff5", "invalid pkt-len", false},
		{"data after the final flush", pack + "0", "after the final flush", false},
		{"second response", pack + pack, "after the final flush", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := copyResponse(t, tc.in, tc.sidebandAll)
			require.ErrorContains(t, err, tc.want)
		})
	}
}

func TestCopyFetchResponsePackCallback(t *testing.T) {
	t.Parallel()
	in := pkt("packfile\n") + pkt("\x01PACK") + pkt("\x01more") + flushPkt

	var out bytes.Buffer
	err := CopyFetchResponse(&out, strings.NewReader(in), false, func(data io.Reader, _ *SidebandWriter) error {
		_, err := io.ReadFull(data, make([]byte, 4))
		return err
	})
	require.ErrorContains(t, err, "pack data after the end of the pack")
	require.False(t, strings.HasSuffix(out.String(), flushPkt))

	boom := errors.New("boom")
	out.Reset()
	err = CopyFetchResponse(&out, strings.NewReader(in), false, func(io.Reader, *SidebandWriter) error { return boom })
	require.ErrorIs(t, err, boom)
	require.Equal(t, pkt("packfile\n"), out.String(), "no flush after a failure")
}

// TestCopyFetchResponseLargePack checks the remux of a pack larger than one
// packet: no packet is over pktline.MaxSize, and the data is unchanged.
func TestCopyFetchResponseLargePack(t *testing.T) {
	t.Parallel()
	data := bytes.Repeat([]byte("0123456789abcdef"), 20_000)
	var in bytes.Buffer
	in.WriteString(pkt("packfile\n"))
	for rest := data; len(rest) > 0; {
		n := min(len(rest), 1000+len(rest)%(pktline.MaxPayloadSize-1001))
		in.WriteString(pkt("\x01" + string(rest[:n])))
		rest = rest[n:]
	}
	in.WriteString(flushPkt)
	got, err := copyResponse(t, in.String(), false)
	require.NoError(t, err)

	f := NewFetchResponseReader(strings.NewReader(got), false)
	var pack []byte
	for {
		p, err := f.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		require.NoError(t, err)
		require.LessOrEqual(t, len(p.Wire), pktline.MaxSize)
		if p.Kind == FetchPackData {
			pack = append(pack, p.Payload...)
		}
	}
	require.Equal(t, data, pack)
}

func TestFetchResponseReaderPackets(t *testing.T) {
	t.Parallel()
	f := NewFetchResponseReader(strings.NewReader(fullResponse("\x01")), true)
	var kinds []FetchPacketKind
	var sections []string
	for {
		p, err := f.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		require.NoError(t, err)
		kinds = append(kinds, p.Kind)
		sections = append(sections, p.Section)
	}
	require.Equal(t, []FetchPacketKind{
		FetchHeader, FetchLine, FetchLine, FetchDelim,
		FetchHeader, FetchLine, FetchDelim,
		FetchHeader, FetchLine, FetchDelim,
		FetchHeader, FetchLine, FetchDelim,
		FetchHeader, FetchPackData, FetchProgress, FetchPackData, FetchProgress, FetchFlush,
	}, kinds)
	require.Equal(t, sectionShallowInfo, sections[4])
	require.Equal(t, SectionPackfile, sections[len(sections)-1])
	_, err := f.Next()
	require.ErrorIs(t, err, io.EOF)
}

func TestSidebandWriter(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	w := NewSidebandWriter(&out)
	data := bytes.Repeat([]byte{7}, 2*pktline.MaxPayloadSize+10)
	n, err := w.Write(data)
	require.NoError(t, err)
	require.Equal(t, len(data), n)
	require.Equal(t, 2*pktline.MaxSize, out.Len(), "full packets go out at once")
	require.NoError(t, w.Error(bytes.Repeat([]byte{'e'}, pktline.MaxPayloadSize)))
	require.NoError(t, w.Progress(nil))
	require.NoError(t, w.Flush())

	r := pktline.NewScanner(&out)
	var sizes []int
	var bands []byte
	var got []byte
	for r.Scan() {
		p := r.Bytes()
		sizes = append(sizes, len(p)+pktline.LenSize)
		bands = append(bands, p[0])
		if p[0] == BandData {
			got = append(got, p[1:]...)
		}
	}
	require.NoError(t, r.Err())
	require.Equal(t, []int{pktline.MaxSize, pktline.MaxSize, 17, pktline.MaxSize, 6, 5}, sizes)
	require.Equal(t, []byte{1, 1, 1, 3, 3, 2}, bands)
	require.Equal(t, data, got)
}

// FuzzFetchResponseReader checks that the reader never panics, that the raw
// packets it returns are exactly the bytes it read, and that the copy never
// panics.
func FuzzFetchResponseReader(f *testing.F) {
	f.Add([]byte(fullResponse("")), false)
	f.Add([]byte(fullResponse("\x01")), true)
	f.Add([]byte(pkt("acknowledgments\n")+pkt("NAK\n")+flushPkt), false)
	f.Add([]byte(pkt("ERR x")), false)
	f.Add([]byte(pkt("packfile\n")+pkt("\x03x")), true)
	f.Fuzz(func(t *testing.T, in []byte, sidebandAll bool) {
		r := NewFetchResponseReader(bytes.NewReader(in), sidebandAll)
		var raw []byte
		var err error
		for err == nil {
			var p FetchPacket
			p, err = r.Next()
			raw = append(raw, p.Wire...)
		}
		require.True(t, bytes.HasPrefix(in, raw))
		if CopyFetchResponse(io.Discard, bytes.NewReader(in), sidebandAll, copyPack) == nil {
			require.ErrorIs(t, err, io.EOF, "the copy accepts only a response that the reader accepts")
		}
	})
}
