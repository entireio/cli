package transcript

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/entireio/cli/internal/transcript/pijsonl"
)

var piFoldOpts = Options{Agent: "pi", CLIVersion: "0.1"}

const piHdr = `{"type":"session","version":3,"id":"s1","timestamp":"2026-01-01T00:00:00Z","cwd":"/w"}`

func piPar(p string) string {
	if p == "" {
		return "null"
	}
	return strconv.Quote(p)
}

func piUser(id, p, text string) string {
	return fmt.Sprintf(`{"type":"message","id":%q,"parentId":%s,"timestamp":"2026-01-01T00:00:01Z","message":{"role":"user","content":[{"type":"text","text":%q}]}}`, id, piPar(p), text)
}

func piUserImage(id, p, text string) string {
	return fmt.Sprintf(`{"type":"message","id":%q,"parentId":%s,"timestamp":"2026-01-01T00:00:01Z","message":{"role":"user","content":[{"type":"image","data":"QUJD","mimeType":"image/png"},{"type":"text","text":%q}]}}`, id, piPar(p), text)
}

func piAssistant(id, p, text string) string {
	return fmt.Sprintf(`{"type":"message","id":%q,"parentId":%s,"timestamp":"2026-01-01T00:00:02Z","message":{"role":"assistant","content":[{"type":"text","text":%q}],"usage":{"input":7,"output":2,"cacheRead":30,"cacheWrite":5,"cacheWrite1h":2,"reasoning":1},"model":"m1"}}`, id, piPar(p), text)
}

func piToolCall(id, p, call string) string {
	return fmt.Sprintf(`{"type":"message","id":%q,"parentId":%s,"timestamp":"2026-01-01T00:00:02Z","message":{"role":"assistant","content":[{"type":"text","text":"<run>"},{"type":"toolCall","id":%q,"name":"read","arguments":{"path":"x","n":1}}],"usage":{"input":9,"output":3,"cacheRead":0,"cacheWrite":0}}}`, id, piPar(p), call)
}

func piToolResult(id, p, call, out string, isErr bool) string {
	return fmt.Sprintf(`{"type":"message","id":%q,"parentId":%s,"timestamp":"2026-01-01T00:00:03Z","message":{"role":"toolResult","toolCallId":%q,"toolName":"read","content":[{"type":"text","text":%q}],"isError":%v}}`, id, piPar(p), call, out, isErr)
}

func piEmptyAssistant(id, p string) string {
	return fmt.Sprintf(`{"type":"message","id":%q,"parentId":%s,"timestamp":"2026-01-01T00:00:02Z","message":{"role":"assistant","content":[],"stopReason":"aborted"}}`, id, piPar(p))
}

// piUsageOnly is an assistant call with no visible content but usage, which
// keeps a line with content [].
func piUsageOnly(id, p string) string {
	return fmt.Sprintf(`{"type":"message","id":%q,"parentId":%s,"timestamp":"2026-01-01T00:00:02Z","message":{"role":"assistant","model":"m2","content":[{"type":"thinking","thinking":"x"}],"usage":{"input":4,"output":6,"cacheRead":0,"cacheWrite":0,"reasoning":6}}}`, id, piPar(p))
}

func piLines(ls ...string) []byte { return []byte(strings.Join(ls, "\n") + "\n") }

// requirePiFoldMatchesLegacy feeds data to a new adapter in chunks (nil rng:
// one line at a time; else 1 to 40 bytes) and checks, after every complete
// line, that folding every record so far equals the legacy compactor's
// output for the bytes so far, and Convert's. A final partial line is
// checked after Flush.
func requirePiFoldMatchesLegacy(t *testing.T, data []byte, rng *rand.Rand) {
	t.Helper()
	a := NewPiAdapter()
	f := NewFolder(piFoldOpts)
	check := func(fed int) {
		t.Helper()
		want, err := legacyCompactPi(data[:fed], piFoldOpts)
		if err != nil {
			t.Fatalf("legacyCompactPi: %v", err)
		}
		if got := f.Lines(); !bytes.Equal(got, want) {
			t.Fatalf("after line %d:\n got:\n%s\nwant:\n%s", a.Lines(), got, want)
		}
		if got, err := Convert(data[:fed], piFoldOpts); err != nil || !bytes.Equal(got, want) {
			t.Fatalf("Convert after line %d (%v):\n got:\n%s\nwant:\n%s", a.Lines(), err, got, want)
		}
	}
	for fed := 0; fed < len(data); {
		n := bytes.IndexByte(data[fed:], '\n') + 1
		if n == 0 {
			n = len(data) - fed
		}
		if rng != nil {
			n = min(len(data)-fed, 1+rng.IntN(40))
		}
		records := a.Feed(data[fed : fed+n])
		fed += n
		if _, err := f.Apply(records); err != nil {
			t.Fatalf("apply after byte %d: %v", fed, err)
		}
		if data[fed-1] == '\n' {
			check(fed)
		}
	}
	if len(data) > 0 && data[len(data)-1] != '\n' {
		if _, err := f.Apply(a.Flush()); err != nil {
			t.Fatalf("apply flush: %v", err)
		}
		check(len(data))
	}
}

// requirePiSlicesMatchLegacy checks Convert against the legacy compactor at
// every StartLine.
func requirePiSlicesMatchLegacy(t *testing.T, data []byte) {
	t.Helper()
	for start := 1; start <= bytes.Count(data, []byte("\n"))+1; start++ {
		opts := piFoldOpts
		opts.StartLine = start
		want, err := legacyCompactPi(data, opts)
		if err != nil {
			t.Fatalf("legacyCompactPi: %v", err)
		}
		if got, err := Convert(data, opts); err != nil || !bytes.Equal(got, want) {
			t.Fatalf("StartLine %d (%v):\n got:\n%s\nwant:\n%s", start, err, got, want)
		}
	}
}

var piAdapterCases = []struct {
	name string
	data []byte
}{
	{"linear with tool call and error result", piLines(piHdr, piUser("u1", "", "hi"), piToolCall("a1", "u1", "c1"), piToolResult("t1", "a1", "c1", "boom", true), piAssistant("a2", "t1", "done"))},
	{"tree switch", piLines(piHdr, piUser("u1", "", "one"), piAssistant("a1", "u1", "1"), piUser("u2", "a1", "two"), piAssistant("a2", "u2", "2"), `{"type":"label","id":"l1","parentId":"a2","timestamp":"t","targetId":"a2","label":"x"}`, piUser("u3", "a1", "three"), piAssistant("a3", "u3", "3"))},
	{"return to earlier branch", piLines(piHdr, piUser("u1", "", "one"), piAssistant("a1", "u1", "1"), piUser("u2", "a1", "two"), piAssistant("a2", "u2", "2"), piUser("u3", "a1", "three"), piAssistant("a3", "u3", "3"), piUser("u4", "a2", "four"), piAssistant("a4", "u4", "4"))},
	{"switch to assistant, toolResult left behind", piLines(piHdr, piUser("u1", "", "hi"), piToolCall("a1", "u1", "c1"), piToolResult("t1", "a1", "c1", "out", false), piAssistant("a2", "t1", "done"), piUser("u2", "a1", "again"), piAssistant("a3", "u2", "ok"))},
	{"return to branch with result", piLines(piHdr, piUser("u1", "", "hi"), piToolCall("a1", "u1", "c1"), piToolResult("t1", "a1", "c1", "out", false), piAssistant("a2", "t1", "done"), piUser("u2", "a1", "again"), piAssistant("a3", "u2", "ok"), piUser("u3", "a2", "back"), piAssistant("a4", "u3", "fin"))},
	{"empty assistant and branch summary", piLines(piHdr, piUser("u1", "", "hi"), piEmptyAssistant("a1", "u1"), piUser("u2", "a1", "retry"), piAssistant("a2", "u2", "ok"), `{"type":"branch_summary","id":"b1","parentId":"u1","timestamp":"t","fromId":"u1","summary":"s"}`, piUser("u3", "b1", "alt"), piAssistant("a3", "u3", "ok3"))},
	{"model and thinking entries in the chain", piLines(piHdr, `{"type":"model_change","id":"m0","parentId":null,"timestamp":"t","provider":"p","modelId":"m1"}`, `{"type":"thinking_level_change","id":"k0","parentId":"m0","timestamp":"t","thinkingLevel":"off"}`, piUser("u1", "k0", "hi"), piAssistant("a1", "u1", "x"), `{"type":"model_change","id":"m1","parentId":"a1","timestamp":"t","provider":"p","modelId":"m2"}`, piUser("u2", "m1", "y"), piAssistant("a2", "u2", "z"))},
	{"new root", piLines(piHdr, piUser("u1", "", "one"), piAssistant("a1", "u1", "1"), piUser("u2", "", "one again"), piAssistant("a2", "u2", "2"))},
	{"no tree yet: two roots before a reply", piLines(piHdr, piUser("u1", "", "one"), piUser("u2", "", "two"), piAssistant("a2", "u2", "2"))},
	{"a valid node but not a valid entry moves the tip", piLines(piHdr, piUser("u1", "", "one"), piAssistant("a1", "u1", "1"), piUser("u2", "a1", "two"), piAssistant("a2", "u2", "2"), `{"type":"message","id":"x","parentId":"a1","timestamp":5,"message":{"role":"user","content":"bad ts"}}`)},
	{"invalid JSON", piLines(piHdr, piUser("u1", "", "one"), `{not json`, piAssistant("a1", "u1", "1"))},
	{"string and empty user content", piLines(piHdr, `{"type":"message","id":"u1","parentId":null,"timestamp":"t","message":{"role":"user","content":"plain"}}`, `{"type":"message","id":"u0","parentId":"u1","timestamp":"t","message":{"role":"user","content":""}}`, piAssistant("a1", "u0", "1"))},
	{"another role moves the tip", piLines(piHdr, piUser("u1", "", "one"), piAssistant("a1", "u1", "1"), piUser("u2", "a1", "two"), piAssistant("a2", "u2", "2"), `{"type":"message","id":"bx","parentId":"a1","timestamp":"t","message":{"role":"bashExecution","command":"ls","output":"x"}}`)},
	{"usage-only call keeps an empty line", piLines(piHdr, piUser("u1", "", "hi"), piUsageOnly("a1", "u1"), piAssistant("a2", "a1", "ok"))},
	{"usage-only call left behind and returned to", piLines(piHdr, piUser("u1", "", "hi"), piUsageOnly("a1", "u1"), piUser("u2", "u1", "alt"), piAssistant("a2", "u2", "ok"), piUser("u3", "a1", "back"))},
	{"zero usage empty call has no line", piLines(piHdr, piUser("u1", "", "hi"), `{"type":"message","id":"a1","parentId":"u1","timestamp":"t","message":{"role":"assistant","content":[],"usage":{"input":0,"output":0,"cacheRead":0,"cacheWrite":0}}}`, piAssistant("a2", "a1", "ok"))},
	{"user images", piLines(piHdr, piUserImage("u1", "", "look"), piAssistant("a1", "u1", "seen"), `{"type":"message","id":"u2","parentId":"a1","timestamp":"t","message":{"role":"user","content":[{"type":"image","data":"REVG","mimeType":"image/jpeg"}]}}`)},
	{"non-text tool result output", piLines(piHdr, piUser("u1", "", "hi"), piToolCall("a1", "u1", "c1"), `{"type":"message","id":"t1","parentId":"a1","timestamp":"t","message":{"role":"toolResult","toolCallId":"c1","toolName":"read","content":[{"type":"image","data":"QUJD"}],"isError":false}}`)},
	{"unterminated last line", []byte(strings.Join([]string{piHdr, piUser("u1", "", "hi"), piAssistant("a1", "u1", "partial")}, "\n"))},
	{"CRLF and blank lines", []byte(piHdr + "\r\n\r\n" + piUser("u1", "", "hi") + "\r\n\n" + piAssistant("a1", "u1", "ok") + "\r\n")},
}

func TestPiAdapter_FoldMatchesLegacy(t *testing.T) {
	t.Parallel()
	for _, c := range piAdapterCases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			requirePiFoldMatchesLegacy(t, c.data, nil)
			for seed := range uint64(20) {
				requirePiFoldMatchesLegacy(t, c.data, rand.New(rand.NewPCG(seed, 1)))
			}
			requirePiSlicesMatchLegacy(t, c.data)
		})
	}
}

func TestPiAdapter_Fixtures(t *testing.T) {
	t.Parallel()
	paths, err := filepath.Glob("testdata/pi_*full.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	streamed, err := filepath.Glob("testdata/pi_stream_*.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	paths = append(paths, streamed...)
	if len(paths) < 4 {
		t.Fatalf("fixtures %v", paths)
	}
	for _, path := range paths {
		t.Run(filepath.Base(path), func(t *testing.T) {
			t.Parallel()
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			requirePiFoldMatchesLegacy(t, data, nil)
			for seed := range uint64(20) {
				requirePiFoldMatchesLegacy(t, data, rand.New(rand.NewPCG(seed, 2)))
			}
			requirePiSlicesMatchLegacy(t, data)
		})
	}
}

// Random Pi-like trees: appends to the leaf, /tree jumps to any earlier
// entry, new roots, tool calls (IDs may repeat) with results on any branch,
// empty and usage-only assistants, user images, model changes. For the
// adapter's fast path's guards also: labels hung off any earlier entry (the
// leaf stays), messages under an entry just above the leaf (as under a chain
// of model changes), parents a later line defines, and entries with no
// parentId.
func TestPiAdapter_GeneratedTrees(t *testing.T) {
	t.Parallel()
	n := uint64(1000)
	if testing.Short() {
		n = 200
	}
	for seed := range n {
		data := piGeneratedTree(rand.New(rand.NewPCG(seed, 7)))
		t.Run(strconv.FormatUint(seed, 10), func(t *testing.T) {
			t.Parallel()
			requirePiFoldMatchesLegacy(t, data, nil)
			if seed%10 == 0 {
				requirePiSlicesMatchLegacy(t, data)
			}
		})
	}
}

// piDecodeMessage works out emptiness without building the record: it must
// agree with piRecord on the edge cases below and every message the
// generator writes.
func TestPiDecodeMessage_EmptyAgreesWithRecord(t *testing.T) {
	t.Parallel()
	assistant := func(content string) string {
		return `{"type":"message","id":"a","parentId":null,"timestamp":"t","message":{"role":"assistant","content":` + content + `}}`
	}
	lines := []string{
		assistant(`""`),
		assistant(`"plain"`),
		assistant(`[]`),
		assistant(`[{"type":"text","text":""}]`),
		assistant(`[{"type":"thinking","thinking":"x"}]`),
		assistant(`[{"type":"text","text":""},{"type":"toolCall","id":"c1","name":"read"}]`),
		assistant(`[{"type":"toolCall","name":"read"}]`),
		assistant(`{"not":"blocks"}`),
		piEmptyAssistant("a", ""),
		piUsageOnly("a", ""),
	}
	for seed := range uint64(200) {
		lines = append(lines, strings.Split(string(piGeneratedTree(rand.New(rand.NewPCG(seed, 7)))), "\n")...)
	}
	for _, l := range lines {
		e, ok := piDecodeEntry([]byte(l))
		if !ok || e.Type != pijsonl.EntryTypeMessage {
			continue
		}
		_, has := piRecord(e, nil)
		if got := piDecodeMessage(e).empty; got != !has {
			t.Fatalf("empty = %v, piRecord ok = %v, for %s", got, has, l)
		}
	}
}

func TestPiAdapter_LongPendingLineIsReleased(t *testing.T) {
	t.Parallel()
	long := piUser("u1", "", strings.Repeat("x", 2*piPendingKeep))
	a := NewPiAdapter()
	a.Feed([]byte(piHdr + "\n" + long[:len(long)/2]))
	a.Feed([]byte(long[len(long)/2:] + "\n"))
	if c := cap(a.pending); c > piPendingKeep {
		t.Fatalf("pending keeps %d bytes after the long line was read", c)
	}
	short := piUser("u2", "u1", "y")
	a.Feed([]byte(short[:10]))
	a.Feed([]byte(short[10:] + "\n"))
	if c := cap(a.pending); c == 0 {
		t.Fatal("pending let a short line's buffer go")
	}
}

func piGeneratedTree(rng *rand.Rand) []byte {
	ls := []string{piHdr}
	var ids, calls []string
	leaf := ""
	count := 4 + rng.IntN(25)
	for i := range count {
		switch {
		case len(ids) > 0 && rng.IntN(5) == 0:
			leaf = ids[rng.IntN(len(ids))]
			if rng.IntN(6) == 0 {
				leaf = ""
			}
		case len(ids) > 2 && rng.IntN(6) == 0:
			leaf = ids[len(ids)-1-rng.IntN(3)]
		case i+1 < count && rng.IntN(12) == 0:
			leaf = fmt.Sprintf("e%d", i+1+rng.IntN(count-i-1)) // defined later
		}
		id := fmt.Sprintf("e%d", i)
		var l string
		switch k := rng.IntN(11); {
		case k == 1:
			l = piAssistant(id, leaf, fmt.Sprint("r", i))
		case k == 2:
			c := fmt.Sprintf("c%d", rng.IntN(4))
			calls = append(calls, c)
			l = piToolCall(id, leaf, c)
		case k == 3 && len(calls) > 0:
			l = piToolResult(id, leaf, calls[rng.IntN(len(calls))], fmt.Sprint("o", i), rng.IntN(2) == 0)
		case k == 4:
			l = piEmptyAssistant(id, leaf)
		case k == 5:
			l = fmt.Sprintf(`{"type":"model_change","id":%q,"parentId":%s,"timestamp":"t","provider":"p","modelId":"m%d"}`, id, piPar(leaf), i)
		case k == 6 && len(ids) > 0:
			target := ids[rng.IntN(len(ids))]
			ls = append(ls, fmt.Sprintf(`{"type":"label","id":%q,"parentId":%q,"timestamp":"t","targetId":%q,"label":"x"}`, id, target, target))
			ids = append(ids, id)
			continue // the leaf stays
		case k == 7:
			l = piUsageOnly(id, leaf)
		case k == 8:
			l = piUserImage(id, leaf, fmt.Sprint("q", i))
		default:
			l = piUser(id, leaf, fmt.Sprint("q", i))
		}
		if rng.IntN(15) == 0 {
			l = strings.Replace(l, `"parentId":`+piPar(leaf)+`,`, "", 1)
		}
		ls = append(ls, l)
		ids = append(ids, id)
		leaf = id
	}
	return piLines(ls...)
}

func TestPiAdapter_UsageOnlyCallHasNoContent(t *testing.T) {
	t.Parallel()
	a := NewPiAdapter()
	recs := a.Feed(piLines(piHdr, piUser("u1", "", "hi"), piUsageOnly("a1", "u1")))
	if len(recs) != 2 {
		t.Fatalf("records = %+v, want 2", recs)
	}
	r := recs[1]
	if r.ID != "a1" || r.SourceID != "a1" || r.Content != nil || r.Usage == nil || r.Usage.OutputTokens != 6 || r.Usage.Model != "m2" {
		t.Errorf("usage-only record = %+v", r)
	}
	if recs[0].SourceID != "" {
		t.Errorf("user record = %+v", recs[0])
	}
}

func TestPiAdapter_LinesCountsConsumedLines(t *testing.T) {
	t.Parallel()
	a := NewPiAdapter()
	a.Feed([]byte(piHdr + "\n\n{not json\n" + piUser("u1", "", "hi")))
	if a.Lines() != 3 {
		t.Errorf("Lines() = %d before Flush, want 3", a.Lines())
	}
	if recs := a.Flush(); len(recs) != 1 || recs[0].ID != "u1" {
		t.Errorf("Flush records = %+v", recs)
	}
	if a.Lines() != 4 {
		t.Errorf("Lines() = %d after Flush, want 4", a.Lines())
	}
	if recs := a.Flush(); recs != nil {
		t.Errorf("second Flush = %+v, want none", recs)
	}
}

// feedPiRecords feeds data in pieces of at most size bytes, then flushes.
func feedPiRecords(data []byte, size int) []Record {
	a := NewPiAdapter()
	var recs []Record
	for len(data) > 0 {
		n := min(len(data), size)
		recs = append(recs, a.Feed(data[:n])...)
		data = data[n:]
	}
	return append(recs, a.Flush()...)
}

// The adapter has no line size limit, as for Claude Code: a line past the Pi
// scanner's 10 MiB limit is read whole, however it is chunked, and keeps its
// place in the tree.
func TestPiAdapter_LongLinesAreReadWhole(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("x", 11<<20)
	data := piLines(piHdr, piUser("u1", "", "one"), piToolCall("a1", "u1", "c1"),
		piToolResult("t1", "a1", "c1", long, false), piUser("u2", "t1", long), piAssistant("a2", "u2", "2"))
	want, err := Fold(feedPiRecords(data, len(data)), piFoldOpts)
	if err != nil {
		t.Fatalf("Fold: %v", err)
	}
	lines, err := Decode(want)
	if err != nil || len(lines) != 4 {
		t.Fatalf("Decode: %d lines, %v", len(lines), err)
	}
	if r := lines[1].Blocks[1].Result; r == nil || r.Output != long {
		t.Errorf("long tool result not spliced into a1")
	}
	if lines[2].Blocks[0].Text != long {
		t.Errorf("long user text not kept")
	}
	for _, size := range []int{3 << 20, 1 << 20} {
		got, err := Fold(feedPiRecords(data, size), piFoldOpts)
		if err != nil || !bytes.Equal(got, want) {
			t.Errorf("%d-byte pieces: fold differs from a whole feed (%v)", size, err)
		}
	}
	// An unterminated long last line is read by Flush.
	unterminated := bytes.TrimSuffix(piLines(piHdr, piUser("u1", "", long)), []byte("\n"))
	if recs := feedPiRecords(unterminated, 3<<20); len(recs) != 1 || recs[0].ID != "u1" {
		t.Errorf("unterminated long line: %d records", len(recs))
	}
}

// Convert still fails where the legacy compactor did: on a line of
// pijsonl.MaxScannerLine bytes or more, newline excluded, terminated or not.
func TestPiConvert_FailsOnLongLinesAsBefore(t *testing.T) {
	t.Parallel()
	line := func(n int) string {
		l := piUser("u2", "u1", "x")
		return l[:len(l)-4] + strings.Repeat(" ", n-len(l)) + l[len(l)-4:]
	}
	for _, tt := range []struct {
		name string
		data []byte
		fail bool
	}{
		{"one byte under the limit", piLines(piHdr, piUser("u1", "", "one"), line(pijsonl.MaxScannerLine-1)), false},
		{"at the limit", piLines(piHdr, piUser("u1", "", "one"), line(pijsonl.MaxScannerLine)), true},
		{"at the limit with CR", piLines(piHdr, piUser("u1", "", "one"), line(pijsonl.MaxScannerLine-1)+"\r"), true},
		{"unterminated, one under", []byte(piHdr + "\n" + line(pijsonl.MaxScannerLine-1)), false},
		{"unterminated, at the limit", []byte(piHdr + "\n" + line(pijsonl.MaxScannerLine)), true},
	} {
		got, err := Convert(tt.data, piFoldOpts)
		want, legacyErr := legacyCompactPi(tt.data, piFoldOpts)
		if (legacyErr != nil) != tt.fail {
			t.Fatalf("%s: legacy error %v, want failure %v", tt.name, legacyErr, tt.fail)
		}
		if tt.fail {
			if !errors.Is(err, bufio.ErrTooLong) {
				t.Errorf("%s: Convert error %v, want bufio.ErrTooLong", tt.name, err)
			}
			continue
		}
		if err != nil || !bytes.Equal(got, want) {
			t.Errorf("%s: Convert (%v) differs from legacy", tt.name, err)
		}
	}
}

// Attaching to a long session reads it all at once: it must stay fast
// (linear in the lines while the session only grows). 3,000 lines took
// 0.6 s and 9,000 took 5.6 s when every line reconciled the whole tree; the
// three sizes show the time now grows with the lines, not their square.
func BenchmarkPiAdapterLongSession(b *testing.B) {
	for _, n := range []int{3_000, 9_000, 10_000} {
		ls := []string{piHdr}
		prev := ""
		for i := 0; len(ls)+3 <= n; i++ {
			u, c, r := fmt.Sprint("u", i), fmt.Sprint("a", i), fmt.Sprint("t", i)
			call := fmt.Sprint("c", i)
			ls = append(ls, piUser(u, prev, "q"), piToolCall(c, u, call), piToolResult(r, c, call, strings.Repeat("o", 1000), false))
			prev = r
		}
		for i := 0; len(ls) < n; i++ {
			ls = append(ls, piAssistant(fmt.Sprint("x", i), prev, "x"))
			prev = fmt.Sprint("x", i)
		}
		data := piLines(ls...)
		b.Run(strconv.Itoa(len(ls))+"lines", func(b *testing.B) {
			b.SetBytes(int64(len(data)))
			for b.Loop() {
				NewPiAdapter().Feed(data)
			}
		})
	}
}
