package transcript

import (
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
)

var testMeta = Options{Agent: "claude-code", CLIVersion: "0.5.1"}

const linePrefix = `{"v":1,"agent":"claude-code","cli_version":"0.5.1",`

func rawMsg(s string) json.RawMessage { return json.RawMessage(s) }

func assistantRecord(id, sourceID, content string) Record {
	return Record{Op: OpMessage, ID: id, SourceID: sourceID, Role: TypeAssistant, TS: rawMsg(`"t1"`), Content: rawMsg(content)}
}

func foldLines(t *testing.T, records ...Record) []string {
	t.Helper()
	out, err := Fold(records, testMeta)
	if err != nil {
		t.Fatalf("Fold: %v", err)
	}
	return strings.Split(strings.TrimSuffix(string(out), "\n"), "\n")
}

func assertLines(t *testing.T, got []string, want ...string) {
	t.Helper()
	if len(want) == 0 {
		want = []string{""}
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("lines differ\ngot:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestFold_Message(t *testing.T) {
	t.Parallel()

	got := foldLines(t,
		Record{Op: OpMessage, ID: "@L1", Role: TypeUser, TS: rawMsg(`"t0"`), Content: rawMsg(`[{"text":"hi"}]`)},
		Record{Op: OpMessage, ID: "m1", SourceID: "m1", Role: TypeAssistant, TS: rawMsg(`"t1"`), Content: rawMsg(`[{"type":"text","text":"yo"}]`), InputTokens: 3},
	)
	assertLines(t, got,
		linePrefix+`"type":"user","ts":"t0","content":[{"text":"hi"}]}`,
		linePrefix+`"type":"assistant","ts":"t1","id":"m1","input_tokens":3,"content":[{"type":"text","text":"yo"}]}`,
	)
}

func TestFold_MessageReplacesKnownIDInPlace(t *testing.T) {
	t.Parallel()

	got := foldLines(t,
		assistantRecord("a", "a", `[{"type":"text","text":"old"}]`),
		Record{Op: OpMessage, ID: "u", Role: TypeUser, Content: rawMsg(`[{"text":"q"}]`)},
		Record{Op: OpMessage, ID: "a", SourceID: "a2", Role: TypeAssistant, TS: rawMsg(`"t9"`), Content: rawMsg(`[{"type":"text","text":"new"}]`)},
	)
	assertLines(t, got,
		linePrefix+`"type":"assistant","ts":"t9","id":"a2","content":[{"type":"text","text":"new"}]}`,
		linePrefix+`"type":"user","content":[{"text":"q"}]}`,
	)
}

func TestFold_AppendConcatenatesAndLaterTSWins(t *testing.T) {
	t.Parallel()

	got := foldLines(t,
		assistantRecord("a", "a", `[{"type":"text","text":"1"}]`),
		Record{Op: OpAppend, ID: "a", TS: rawMsg(`"t2"`), Content: rawMsg(`[{"type":"text","text":"2"}]`), OutputTokens: 5},
		Record{Op: OpAppend, ID: "a", TS: rawMsg(`"t3"`), Content: rawMsg(`[{"type":"text","text":"3"}]`)},
	)
	assertLines(t, got,
		linePrefix+`"type":"assistant","ts":"t3","id":"a","output_tokens":5,"content":[{"type":"text","text":"1"},{"type":"text","text":"2"},{"type":"text","text":"3"}]}`,
	)
}

func TestFold_AppendOfNothingToNullContentWritesEmptyArray(t *testing.T) {
	t.Parallel()

	got := foldLines(t,
		Record{Op: OpMessage, ID: "u", Role: TypeUser, Content: rawMsg(`null`)},
		Record{Op: OpAppend, ID: "u", Content: rawMsg(`[]`)},
	)
	assertLines(t, got,
		linePrefix+`"type":"user","content":[]}`,
	)
}

func TestFold_AppendWithoutTSClearsIt(t *testing.T) {
	t.Parallel()

	got := foldLines(t,
		assistantRecord("a", "a", `[{"type":"text","text":"1"}]`),
		Record{Op: OpAppend, ID: "a", Content: rawMsg(`[{"type":"text","text":"2"}]`)},
	)
	assertLines(t, got,
		linePrefix+`"type":"assistant","id":"a","content":[{"type":"text","text":"1"},{"type":"text","text":"2"}]}`,
	)
}

func TestFold_Result(t *testing.T) {
	t.Parallel()

	content := `[{"type":"tool_use","id":"tu1","name":"A","input":{}},{"type":"tool_use","id":"tu2","name":"B","input":{}},{"type":"text","text":"x"}]`
	tests := []struct {
		name      string
		toolUseID string
		want      string
	}{
		{"matching id", "tu1", `[{"id":"tu1","input":{},"name":"A","result":{"output":"o","status":"success"},"type":"tool_use"},{"id":"tu2","input":{},"name":"B","type":"tool_use"},{"text":"x","type":"text"}]`},
		{"empty id targets last tool_use", "", `[{"id":"tu1","input":{},"name":"A","type":"tool_use"},{"id":"tu2","input":{},"name":"B","result":{"output":"o","status":"success"},"type":"tool_use"},{"text":"x","type":"text"}]`},
		{"no match is ignored", "tu9", content},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := foldLines(t,
				assistantRecord("a", "", content),
				Record{Op: OpResult, ID: "a", ToolUseID: tc.toolUseID, Result: &ToolResult{Output: "o", Status: ToolStatusSuccess}},
			)
			assertLines(t, got, linePrefix+`"type":"assistant","ts":"t1","content":`+tc.want+`}`)
		})
	}
}

func TestFold_ResultTargetsLastToolUseWithID(t *testing.T) {
	t.Parallel()

	got := foldLines(t,
		assistantRecord("a", "", `[{"type":"tool_use","id":"tu1","name":"A","input":{}}]`),
		Record{Op: OpAppend, ID: "a", TS: rawMsg(`"t1"`), Content: rawMsg(`[{"type":"tool_use","id":"tu1","name":"B","input":{}}]`)},
		Record{Op: OpResult, ID: "a", ToolUseID: "tu1", Result: &ToolResult{Output: "o", Status: ToolStatusSuccess}},
	)
	assertLines(t, got, linePrefix+`"type":"assistant","ts":"t1","content":[{"id":"tu1","input":{},"name":"A","type":"tool_use"},{"id":"tu1","input":{},"name":"B","result":{"output":"o","status":"success"},"type":"tool_use"}]}`)
}

// Results that match nothing leave the content's key order as given, even
// after the fold has split it to look for a match.
func TestFold_UnmatchedResultsKeepKeyOrder(t *testing.T) {
	t.Parallel()

	content := `[{"type":"tool_use","id":"tu1","name":"A","input":{}}]`
	got := foldLines(t,
		assistantRecord("a", "", content),
		Record{Op: OpResult, ID: "a", ToolUseID: "tu8", Result: &ToolResult{Output: "o"}},
		Record{Op: OpResult, ID: "a", ToolUseID: "tu9", Result: &ToolResult{Output: "o"}},
	)
	assertLines(t, got, linePrefix+`"type":"assistant","ts":"t1","content":`+content+`}`)
}

// A message record replaces whatever the fold learned from earlier content
// while looking for a result's target.
func TestFold_ResultAfterRestatementSeesNewContentOnly(t *testing.T) {
	t.Parallel()

	got := foldLines(t,
		assistantRecord("a", "", `[{"type":"tool_use","id":"tu1","name":"A","input":{}}]`),
		Record{Op: OpResult, ID: "a", ToolUseID: "tu9", Result: &ToolResult{Output: "o"}},
		assistantRecord("a", "", `[{"type":"text","text":"x"},{"type":"tool_use","id":"tu2","name":"B","input":{}}]`),
		Record{Op: OpResult, ID: "a", ToolUseID: "tu1", Result: &ToolResult{Output: "stale"}},
		Record{Op: OpResult, ID: "a", ToolUseID: "tu2", Result: &ToolResult{Output: "o", Status: ToolStatusSuccess}},
	)
	assertLines(t, got, linePrefix+`"type":"assistant","ts":"t1","content":[{"text":"x","type":"text"},{"id":"tu2","input":{},"name":"B","result":{"output":"o","status":"success"},"type":"tool_use"}]}`)
}

// Only blocks present when a result is attached are written with sorted keys.
func TestFold_ResultSortsKeysOfBlocksPresentOnly(t *testing.T) {
	t.Parallel()

	result := &ToolResult{Output: "o", Status: ToolStatusSuccess}
	records := []Record{
		assistantRecord("a", "", `[{"type":"tool_use","id":"tu1","name":"A","input":{}}]`),
		{Op: OpResult, ID: "a", ToolUseID: "tu1", Result: result},
		{Op: OpAppend, ID: "a", TS: rawMsg(`"t1"`), Content: rawMsg(`[{"type":"text","text":"x"}]`)},
	}
	assertLines(t, foldLines(t, records...), linePrefix+`"type":"assistant","ts":"t1","content":[{"id":"tu1","input":{},"name":"A","result":{"output":"o","status":"success"},"type":"tool_use"},{"type":"text","text":"x"}]}`)

	records = append(records, Record{Op: OpResult, ID: "a", ToolUseID: "tu1", Result: result})
	assertLines(t, foldLines(t, records...), linePrefix+`"type":"assistant","ts":"t1","content":[{"id":"tu1","input":{},"name":"A","result":{"output":"o","status":"success"},"type":"tool_use"},{"text":"x","type":"text"}]}`)
}

func TestFold_ResultOnNonArrayContentIsIgnored(t *testing.T) {
	t.Parallel()

	got := foldLines(t,
		assistantRecord("a", "a", `"just text"`),
		Record{Op: OpResult, ID: "a", ToolUseID: "tu1", Result: &ToolResult{Output: "o", Status: ToolStatusError}},
	)
	assertLines(t, got, linePrefix+`"type":"assistant","ts":"t1","id":"a","content":"just text"}`)
}

func TestFold_UsageNonZeroReplaces(t *testing.T) {
	t.Parallel()

	got := foldLines(t,
		Record{Op: OpMessage, ID: "a", SourceID: "a", Role: TypeAssistant, Content: rawMsg(`[{"type":"text","text":"x"}]`), InputTokens: 10, OutputTokens: 1},
		Record{Op: OpUsage, ID: "a", OutputTokens: 7},
		Record{Op: OpUsage, ID: "a"},
	)
	assertLines(t, got, linePrefix+`"type":"assistant","id":"a","input_tokens":10,"output_tokens":7,"content":[{"type":"text","text":"x"}]}`)
}

func TestFold_DropAndRestore(t *testing.T) {
	t.Parallel()

	first := Record{Op: OpMessage, ID: "u1", Role: TypeUser, Content: rawMsg(`[{"text":"one"}]`)}
	second := Record{Op: OpMessage, ID: "u2", Role: TypeUser, Content: rawMsg(`[{"text":"two"}]`)}

	got := foldLines(t, first, second, Record{Op: OpDrop, ID: "u1"})
	assertLines(t, got, linePrefix+`"type":"user","content":[{"text":"two"}]}`)

	// Re-sending the message restores it at its original position.
	got = foldLines(t, first, second, Record{Op: OpDrop, ID: "u1"}, first)
	assertLines(t, got,
		linePrefix+`"type":"user","content":[{"text":"one"}]}`,
		linePrefix+`"type":"user","content":[{"text":"two"}]}`,
	)
}

func TestFold_SkipsEmptyAssistantsOnly(t *testing.T) {
	t.Parallel()

	got := foldLines(t,
		assistantRecord("a", "a", `[]`),
		Record{Op: OpMessage, ID: "u", Role: TypeUser, Content: rawMsg(`[]`)},
		assistantRecord("n", "n", `null`),
		Record{Op: OpMessage, ID: "nil", Role: TypeAssistant},
	)
	// A missing content prints null; a literal null counts as empty, as in compaction.
	assertLines(t, got,
		linePrefix+`"type":"user","content":[]}`,
		linePrefix+`"type":"assistant","content":null}`,
	)
}

func TestFold_DerivedIDsAreOmitted(t *testing.T) {
	t.Parallel()

	got := foldLines(t,
		assistantRecord("m1", "m1", `[{"type":"text","text":"a"}]`),
		assistantRecord("m1@L3", "m1", `[{"type":"text","text":"b"}]`),
		assistantRecord("@L4", "", `[{"type":"text","text":"c"}]`),
	)
	assertLines(t, got,
		linePrefix+`"type":"assistant","ts":"t1","id":"m1","content":[{"type":"text","text":"a"}]}`,
		linePrefix+`"type":"assistant","ts":"t1","id":"m1","content":[{"type":"text","text":"b"}]}`,
		linePrefix+`"type":"assistant","ts":"t1","content":[{"type":"text","text":"c"}]}`,
	)
}

func TestFolder_ApplyReportsTouchedIDsAndLines(t *testing.T) {
	t.Parallel()

	f := NewFolder(testMeta)
	touched, err := f.Apply([]Record{
		assistantRecord("a", "a", `[]`),
		{Op: OpMessage, ID: "u", Role: TypeUser, Content: rawMsg(`[{"text":"q"}]`)},
		{Op: OpAppend, ID: "a", Content: rawMsg(`[{"type":"text","text":"x"}]`)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"a", "u"}; !reflect.DeepEqual(touched, want) {
		t.Errorf("touched = %v, want %v", touched, want)
	}

	line, visible := f.Line("a")
	if !visible || string(line) != linePrefix+`"type":"assistant","id":"a","content":[{"type":"text","text":"x"}]}` {
		t.Errorf("Line(a) = %s, %v", line, visible)
	}
	if _, visible := f.Line("missing"); visible {
		t.Error("Line(missing) is visible")
	}

	touched, err = f.Apply([]Record{{Op: OpDrop, ID: "u"}})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(touched, []string{"u"}) {
		t.Errorf("touched = %v, want [u]", touched)
	}
	if _, visible := f.Line("u"); visible {
		t.Error("dropped message is visible")
	}
	if got := string(f.Lines()); got != linePrefix+`"type":"assistant","id":"a","content":[{"type":"text","text":"x"}]}`+"\n" {
		t.Errorf("Lines() = %s", got)
	}
}

// A streaming consumer decodes every record into the same variable, and
// json.RawMessage reuses its buffer. The Folder must not keep those buffers.
func TestFolder_ApplyDoesNotRetainRecordBuffers(t *testing.T) {
	t.Parallel()

	stream := `{"op":"message","id":"m1","role":"user","ts":"t1","content":[{"text":"one"}]}
{"op":"message","id":"m2","role":"user","ts":"t2","content":[{"text":"two"}]}
{"op":"append","id":"m2","ts":"t3","content":[]}
{"op":"message","id":"m3","role":"user","ts":"t4","content":[{"text":"xxx"}]}
`
	f := NewFolder(testMeta)
	dec := json.NewDecoder(strings.NewReader(stream))
	var r Record
	for {
		err := dec.Decode(&r)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.Apply([]Record{r}); err != nil {
			t.Fatal(err)
		}
	}

	got := strings.Split(strings.TrimSuffix(string(f.Lines()), "\n"), "\n")
	assertLines(t, got,
		linePrefix+`"type":"user","ts":"t1","content":[{"text":"one"}]}`,
		linePrefix+`"type":"user","ts":"t3","content":[{"text":"two"}]}`,
		linePrefix+`"type":"user","ts":"t4","content":[{"text":"xxx"}]}`,
	)
}

// Empty content and timestamps mean absent, as they do once a Record is
// encoded. Empty content used to fold to a line that could not be encoded,
// so the message vanished without an error.
func TestFolder_EmptyRawFieldsMeanAbsent(t *testing.T) {
	t.Parallel()

	f := NewFolder(testMeta)
	if _, err := f.Apply([]Record{{Op: OpMessage, ID: "u", Role: TypeUser, TS: rawMsg(`"t1"`), Content: rawMsg(`[{"text":"a"}]`)}}); err != nil {
		t.Fatal(err)
	}
	batch := []Record{
		{Op: OpMessage, ID: "u", Role: TypeUser, TS: json.RawMessage{}, Content: json.RawMessage{}},
		{Op: OpMessage, ID: "v", Role: TypeUser, TS: rawMsg(`"t2"`), Content: rawMsg(`[{"text":"b"}]`)},
	}
	touched, err := f.Apply(batch)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !reflect.DeepEqual(touched, []string{"u", "v"}) {
		t.Errorf("touched = %v, want [u v]", touched)
	}
	got := strings.Split(strings.TrimSuffix(string(f.Lines()), "\n"), "\n")
	assertLines(t, got,
		linePrefix+`"type":"user","content":null}`,
		linePrefix+`"type":"user","ts":"t2","content":[{"text":"b"}]}`,
	)

	// Folding the same records after a JSON round trip gives the same lines.
	data, err := json.Marshal(batch)
	if err != nil {
		t.Fatal(err)
	}
	var decoded []Record
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	want, err := Fold(batch, testMeta)
	if err != nil {
		t.Fatal(err)
	}
	gotRoundTrip, err := Fold(decoded, testMeta)
	if err != nil {
		t.Fatal(err)
	}
	if string(gotRoundTrip) != string(want) {
		t.Errorf("fold differs after round trip\ngot:  %s\nwant: %s", gotRoundTrip, want)
	}
}

// A message record restates a message completely, even after appends and
// results decoded its content.
func TestFold_MessageReplacesDecodedContent(t *testing.T) {
	t.Parallel()

	got := foldLines(t,
		assistantRecord("a", "a", `[{"type":"tool_use","id":"tu1","name":"Bash","input":{}}]`),
		Record{Op: OpAppend, ID: "a", Content: rawMsg(`[{"type":"text","text":"x"}]`)},
		Record{Op: OpResult, ID: "a", ToolUseID: "tu1", Result: &ToolResult{Output: "o", Status: ToolStatusSuccess}},
		assistantRecord("a", "a", `[{"type":"text","text":"fresh"}]`),
	)
	assertLines(t, got,
		linePrefix+`"type":"assistant","ts":"t1","id":"a","content":[{"type":"text","text":"fresh"}]}`,
	)
}

// The fold keeps its own copy of a result: the caller may reuse it.
func TestFold_ResultIsCopied(t *testing.T) {
	t.Parallel()

	f := NewFolder(testMeta)
	result := &ToolResult{Output: "first", Status: ToolStatusSuccess, File: &ToolResultFile{FilePath: "/a"}}
	if _, err := f.Apply([]Record{
		assistantRecord("a", "a", `[{"type":"tool_use","id":"tu1","name":"Read","input":{}}]`),
		{Op: OpResult, ID: "a", ToolUseID: "tu1", Result: result},
	}); err != nil {
		t.Fatal(err)
	}
	result.Output = "changed"
	result.File.FilePath = "/b"
	line, ok := f.Line("a")
	if !ok {
		t.Fatal("line not visible")
	}
	assertLines(t, []string{string(line)},
		linePrefix+`"type":"assistant","ts":"t1","id":"a","content":[{"id":"tu1","input":{},"name":"Read","result":{"output":"first","status":"success","file":{"filePath":"/a"}},"type":"tool_use"}]}`,
	)
}

func TestFolder_ApplyRejectsInvalidBatchAtomically(t *testing.T) {
	t.Parallel()

	ok := Record{Op: OpMessage, ID: "u", Role: TypeUser, Content: rawMsg(`[{"text":"q"}]`)}
	tests := []struct {
		name string
		bad  Record
	}{
		{"unknown op", Record{Op: "rename", ID: "u"}},
		{"missing id", Record{Op: OpDrop}},
		{"bad role", Record{Op: OpMessage, ID: "x", Role: "system"}},
		{"unknown target", Record{Op: OpAppend, ID: "nope"}},
		{"result without result", Record{Op: OpResult, ID: "u", ToolUseID: "t"}},
		{"invalid content", Record{Op: OpMessage, ID: "x", Role: TypeUser, Content: rawMsg(`[`)}},
		{"invalid ts", Record{Op: OpAppend, ID: "u", TS: rawMsg(`t1`)}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := NewFolder(testMeta)
			if _, err := f.Apply([]Record{ok, tc.bad}); err == nil {
				t.Fatal("Apply accepted an invalid record")
			}
			if got := f.Lines(); len(got) != 0 {
				t.Errorf("failed Apply changed the folder: %s", got)
			}
		})
	}
}

func TestRecord_JSONRoundTrip(t *testing.T) {
	t.Parallel()

	records := []Record{
		{Op: OpMessage, ID: "m1", SourceID: "m1", Role: TypeAssistant, TS: rawMsg(`"2026-01-01T00:00:00Z"`), Content: rawMsg(`[{"type":"text","text":"a <b> & c"}]`), InputTokens: 1, OutputTokens: 2},
		{Op: OpMessage, ID: "@L2", Role: TypeUser, TS: rawMsg(`null`), Content: rawMsg(`null`)},
		{Op: OpMessage, ID: "@L3", Role: TypeAssistant},
		{Op: OpAppend, ID: "m1", TS: rawMsg(`1700000000`), Content: rawMsg(`[{"type":"tool_use","id":"tu1","name":"Bash","input":{}}]`), OutputTokens: 9},
		{Op: OpResult, ID: "m1", ToolUseID: "tu1", Result: &ToolResult{Output: "out", Status: ToolStatusError, File: &ToolResultFile{FilePath: "/f", NumLines: 3}, MatchCount: 2}},
		{Op: OpResult, ID: "m1", Result: &ToolResult{Status: ToolStatusSuccess}},
		{Op: OpUsage, ID: "m1", InputTokens: 4},
		{Op: OpDrop, ID: "@L2"},
	}

	data, err := json.Marshal(records)
	if err != nil {
		t.Fatal(err)
	}
	var decoded []Record
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	reencoded, err := json.Marshal(decoded)
	if err != nil {
		t.Fatal(err)
	}
	if string(reencoded) != string(data) {
		t.Errorf("round trip changed encoding\nbefore: %s\nafter:  %s", data, reencoded)
	}

	want, err := Fold(records, testMeta)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Fold(decoded, testMeta)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Errorf("fold of decoded records differs\ngot:  %s\nwant: %s", got, want)
	}

	// The wire names are part of the protocol.
	const wantFirst = `{"op":"message","id":"m1","sourceId":"m1","role":"assistant","ts":"2026-01-01T00:00:00Z","content":[{"type":"text","text":"a \u003cb\u003e \u0026 c"}],"inputTokens":1,"outputTokens":2}`
	if first, err := json.Marshal(records[0]); err != nil || string(first) != wantFirst {
		t.Errorf("message record encoding = %s (%v), want %s", first, err, wantFirst)
	}
	const wantResult = `{"op":"result","id":"m1","toolUseId":"tu1","result":{"output":"out","status":"error","file":{"filePath":"/f","numLines":3},"matchCount":2}}`
	if result, err := json.Marshal(records[4]); err != nil || string(result) != wantResult {
		t.Errorf("result record encoding = %s (%v), want %s", result, err, wantResult)
	}
}
