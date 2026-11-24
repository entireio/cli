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

// holdsChunk reports whether b still holds c itself rather than a copy.
func holdsChunk(b *RequestBody, c []byte) bool {
	for _, held := range b.chunks {
		if &held[0] == &c[0] {
			return true
		}
	}
	return false
}

// The agent amend must copy only the capability line: the rest of the body
// is the pack, and copying it is what made a push hold the pack several times.
func TestAppendAgentToReceivePackBody_CopiesOnlyCapabilityLine(t *testing.T) {
	t.Parallel()

	cmd, wantCmd := testCommand()
	b := newTestBody(cmd+"0000", "PACK", "more pack")
	pack, morePack := b.chunks[1], b.chunks[2]

	if err := AppendAgentToReceivePackBody(b, testHelperAgent); err != nil {
		t.Fatalf("AppendAgentToReceivePackBody: %v", err)
	}
	want := wantCmd + "0000PACKmore pack"
	if got := bodyString(t, b); got != want {
		t.Errorf("body = %q, want %q", got, want)
	}
	if b.Len() != int64(len(want)) {
		t.Errorf("Len = %d, want %d", b.Len(), len(want))
	}
	if !holdsChunk(b, pack) || !holdsChunk(b, morePack) {
		t.Error("pack chunks were copied")
	}
}

const testHelperAgent = "git-remote-entire/dev"

// testCommand returns a receive-pack command line carrying capabilities,
// before and after the helper agent is appended.
func testCommand() (string, string) {
	head := strings.Repeat("a", 40) + " " + strings.Repeat("b", 40) + " " + testRefMain + "\x00 report-status agent=git/2.54.0"
	return pktLine(head + "\n"), pktLine(head + "+" + testHelperAgent + "\n")
}

// testShallowLines returns n shallow pkt-lines, which a shallow client
// sends ahead of its first command.
func testShallowLines(n int) string {
	var sb strings.Builder
	for i := range n {
		sb.WriteString(pktLine(fmt.Sprintf("shallow %040x\n", i)))
	}
	return sb.String()
}

// send-pack frames the request in payloads of at most 65515 bytes, so a
// command line behind enough shallow lines spans two chunks.
func TestAppendAgentToReceivePackBody_CommandSpansChunks(t *testing.T) {
	t.Parallel()

	cmd, wantCmd := testCommand()
	for _, split := range []int{1, 4, 20, len(cmd) - 1} {
		b := newTestBody(cmd[:split], cmd[split:]+"0000", "PACK")
		pack := b.chunks[2]
		if err := AppendAgentToReceivePackBody(b, testHelperAgent); err != nil {
			t.Fatalf("split at %d: %v", split, err)
		}
		if got, want := bodyString(t, b), wantCmd+"0000PACK"; got != want {
			t.Errorf("split at %d: body = %q, want %q", split, got, want)
		}
		if !holdsChunk(b, pack) {
			t.Errorf("split at %d: pack chunk was copied", split)
		}
	}
}

// A shallow client's shallow lines come first; the capabilities are on the
// first command after them, on both push paths.
func TestAppendAgentToReceivePackBody_AfterShallowLines(t *testing.T) {
	t.Parallel()

	cmd, wantCmd := testCommand()

	t.Run("connect", func(t *testing.T) {
		t.Parallel()
		shallow := testShallowLines(2)
		b, err := ReadReceivePackRequest(strings.NewReader(shallow + cmd + "0000" + testFakePackData))
		if err != nil {
			t.Fatal(err)
		}
		if err := AppendAgentToReceivePackBody(b, testHelperAgent); err != nil {
			t.Fatal(err)
		}
		if got, want := bodyString(t, b), shallow+wantCmd+"0000"+testFakePackData; got != want {
			t.Errorf("body = %q, want %q", got, want)
		}
	})

	t.Run("send-pack framing", func(t *testing.T) {
		t.Parallel()
		// Enough shallow lines that the command straddles send-pack's
		// first 65515-byte payload.
		shallow := testShallowLines(65515/53 - 1)
		request := shallow + cmd + "0000"
		var framed strings.Builder
		for rest := request; rest != ""; {
			n := min(len(rest), 65515)
			framed.WriteString(pktLine(rest[:n]))
			rest = rest[n:]
		}
		framed.WriteString(pktLine(testFakePackData) + "0000")

		var b RequestBody
		if err := ReadSendPackRequest(bufio.NewReader(strings.NewReader(framed.String())), &b); err != nil {
			t.Fatal(err)
		}
		if len(b.chunks) < 3 || len(b.chunks[0]) != 65515 {
			t.Fatalf("fixture: want the command split across the first two chunks, got %d chunks", len(b.chunks))
		}
		if err := AppendAgentToReceivePackBody(&b, testHelperAgent); err != nil {
			t.Fatal(err)
		}
		if got, want := bodyString(t, &b), shallow+wantCmd+"0000"+testFakePackData; got != want {
			t.Errorf("amended body differs (len %d, want %d)", len(got), len(want))
		}
	})
}

func TestAppendAgentToReceivePackBody_Unchanged(t *testing.T) {
	t.Parallel()

	noCaps := testShallowLines(1) + pktLine(strings.Repeat("a", 40)+" "+strings.Repeat("b", 40)+" "+testRefMain+"\n") + "0000PACK"
	for name, in := range map[string]string{
		"no capability line before flush": noCaps,
		"empty":                           "",
	} {
		b := newTestBody(in)
		if err := AppendAgentToReceivePackBody(b, testHelperAgent); err != nil {
			t.Errorf("%s: %v", name, err)
		}
		if got := bodyString(t, b); got != in {
			t.Errorf("%s: body = %q, want unchanged", name, got)
		}
	}
}

func TestAppendAgentToReceivePackBody_TruncatedFails(t *testing.T) {
	t.Parallel()

	cmd, _ := testCommand()
	for name, in := range map[string]string{
		"line longer than body": cmd[:len(cmd)-5],
		"partial length prefix": testShallowLines(1) + "00",
		"invalid length":        "0003",
		"non-hex length":        "zzzz",
	} {
		if err := AppendAgentToReceivePackBody(newTestBody(in), testHelperAgent); err == nil {
			t.Errorf("%s: expected an error", name)
		}
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
