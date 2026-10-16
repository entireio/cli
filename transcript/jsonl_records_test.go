package transcript

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"os"
	"strings"
	"testing"
)

// jsonlFixtures are the golden inputs handled by compactJSONLWith, with the
// preprocessor Compact selects for them.
var jsonlFixtures = []struct {
	path       string
	preprocess linePreprocessor
}{
	{"testdata/claude_full.jsonl", nil},
	{"testdata/claude_full2.jsonl", nil},
	{"testdata/droid_full.jsonl", unwrapDroidEnvelope},
}

// jsonlEdgeCases pin the compaction rules the adapter must reproduce.
var jsonlEdgeCases = map[string]string{
	"fragments merge, later ts and non-zero tokens win": `{"type":"assistant","timestamp":"t1","message":{"id":"m1","content":[{"type":"text","text":"a"}],"usage":{"input_tokens":5,"output_tokens":1}}}
{"type":"assistant","timestamp":"t2","message":{"id":"m1","content":[{"type":"text","text":"b"}],"usage":{"input_tokens":0,"output_tokens":9}}}
{"type":"assistant","message":{"id":"m1","content":[{"type":"text","text":"c"}]}}
`,
	"dropped lines do not break adjacency": `{"type":"assistant","timestamp":"t1","message":{"id":"m1","content":[{"type":"tool_use","id":"tu1","name":"Bash","input":{}}]}}
{"type":"progress","data":{}}
not json

{"type":"user","timestamp":"t2","message":{"content":[{"type":"tool_result","tool_use_id":"tu1","content":"ok"}]}}
`,
	"same id in two non-adjacent runs": `{"type":"assistant","timestamp":"t1","message":{"id":"m1","content":[{"type":"tool_use","id":"tu1","name":"Bash","input":{}}]}}
{"type":"user","timestamp":"t2","message":{"content":[{"type":"tool_result","tool_use_id":"tu1","content":"ok"}]}}
{"type":"assistant","timestamp":"t3","message":{"id":"m1","content":[{"type":"text","text":"done"}]}}
`,
	"result only from the immediately following user entry": `{"type":"assistant","timestamp":"t1","message":{"id":"m1","content":[{"type":"tool_use","id":"tu1","name":"Bash","input":{}}]}}
{"type":"user","timestamp":"t2","message":{"content":"interrupt"}}
{"type":"user","timestamp":"t3","message":{"content":[{"type":"tool_result","tool_use_id":"tu1","content":"late"}]}}
{"type":"user","timestamp":"t4","message":{"content":[{"type":"tool_result","tool_use_id":"tu1","content":"late"},{"type":"text","text":"kept"}]}}
`,
	"unmatched and empty tool_use_id results": `{"type":"assistant","timestamp":"t1","message":{"id":"m1","content":[{"type":"tool_use","id":"tu1","name":"A","input":{}},{"type":"tool_use","id":"tu2","name":"B","input":{}}]}}
{"type":"user","timestamp":"t2","message":{"content":[{"type":"tool_result","tool_use_id":"nope","content":"x"},{"type":"tool_result","content":"last","is_error":true}]},"toolUseResult":{"stdout":"ignored for two results"}}
`,
	"stdout-only toolUseResult targets the last tool_use": `{"type":"assistant","timestamp":"t1","message":{"id":"m1","content":[{"type":"tool_use","id":"tu1","name":"Bash","input":{}}]}}
{"type":"user","timestamp":"t2","message":{"content":[]},"toolUseResult":{"stdout":"out"}}
`,
	"consumed user text and images follow the assistant": `{"type":"assistant","timestamp":"t1","message":{"id":"m1","content":[{"type":"tool_use","id":"tu1","name":"Read","input":{}}]}}
{"type":"user","timestamp":"t2","promptId":"p1","message":{"content":[{"type":"tool_result","tool_use_id":"tu1","content":"x"},{"type":"image","source":{"type":"base64","data":"AAA"}}]},"toolUseResult":{"file":{"filePath":"/a","numLines":3}}}
`,
	"thinking-only assistant is skipped": `{"type":"assistant","timestamp":"t1","message":{"id":"m1","content":[{"type":"thinking","thinking":"hm"}]}}
{"type":"user","timestamp":"t2","message":{"content":[{"type":"tool_result","tool_use_id":"tu1","content":"x"}]}}
`,
	"empty assistant before consumed user text": `{"type":"assistant","timestamp":"t1","message":{"id":"m1","content":[{"type":"thinking","thinking":"hm"}]}}
{"type":"user","timestamp":"t2","message":{"content":[{"type":"tool_result","tool_use_id":"tu1","content":"x"},{"type":"text","text":"stop"}]}}
`,
	"non-array fragments": `{"type":"assistant","timestamp":"t1","message":{"id":"m1","content":"a string"}}
{"type":"assistant","timestamp":"t2","message":{"id":"m1","content":[{"type":"text","text":"b"}]}}
{"type":"assistant","timestamp":"t3","message":{"id":"m2"}}
{"type":"assistant","timestamp":"t4","message":{"id":"m2","content":[]}}
{"type":"assistant","timestamp":"t5","message":{"id":"m3","content":null}}
{"type":"assistant","timestamp":"t6","message":{"id":"m3","content":null}}
{"type":"assistant","timestamp":null,"message":{"id":"m4","content":[1,{"type":"tool_use","id":"tu1","name":"X","input":{}}]}}
{"type":"user","timestamp":"t8","message":{"content":[{"type":"tool_result","tool_use_id":"tu1","content":"x"}]}}
`,
	"non-array fragments after array content": `{"type":"assistant","timestamp":"t1","message":{"id":"m1","content":[{"type":"tool_use","id":"tu1","name":"X","input":{}}],"usage":{"input_tokens":5}}}
{"type":"assistant","timestamp":"t2","message":{"id":"m1","content":"str"}}
{"type":"assistant","timestamp":"t3","message":{"id":"m1","content":null,"usage":{"output_tokens":7}}}
{"type":"assistant","message":{"id":"m1"}}
{"type":"assistant","timestamp":"t5","message":{"id":"m1","content":[{"type":"text","text":"b"}]}}
{"type":"user","timestamp":"t6","message":{"content":[{"type":"tool_result","tool_use_id":"tu1","content":"x"}]}}
{"type":"assistant","timestamp":"t7","message":{"id":"m2","content":[]}}
{"type":"assistant","timestamp":"t8","message":{"id":"m2","content":"str"}}
`,
	"id-less assistants merge (Cursor)": `{"role":"user","message":{"content":[{"type":"text","text":"<user_query>hi</user_query>"}]}}
{"role":"assistant","message":{"content":[{"type":"text","text":"a"}]}}
{"role":"assistant","message":{"content":[{"type":"text","text":"b <&>"}]}}
`,
	"orphan tool-result user entries": `{"type":"user","timestamp":"t1","message":{"content":[{"type":"tool_result","tool_use_id":"tu1","content":"x"}]}}
{"type":"human","timestamp":"t2","message":{"content":[{"type":"tool_result","tool_use_id":"tu1","content":"x"},{"type":"text","text":"hello"}]}}
{"type":"user","timestamp":"t3","message":{"content":""}}
`,
	"no trailing newline": `{"type":"user","timestamp":"t1","message":{"content":"hi"}}
{"type":"assistant","timestamp":"t2","message":{"id":"m1","content":[{"type":"text","text":"yo"}]}}`,
}

func TestJSONLAdapter_FoldMatchesLegacyBytes(t *testing.T) {
	t.Parallel()

	for _, fx := range jsonlFixtures {
		t.Run(fx.path, func(t *testing.T) {
			t.Parallel()
			input, err := os.ReadFile(fx.path)
			if err != nil {
				t.Fatal(err)
			}
			assertMatchesLegacy(t, input, fx.preprocess)
		})
	}
	for name, input := range jsonlEdgeCases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			assertMatchesLegacy(t, []byte(input), nil)
		})
	}
}

// TestJSONLAdapter_RandomTranscriptsMatchLegacy generates transcripts from a
// pool of lines that exercises every compaction rule and compares the folded
// records with the legacy compactor byte for byte, under random chunking.
func TestJSONLAdapter_RandomTranscriptsMatchLegacy(t *testing.T) {
	t.Parallel()

	for seed := range uint64(500) {
		rng := rand.New(rand.NewPCG(seed, 0))
		preprocess, input := randomTranscript(rng)

		assertMatchesLegacy(t, input, preprocess)

		whole := adapterRecords(input, preprocess, [][]byte{input})
		chunked := adapterRecords(input, preprocess, randomChunks(rng, input))
		if !bytes.Equal(encodeRecords(t, whole), encodeRecords(t, chunked)) {
			t.Fatalf("seed %d: chunked records differ\ninput:\n%s", seed, input)
		}
	}
}

func TestJSONLAdapter_ChunkingAtEveryLineBoundary(t *testing.T) {
	t.Parallel()

	type input struct {
		data       []byte
		preprocess linePreprocessor
	}
	inputs := make(map[string]input)
	for _, fx := range jsonlFixtures {
		b, err := os.ReadFile(fx.path)
		if err != nil {
			t.Fatal(err)
		}
		inputs[fx.path] = input{b, fx.preprocess}
	}
	for name, s := range jsonlEdgeCases {
		inputs[name] = input{data: []byte(s)}
	}

	for name, in := range inputs {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			input, preprocess := in.data, in.preprocess
			whole := adapterRecords(input, preprocess, nil)
			if len(whole) == 0 {
				t.Fatal("input produced no records")
			}
			want := encodeRecords(t, whole)
			wantFold := foldRecords(t, whole)

			// Every line as its own chunk splits at all boundaries at once.
			// Small inputs are also split in two at each boundary; that is
			// quadratic, so the 1 MB fixture relies on the line-per-chunk pass.
			for i := range input {
				if input[i] != '\n' || len(input) > 64<<10 {
					continue
				}
				recs := adapterRecords(input, preprocess, [][]byte{input[:i+1], input[i+1:]})
				if got := encodeRecords(t, recs); !bytes.Equal(got, want) {
					t.Fatalf("split after byte %d: records differ\ngot:\n%s\nwant:\n%s", i, got, want)
				}
			}
			recs := adapterRecords(input, preprocess, bytes.SplitAfter(input, []byte("\n")))
			if got := encodeRecords(t, recs); !bytes.Equal(got, want) {
				t.Fatalf("line-per-chunk records differ\ngot:\n%s\nwant:\n%s", got, want)
			}
			if got := foldRecords(t, recs); !bytes.Equal(got, wantFold) {
				t.Fatalf("line-per-chunk fold differs\ngot:\n%s\nwant:\n%s", got, wantFold)
			}
		})
	}
}

func TestJSONLAdapter_FeedHoldsPartialLineUntilComplete(t *testing.T) {
	t.Parallel()

	a := newJSONLAdapter(nil)
	line := `{"type":"user","timestamp":"t1","message":{"content":"hi"}}` + "\n"
	if recs := a.Feed([]byte(line[:20])); len(recs) != 0 {
		t.Fatalf("partial line emitted records: %+v", recs)
	}
	if recs := a.Feed([]byte(line[20:])); len(recs) != 1 {
		t.Fatalf("completed line: got %d records, want 1", len(recs))
	}
	if recs := a.Flush(); len(recs) != 0 {
		t.Fatalf("Flush after a complete line emitted records: %+v", recs)
	}
}

func TestJSONLAdapter_ReusedMessageIDGetsDistinctKey(t *testing.T) {
	t.Parallel()

	recs := adapterRecords([]byte(jsonlEdgeCases["same id in two non-adjacent runs"]), nil, nil)

	var messages []Record
	for _, r := range recs {
		if r.Op == OpMessage && r.Role == TypeAssistant {
			messages = append(messages, r)
		}
	}
	if len(messages) != 2 {
		t.Fatalf("got %d assistant messages, want 2: %+v", len(messages), recs)
	}
	if messages[0].ID != "m1" || messages[0].SourceID != "m1" {
		t.Errorf("first run: id %q sourceId %q, want m1/m1", messages[0].ID, messages[0].SourceID)
	}
	if messages[1].ID != "m1@L3" || messages[1].SourceID != "m1" {
		t.Errorf("second run: id %q sourceId %q, want m1@L3/m1", messages[1].ID, messages[1].SourceID)
	}

	out := foldRecords(t, recs)
	if n := strings.Count(string(out), `"id":"m1"`); n != 2 {
		t.Errorf("compact output prints id m1 %d times, want 2:\n%s", n, out)
	}
}

func TestJSONLAdapter_UserMessagesHaveDerivedKeysOnly(t *testing.T) {
	t.Parallel()

	recs := adapterRecords([]byte(`{"type":"assistant","message":{"id":"@L2","content":[{"type":"text","text":"x"}]}}
{"type":"user","promptId":"p1","message":{"content":"hi"}}
`), nil, nil)
	if len(recs) != 2 {
		t.Fatalf("got %d records, want 2", len(recs))
	}
	if recs[0].ID != "@L2" {
		t.Errorf("assistant key %q, want the agent's ID @L2", recs[0].ID)
	}
	// The derived key for line 2 collides with that agent ID.
	if recs[1].ID != "@L2'" || recs[1].SourceID != "" {
		t.Errorf("user key %q sourceId %q, want @L2' and none", recs[1].ID, recs[1].SourceID)
	}
}

// TestJSONLAdapter_EmptyAssistantBeforeConsumedUserTextIsSkipped pins the one
// intentional change from the legacy compactor, which emitted `"content":[]`
// for an empty assistant whose following user entry had tool results and text.
func TestJSONLAdapter_EmptyAssistantBeforeConsumedUserTextIsSkipped(t *testing.T) {
	t.Parallel()

	out, err := compactJSONL([]byte(jsonlEdgeCases["empty assistant before consumed user text"]), defaultOpts)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"v":1,"agent":"claude-code","cli_version":"0.5.1","type":"user","ts":"t2","content":[{"text":"stop"}]}` + "\n"
	if string(out) != want {
		t.Errorf("got:\n%s\nwant:\n%s", out, want)
	}
}

// Alternating array and non-array fragments must not restate the message on
// every non-array fragment: the records would grow with the square of the
// message's length.
func TestJSONLAdapter_AlternatingFragmentRecordsGrowLinearly(t *testing.T) {
	t.Parallel()

	alternating := func(n int) []byte {
		var b strings.Builder
		for i := range n {
			content := `"s"`
			if i%2 == 0 {
				content = fmt.Sprintf(`[{"type":"text","text":"fragment %d"}]`, i)
			}
			fmt.Fprintf(&b, `{"type":"assistant","timestamp":"t%d","message":{"id":"m1","content":%s}}`+"\n", i, content)
		}
		return []byte(b.String())
	}
	for _, n := range []int{1000, 4000} {
		input := alternating(n)
		assertMatchesLegacy(t, input, nil)
		if size := len(encodeRecords(t, adapterRecords(input, nil, nil))); size > 2*len(input) {
			t.Errorf("%d fragments: %d bytes of records for %d bytes of input", n, size, len(input))
		}
	}
}

func assertMatchesLegacy(t *testing.T, input []byte, preprocess linePreprocessor) {
	t.Helper()

	want, err := legacyCompactJSONLWith(input, defaultOpts, preprocess)
	if err != nil {
		t.Fatalf("legacy: %v", err)
	}
	got, err := compactJSONLWith(input, defaultOpts, preprocess)
	if err != nil {
		t.Fatalf("compactJSONLWith: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("output differs from legacy compactor\ninput:\n%s\ngot:\n%s\nwant:\n%s", input, got, want)
	}
}

// adapterRecords feeds chunks (or the whole input when chunks is nil) to a
// fresh adapter and returns every record, including Flush's.
func adapterRecords(input []byte, preprocess linePreprocessor, chunks [][]byte) []Record {
	if chunks == nil {
		chunks = [][]byte{input}
	}
	a := newJSONLAdapter(preprocess)
	var recs []Record
	for _, c := range chunks {
		recs = append(recs, a.Feed(c)...)
	}
	return append(recs, a.Flush()...)
}

func encodeRecords(t *testing.T, recs []Record) []byte {
	t.Helper()
	var buf bytes.Buffer
	for _, r := range recs {
		b, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		buf.Write(b)
		buf.WriteByte('\n')
	}
	return buf.Bytes()
}

func foldRecords(t *testing.T, recs []Record) []byte {
	t.Helper()
	out, err := Fold(recs, defaultOpts)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func randomChunks(rng *rand.Rand, input []byte) [][]byte {
	var chunks [][]byte
	for len(input) > 0 {
		n := 1 + rng.IntN(min(len(input), 200))
		chunks = append(chunks, input[:n])
		input = input[n:]
	}
	return chunks
}

// randomTranscript builds a Claude Code, Cursor or Droid transcript from a
// small vocabulary, so message IDs and tool_use IDs collide often.
func randomTranscript(rng *rand.Rand) (linePreprocessor, []byte) {
	ids := []string{"m1", "m2", ""}
	toolIDs := []string{"tu1", "tu2", ""}
	timestamps := []string{`"t1"`, `"t2"`, `null`, ``}

	tsField := func() string {
		ts := timestamps[rng.IntN(len(timestamps))]
		if ts == "" {
			return ""
		}
		return `"timestamp":` + ts + `,`
	}
	assistantContent := func() string {
		switch rng.IntN(9) {
		case 0:
			return `,"content":"plain <b>&</b>"`
		case 1:
			return ``
		case 2:
			return `,"content":null`
		case 3:
			return `,"content":[]`
		case 4:
			return `,"content":[{"type":"thinking","thinking":"hm","signature":"s"}]`
		default:
			var blocks []string
			for range 1 + rng.IntN(2) {
				if rng.IntN(2) == 0 {
					blocks = append(blocks, fmt.Sprintf(`{"type":"tool_use","id":%q,"name":"Bash","input":{"cmd":"ls"},"caller":"x"}`, toolIDs[rng.IntN(len(toolIDs))]))
				} else {
					blocks = append(blocks, `{"text":"hello <&>","type":"text"}`)
				}
			}
			return `,"content":[` + strings.Join(blocks, ",") + `]`
		}
	}
	usage := func() string {
		if rng.IntN(2) == 0 {
			return ""
		}
		return fmt.Sprintf(`,"usage":{"input_tokens":%d,"output_tokens":%d}`, rng.IntN(3)*10, rng.IntN(3)*7)
	}
	userContent := func() (string, string) {
		var blocks []string
		for range rng.IntN(3) {
			switch rng.IntN(4) {
			case 0:
				blocks = append(blocks, fmt.Sprintf(`{"type":"tool_result","tool_use_id":%q,"content":"out","is_error":%t}`, toolIDs[rng.IntN(len(toolIDs))], rng.IntN(2) == 0))
			case 1:
				blocks = append(blocks, `{"type":"text","text":"<ide_opened_file>f</ide_opened_file>do it"}`)
			case 2:
				blocks = append(blocks, `{"type":"image","source":{"type":"base64","data":"AA=="}}`)
			default:
				blocks = append(blocks, `{"type":"tool_result","content":"anon"}`)
			}
		}
		content := `[` + strings.Join(blocks, ",") + `]`
		if rng.IntN(5) == 0 {
			content = `"just text"`
		}
		extra := ""
		switch rng.IntN(5) {
		case 0:
			extra = `,"toolUseResult":{"stdout":"so"}`
		case 1:
			extra = `,"toolUseResult":{"file":{"filePath":"/f","numLines":2}}`
		case 2:
			extra = `,"toolUseResult":{"filePath":"/e","numFiles":3}`
		}
		return content, extra
	}

	droid := rng.IntN(4) == 0
	var lines []string
	for range rng.IntN(25) {
		switch k := rng.IntN(10); {
		case k < 4:
			id := ids[rng.IntN(len(ids))]
			msg := fmt.Sprintf(`{"id":%q%s%s}`, id, assistantContent(), usage())
			if id == "" {
				msg = fmt.Sprintf(`{"role":"assistant"%s%s}`, assistantContent(), usage())
			}
			if droid {
				lines = append(lines, fmt.Sprintf(`{"type":"message",%s"id":%q,"message":{"role":"assistant"%s}}`, tsField(), id, assistantContent()))
			} else {
				lines = append(lines, fmt.Sprintf(`{"type":"assistant",%s"message":%s}`, tsField(), msg))
			}
		case k < 8:
			content, extra := userContent()
			if droid {
				lines = append(lines, fmt.Sprintf(`{"type":"message",%s"message":{"role":"user","content":%s}}`, tsField(), content))
			} else {
				lines = append(lines, fmt.Sprintf(`{"type":"user",%s"promptId":"p1","message":{"content":%s}%s}`, tsField(), content, extra))
			}
		case k < 9:
			lines = append(lines, []string{`{"type":"progress"}`, `{"type":"system","content":"x"}`, `{broken`, ``, `{"type":"session_start"}`}[rng.IntN(5)])
		default:
			lines = append(lines, `{"role":"user","message":{"content":[{"type":"text","text":"<user_query>q</user_query>"}]}}`)
		}
	}

	out := strings.Join(lines, "\n")
	if len(lines) > 0 && rng.IntN(4) != 0 {
		out += "\n"
	}
	if droid {
		return unwrapDroidEnvelope, []byte(out)
	}
	return nil, []byte(out)
}
