package gitproto

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"testing/iotest"
)

// bodyString reads the whole body through a fresh Reader.
func bodyString(t *testing.T, b *RequestBody) string {
	t.Helper()
	data, err := io.ReadAll(b.Reader())
	if err != nil {
		t.Fatalf("reading request body: %v", err)
	}
	return string(data)
}

func newTestBody(chunks ...string) *RequestBody {
	b := &RequestBody{}
	for _, c := range chunks {
		b.appendChunk([]byte(c))
	}
	return b
}

func TestRequestBody_ReadAtAcrossChunks(t *testing.T) {
	t.Parallel()

	chunks := []string{"abc", "", "defgh", "i", "jklmnop"}
	b := newTestBody(chunks...)
	want := strings.Join(chunks, "")
	if b.Len() != int64(len(want)) {
		t.Fatalf("Len = %d, want %d", b.Len(), len(want))
	}

	for off := range len(want) + 1 {
		for size := range len(want) - off + 2 {
			p := make([]byte, size)
			n, err := b.ReadAt(p, int64(off))
			wantN := min(size, len(want)-off)
			if n != wantN || string(p[:n]) != want[off:off+wantN] {
				t.Fatalf("ReadAt(%d bytes, off %d) = %q, want %q", size, off, p[:n], want[off:off+wantN])
			}
			// At the end, io.EOF is allowed even for an empty read, as bytes.Reader does.
			if short := wantN < size || off == len(want); short != errors.Is(err, io.EOF) {
				t.Fatalf("ReadAt(%d bytes, off %d) err = %v, short read = %v", size, off, err, short)
			}
		}
	}
}

// The transport rewinds the body for its 401 retry and replica failover,
// and net/http calls GetBody to resend it on a redirect.
func TestRequestBody_Replays(t *testing.T) {
	t.Parallel()

	b := newTestBody("first ", "second ", "third")
	r := b.Reader()
	if _, err := io.CopyN(io.Discard, r, 8); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "first second third" {
		t.Fatalf("after rewind read %q", got)
	}

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "https://example.invalid/", b.Reader())
	if err != nil {
		t.Fatal(err)
	}
	b.ApplyTo(req)
	if req.ContentLength != b.Len() {
		t.Errorf("ContentLength = %d, want %d", req.ContentLength, b.Len())
	}
	rc, err := req.GetBody()
	if err != nil {
		t.Fatal(err)
	}
	got, err = io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "first second third" {
		t.Errorf("GetBody read %q", got)
	}
}

func TestRequestBody_ReadFromUsesFixedBlocks(t *testing.T) {
	t.Parallel()

	data := bytes.Repeat([]byte("x"), 2*bodyChunkSize+10)
	var b RequestBody
	n, err := b.ReadFrom(bytes.NewReader(data))
	if err != nil || n != int64(len(data)) {
		t.Fatalf("ReadFrom = %d, %v", n, err)
	}
	if len(b.chunks) != 3 {
		t.Errorf("got %d chunks, want 3", len(b.chunks))
	}
	if bodyString(t, &b) != string(data) {
		t.Error("body content differs from input")
	}
}

// finalReadErr yields data and returns err from the same Read that hands
// over the last bytes, which io.Reader allows.
type finalReadErr struct {
	data []byte
	err  error
}

func (r *finalReadErr) Read(p []byte) (int, error) {
	n := copy(p, r.data)
	r.data = r.data[n:]
	if len(r.data) == 0 {
		return n, r.err
	}
	return n, nil
}

// A reader that fails before EOF leaves the body incomplete; ReadFrom must
// return its error, as io.Copy did, so a truncated pack is never posted.
// The failure arrives mid-block, with a full block, and after one — both
// on a Read of its own and on the Read that returns the last bytes.
func TestRequestBody_ReadFromPropagatesErrors(t *testing.T) {
	t.Parallel()

	readers := map[string]func(data []byte, err error) io.Reader{
		"separate read": func(data []byte, err error) io.Reader {
			return io.MultiReader(bytes.NewReader(data), iotest.ErrReader(err))
		},
		"same read": func(data []byte, err error) io.Reader {
			return &finalReadErr{data: data, err: err}
		},
	}
	readErrs := []error{io.ErrUnexpectedEOF, errors.New("pipe broke"), fmt.Errorf("wrapped: %w", io.EOF)}

	for name, newReader := range readers {
		for _, size := range []int{0, 10, bodyChunkSize, bodyChunkSize + 10} {
			for _, readErr := range readErrs {
				var b RequestBody
				n, err := b.ReadFrom(newReader(make([]byte, size), readErr))
				if !errors.Is(err, readErr) {
					t.Errorf("%s, %d bytes, %v: ReadFrom err = %v", name, size, readErr, err)
				}
				if n != int64(size) || b.Len() != int64(size) {
					t.Errorf("%s, %d bytes, %v: read %d, Len %d", name, size, readErr, n, b.Len())
				}
			}
		}
	}
}

// io.EOF returned together with the last bytes is a clean end.
func TestRequestBody_ReadFromEOFWithFinalBytes(t *testing.T) {
	t.Parallel()

	for _, size := range []int{10, bodyChunkSize, bodyChunkSize + 10} {
		data := bytes.Repeat([]byte("y"), size)
		var b RequestBody
		n, err := b.ReadFrom(&finalReadErr{data: data, err: io.EOF})
		if err != nil || n != int64(size) {
			t.Errorf("%d bytes: ReadFrom = %d, %v", size, n, err)
		}
		if bodyString(t, &b) != string(data) {
			t.Errorf("%d bytes: body content differs from input", size)
		}
	}
}

// The agent amend must copy only the first chunk: the rest of the body is
// the pack, and copying it is what made a push hold the pack several times.
func TestAppendAgentToReceivePackBody_AmendsFirstChunkOnly(t *testing.T) {
	t.Parallel()

	oldSHA := strings.Repeat("a", 40)
	newSHA := strings.Repeat("b", 40)
	cmd := oldSHA + " " + newSHA + " " + testRefMain + "\x00 report-status agent=git/2.54.0\n"
	b := newTestBody(pktLine(cmd)+"0000", "PACK", "more pack")
	pack := b.chunks[1]

	if err := AppendAgentToReceivePackBody(b, "git-remote-entire/dev"); err != nil {
		t.Fatalf("AppendAgentToReceivePackBody: %v", err)
	}
	wantCmd := oldSHA + " " + newSHA + " " + testRefMain + "\x00 report-status agent=git/2.54.0+git-remote-entire/dev\n"
	want := pktLine(wantCmd) + "0000PACKmore pack"
	if got := bodyString(t, b); got != want {
		t.Errorf("body = %q, want %q", got, want)
	}
	if b.Len() != int64(len(want)) {
		t.Errorf("Len = %d, want %d", b.Len(), len(want))
	}
	if &b.chunks[1][0] != &pack[0] {
		t.Error("pack chunk was copied")
	}
}

func TestAppendAgentToReceivePackBody_FirstLineSplitFails(t *testing.T) {
	t.Parallel()

	cmd := pktLine(strings.Repeat("a", 40) + " " + strings.Repeat("b", 40) + " " + testRefMain + "\x00 agent=git/2.54.0\n")
	b := newTestBody(cmd[:20], cmd[20:]+"0000")
	if err := AppendAgentToReceivePackBody(b, "git-remote-entire/dev"); err == nil {
		t.Fatal("expected an error when the first command line spans chunks")
	}
}

func TestReadSendPackRequest_ChunkPerPacket(t *testing.T) {
	t.Parallel()

	payloads := []string{pktLine("cmd\n") + "0000", "PACK", "data"}
	var in strings.Builder
	for _, p := range payloads {
		in.WriteString(pktLine(p))
	}
	in.WriteString("0000")

	var b RequestBody
	if err := ReadSendPackRequest(bufio.NewReader(strings.NewReader(in.String())), &b); err != nil {
		t.Fatal(err)
	}
	if len(b.chunks) != len(payloads) {
		t.Fatalf("got %d chunks, want %d", len(b.chunks), len(payloads))
	}
	for i, p := range payloads {
		if string(b.chunks[i]) != p {
			t.Errorf("chunk %d = %q, want %q", i, b.chunks[i], p)
		}
	}
}

// AppendAgentToReceivePackBody relies on the capability line being the
// whole first chunk on the v0/v1 connect path.
func TestReadReceivePackRequest_FirstChunkIsFirstLine(t *testing.T) {
	t.Parallel()

	first := pktLine(strings.Repeat("0", 40) + " " + strings.Repeat("b", 40) + " " + testRefMain + "\x00 report-status\n")
	second := pktLine(strings.Repeat("0", 40) + " " + strings.Repeat("c", 40) + " refs/heads/other\n")
	b, err := ReadReceivePackRequest(strings.NewReader(first + second + "0000" + testFakePackData))
	if err != nil {
		t.Fatal(err)
	}
	if string(b.chunks[0]) != first {
		t.Errorf("first chunk = %q, want %q", b.chunks[0], first)
	}
	if got, want := bodyString(t, b), first+second+"0000"+testFakePackData; got != want {
		t.Errorf("body = %q, want %q", got, want)
	}
}
