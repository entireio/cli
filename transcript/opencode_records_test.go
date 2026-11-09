package transcript

import (
	"bytes"
	"strings"
	"testing"
)

// The fixtures below are synthetic OpenCode exports. Each sequence is one
// session exported at successive moments: a message's text grows, a tool's
// state moves from running to completed, tokens are counted once a step
// finishes, and new messages arrive.

var openCodeFoldOpts = Options{Agent: "opencode", CLIVersion: "0.1"}

// OpenCode 1.x exports.
var openCodeV1Steps = []string{
	// The first assistant message is streaming: partial text, a running tool.
	`{"info":{"id":"ses_1","directory":"/repo"},"messages":[
	 {"info":{"id":"msg_u1","sessionID":"ses_1","role":"user","time":{"created":1700000000000}},
	  "parts":[{"id":"prt_u1","type":"text","text":"Rename the helper."}]},
	 {"info":{"id":"msg_a1","sessionID":"ses_1","role":"assistant","time":{"created":1700000001000}},
	  "parts":[{"id":"prt_a1s","type":"step-start"},{"id":"prt_a1t","type":"text","text":"Let me"},
	   {"id":"prt_a1b","type":"tool","callID":"call_1","tool":"bash","state":{"status":"running","input":{"command":"ls"}}}]}
	]}`,
	// msg_a1 finished (more text, the tool completed, tokens); msg_a2 has
	// started with nothing to show yet.
	`{"info":{"id":"ses_1","directory":"/repo"},"messages":[
	 {"info":{"id":"msg_u1","sessionID":"ses_1","role":"user","time":{"created":1700000000000}},
	  "parts":[{"id":"prt_u1","type":"text","text":"Rename the helper."}]},
	 {"info":{"id":"msg_a1","sessionID":"ses_1","role":"assistant","time":{"created":1700000001000,"completed":1700000002000},"tokens":{"input":120,"output":30,"reasoning":0,"cache":{"read":0,"write":0}}},
	  "parts":[{"id":"prt_a1s","type":"step-start"},{"id":"prt_a1t","type":"text","text":"Let me look at the files."},
	   {"id":"prt_a1b","type":"tool","callID":"call_1","tool":"bash","state":{"status":"completed","input":{"command":"ls"},"output":"main.go\n"}},
	   {"id":"prt_a1f","type":"step-finish"}]},
	 {"info":{"id":"msg_a2","sessionID":"ses_1","role":"assistant","time":{"created":1700000003000}},
	  "parts":[{"id":"prt_a2s","type":"step-start"}]}
	]}`,
	// msg_a2 has content and a failed task call; a second prompt and a
	// reasoning-only reply with tokens follow.
	`{"info":{"id":"ses_1","directory":"/repo"},"messages":[
	 {"info":{"id":"msg_u1","sessionID":"ses_1","role":"user","time":{"created":1700000000000}},
	  "parts":[{"id":"prt_u1","type":"text","text":"Rename the helper."}]},
	 {"info":{"id":"msg_a1","sessionID":"ses_1","role":"assistant","time":{"created":1700000001000,"completed":1700000002000},"tokens":{"input":120,"output":30,"reasoning":0,"cache":{"read":0,"write":0}}},
	  "parts":[{"id":"prt_a1s","type":"step-start"},{"id":"prt_a1t","type":"text","text":"Let me look at the files."},
	   {"id":"prt_a1b","type":"tool","callID":"call_1","tool":"bash","state":{"status":"completed","input":{"command":"ls"},"output":"main.go\n"}},
	   {"id":"prt_a1f","type":"step-finish"}]},
	 {"info":{"id":"msg_a2","sessionID":"ses_1","role":"assistant","time":{"created":1700000003000,"completed":1700000004000},"tokens":{"input":200,"output":40}},
	  "parts":[{"id":"prt_a2s","type":"step-start"},{"id":"prt_a2t","type":"text","text":"Renamed it."},
	   {"id":"prt_a2k","type":"tool","callID":"call_2","tool":"task","state":{"status":"error","input":{"prompt":"check"},"output":"aborted","metadata":{"sessionId":"ses_child"}}}]},
	 {"info":{"id":"msg_u2","sessionID":"ses_1","role":"user","time":{"created":1700000005000}},
	  "parts":[{"id":"prt_u2","type":"text","text":"Thanks."}]},
	 {"info":{"id":"msg_a3","sessionID":"ses_1","role":"assistant","time":{"created":1700000006000},"tokens":{"input":10,"output":5}},
	  "parts":[{"id":"prt_a3r","type":"reasoning","text":"nothing to add"}]}
	]}`,
}

// OpenCode 2.x exports of the same kind of session, with an idle marker and
// a compaction.
var openCodeV2Steps = []string{
	`{"info":{"id":"ses_2","location":{"directory":"/repo"}},"messages":[
	 {"id":"msg_u1","type":"user","text":"Count the files.","files":[],"time":{"created":1700000000000}},
	 {"id":"msg_a1","type":"assistant","agent":"build","model":{"id":"mock","providerID":"mock"},"time":{"created":1700000001000},
	  "content":[{"type":"text","text":"Count"},
	   {"type":"tool","id":"call_1","name":"shell","state":{"status":"running","input":{"command":"ls | wc -l"}},"time":{"created":1700000001100}}]}
	]}`,
	`{"info":{"id":"ses_2","location":{"directory":"/repo"}},"messages":[
	 {"id":"msg_u1","type":"user","text":"Count the files.","files":[],"time":{"created":1700000000000}},
	 {"id":"msg_a1","type":"assistant","agent":"build","model":{"id":"mock","providerID":"mock"},"time":{"created":1700000001000,"completed":1700000002000},
	  "tokens":{"input":50,"output":12,"reasoning":0,"cache":{"read":0,"write":0}},
	  "content":[{"type":"text","text":"Counting them now."},
	   {"type":"tool","id":"call_1","name":"shell","state":{"status":"completed","input":{"command":"ls | wc -l"},"content":[{"type":"text","text":"3"}]},"time":{"created":1700000001100,"ran":1700000001200,"completed":1700000001300}}]},
	 {"id":"msg_i1","type":"idle","outcome":"succeeded","time":{"created":1700000002100}}
	]}`,
	`{"info":{"id":"ses_2","location":{"directory":"/repo"}},"messages":[
	 {"id":"msg_u1","type":"user","text":"Count the files.","files":[],"time":{"created":1700000000000}},
	 {"id":"msg_a1","type":"assistant","agent":"build","model":{"id":"mock","providerID":"mock"},"time":{"created":1700000001000,"completed":1700000002000},
	  "tokens":{"input":50,"output":12,"reasoning":0,"cache":{"read":0,"write":0}},
	  "content":[{"type":"text","text":"Counting them now."},
	   {"type":"tool","id":"call_1","name":"shell","state":{"status":"completed","input":{"command":"ls | wc -l"},"content":[{"type":"text","text":"3"}]},"time":{"created":1700000001100,"ran":1700000001200,"completed":1700000001300}}]},
	 {"id":"msg_i1","type":"idle","outcome":"succeeded","time":{"created":1700000002100}},
	 {"id":"msg_u2","type":"user","text":"Write it down.","files":[],"time":{"created":1700000003000}},
	 {"id":"msg_c1","type":"compaction","reason":"auto","summary":"The user counted files.","time":{"created":1700000003500},"tokens":{"input":30,"output":8}},
	 {"id":"msg_a2","type":"assistant","agent":"build","time":{"created":1700000004000},
	  "content":[{"type":"tool","id":"call_2","name":"write","state":{"status":"running","input":{"path":"count.txt","content":"3"}}}]}
	]}`,
}

// openCodeSnapshotStream is the snapshot lines of export, each followed by
// a newline, as a producer feeds them.
func openCodeSnapshotStream(t *testing.T, export string) []byte {
	t.Helper()
	lines, err := OpenCodeSnapshotLines([]byte(export))
	if err != nil {
		t.Fatalf("OpenCodeSnapshotLines: %v", err)
	}
	var out []byte
	for _, line := range lines {
		if bytes.ContainsAny(line, "\r\n") {
			t.Fatalf("snapshot line spans lines: %s", line)
		}
		out = append(out, line...)
		out = append(out, '\n')
	}
	return out
}

// requireOpenCodeFoldMatchesConvert checks that folding records equals
// Convert of export.
func requireOpenCodeFoldMatchesConvert(t *testing.T, records []Record, export string) {
	t.Helper()
	want, err := Convert([]byte(export), openCodeFoldOpts)
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	if len(want) == 0 {
		t.Fatal("Convert produced no lines: the fixture tests nothing")
	}
	got, err := Fold(records, openCodeFoldOpts)
	if err != nil {
		t.Fatalf("Fold: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("fold differs from Convert\n got:\n%s\nwant:\n%s", got, want)
	}
}

var openCodeFixtures = []struct {
	name  string
	steps []string
}{
	{"v1", openCodeV1Steps},
	{"v2", openCodeV2Steps},
}

func TestOpenCodeAdapterSingleFeed(t *testing.T) {
	t.Parallel()
	for _, fx := range openCodeFixtures {
		for i, export := range fx.steps {
			a := NewOpenCodeAdapter()
			stream := openCodeSnapshotStream(t, export)
			records := a.Feed(stream)
			if rest := a.Flush(); len(rest) != 0 {
				t.Errorf("%s step %d: Flush after whole lines returned %d records", fx.name, i, len(rest))
			}
			if want := bytes.Count(stream, []byte("\n")); a.Lines() != want {
				t.Errorf("%s step %d: Lines() = %d, want %d", fx.name, i, a.Lines(), want)
			}
			requireOpenCodeFoldMatchesConvert(t, records, export)
		}
	}
}

// Feeding each export in turn re-sends the messages that changed, and the
// fold of everything sent so far equals Convert of the latest export.
func TestOpenCodeAdapterGrowingSnapshots(t *testing.T) {
	t.Parallel()
	for _, fx := range openCodeFixtures {
		a := NewOpenCodeAdapter()
		var records []Record
		for i, export := range fx.steps {
			batch := a.Feed(openCodeSnapshotStream(t, export))
			if i > 0 && len(batch) == 0 {
				t.Fatalf("%s step %d: no records for a changed export", fx.name, i)
			}
			records = append(records, batch...)
			requireOpenCodeFoldMatchesConvert(t, records, export)
		}
	}
}

// The step-1 v1 snapshot of msg_a1 is replaced by the step-2 one, under the
// same key: the text grew and the tool completed.
func TestOpenCodeAdapterReplacesChangedMessage(t *testing.T) {
	t.Parallel()
	a := NewOpenCodeAdapter()
	a.Feed(openCodeSnapshotStream(t, openCodeV1Steps[0]))
	batch := a.Feed(openCodeSnapshotStream(t, openCodeV1Steps[1]))
	var ids []string
	for _, r := range batch {
		if r.Op != OpMessage {
			t.Errorf("record %+v: want only message records", r)
		}
		ids = append(ids, r.ID)
	}
	if got := strings.Join(ids, ","); got != "msg_a1,msg_a2" {
		t.Errorf("records for %s, want msg_a1 (changed) and msg_a2 (new), not msg_u1 (unchanged)", got)
	}
}

// A repeated, unchanged snapshot sends nothing, and neither does one whose
// changes do not show in the transcript (a step-finish part, a completion
// time).
func TestOpenCodeAdapterUnchangedSnapshotsSendNothing(t *testing.T) {
	t.Parallel()
	for _, fx := range openCodeFixtures {
		a := NewOpenCodeAdapter()
		last := fx.steps[len(fx.steps)-1]
		if len(a.Feed(openCodeSnapshotStream(t, last))) == 0 {
			t.Fatalf("%s: no records for the first feed", fx.name)
		}
		if again := a.Feed(openCodeSnapshotStream(t, last)); len(again) != 0 {
			t.Errorf("%s: repeated snapshots sent %d records: %+v", fx.name, len(again), again)
		}
		if again, err := a.FeedExport([]byte(last)); err != nil || len(again) != 0 {
			t.Errorf("%s: FeedExport of the same export sent %d records (%v)", fx.name, len(again), err)
		}
	}

	a := NewOpenCodeAdapter()
	a.Feed(openCodeSnapshotStream(t, openCodeV1Steps[2]))
	invisible := strings.Replace(openCodeV1Steps[2],
		`"time":{"created":1700000006000}`,
		`"time":{"created":1700000006000,"completed":1700000007000}`, 1)
	invisible = strings.Replace(invisible,
		`{"id":"prt_a3r","type":"reasoning","text":"nothing to add"}`,
		`{"id":"prt_a3r","type":"reasoning","text":"nothing to add"},{"id":"prt_a3f","type":"step-finish"}`, 1)
	if invisible == openCodeV1Steps[2] {
		t.Fatal("fixture edit did not apply")
	}
	if got := a.Feed(openCodeSnapshotStream(t, invisible)); len(got) != 0 {
		t.Errorf("a change that does not show sent %d records: %+v", len(got), got)
	}
}

// A message the session no longer has (a revert) is dropped by FeedExport,
// or by Drop, and restored in place when it comes back.
func TestOpenCodeAdapterDropsMessages(t *testing.T) {
	t.Parallel()

	// Revert msg_a2 and everything after it, then prompt again.
	cut := strings.Index(openCodeV1Steps[2], `{"info":{"id":"msg_a2"`)
	reverted := strings.TrimRight(openCodeV1Steps[2][:cut], " \t\n,") + "\n]}"
	reprompted := strings.TrimSuffix(reverted, "\n]}") + `,
	 {"info":{"id":"msg_u3","sessionID":"ses_1","role":"user","time":{"created":1700000009000}},
	  "parts":[{"id":"prt_u3","type":"text","text":"Try once more."}]}
	]}`

	a := NewOpenCodeAdapter()
	var records []Record
	for _, export := range []string{openCodeV1Steps[2], reverted, reprompted} {
		batch, err := a.FeedExport([]byte(export))
		if err != nil {
			t.Fatalf("FeedExport: %v", err)
		}
		records = append(records, batch...)
		requireOpenCodeFoldMatchesConvert(t, records, export)
	}
	var drops []string
	for _, r := range records {
		if r.Op == OpDrop {
			drops = append(drops, r.ID)
		}
	}
	if got := strings.Join(drops, ","); got != "msg_a2,msg_u2,msg_a3" {
		t.Errorf("drops %s, want msg_a2,msg_u2,msg_a3 in first-sent order", got)
	}

	// Drop by hand, then the message's next snapshot restores it in place,
	// although it did not change.
	b := NewOpenCodeAdapter()
	stream := openCodeSnapshotStream(t, openCodeV1Steps[2])
	records = b.Feed(stream)
	dropped := b.Drop("msg_a2")
	if len(dropped) != 1 || dropped[0].Op != OpDrop || dropped[0].ID != "msg_a2" {
		t.Fatalf("Drop = %+v, want one drop of msg_a2", dropped)
	}
	if again := b.Drop("msg_a2"); len(again) != 0 {
		t.Errorf("a second Drop = %+v, want nothing", again)
	}
	if never := b.Drop("msg_unknown"); len(never) != 0 {
		t.Errorf("Drop of an unknown message = %+v, want nothing", never)
	}
	records = append(records, dropped...)
	records = append(records, b.Feed(stream)...)
	requireOpenCodeFoldMatchesConvert(t, records, openCodeV1Steps[2])
}

// Records depend only on the bytes fed, not on how they were chunked.
func TestOpenCodeAdapterChunkedFeed(t *testing.T) {
	t.Parallel()
	for _, fx := range openCodeFixtures {
		var stream []byte
		for _, export := range fx.steps {
			stream = append(stream, openCodeSnapshotStream(t, export)...)
		}
		whole := NewOpenCodeAdapter().Feed(stream)
		for _, size := range []int{1, 7, 64} {
			a := NewOpenCodeAdapter()
			var records []Record
			for i := 0; i < len(stream); i += size {
				records = append(records, a.Feed(stream[i:min(i+size, len(stream))])...)
			}
			records = append(records, a.Flush()...)
			if len(records) != len(whole) {
				t.Errorf("%s, chunks of %d: %d records, want %d", fx.name, size, len(records), len(whole))
			}
			requireOpenCodeFoldMatchesConvert(t, records, fx.steps[len(fx.steps)-1])
		}

		// A final line without its newline is read by Flush.
		a := NewOpenCodeAdapter()
		trimmed := bytes.TrimSuffix(stream, []byte("\n"))
		records := a.Feed(trimmed)
		records = append(records, a.Flush()...)
		requireOpenCodeFoldMatchesConvert(t, records, fx.steps[len(fx.steps)-1])
	}
}

// Lines that are not messages are consumed and ignored.
func TestOpenCodeAdapterIgnoresNonMessages(t *testing.T) {
	t.Parallel()
	a := NewOpenCodeAdapter()
	got := a.Feed([]byte("\nnot json\n{\"info\":{\"role\":\"user\"},\"parts\":[]}\n" +
		`{"info":{"id":"msg_s","role":"system"},"parts":[]}` + "\n"))
	if len(got) != 0 {
		t.Errorf("records %+v, want none", got)
	}
	if a.Lines() != 4 {
		t.Errorf("Lines() = %d, want 4", a.Lines())
	}
}

func TestOpenCodeSnapshotLinesNormalizesV2(t *testing.T) {
	t.Parallel()
	lines, err := OpenCodeSnapshotLines([]byte(openCodeV2Steps[2]))
	if err != nil {
		t.Fatal(err)
	}
	// The idle marker joins msg_a1; the compaction becomes a marker and a
	// summary.
	var ids []string
	for _, line := range lines {
		msg, ok := decodeOpenCodeSnapshot(line)
		if !ok || msg.Info.Role == "" {
			t.Fatalf("line is not a 1.x-shaped message: %s", line)
		}
		ids = append(ids, msg.Info.ID)
	}
	if got := strings.Join(ids, ","); got != "msg_u1,msg_a1,msg_u2,msg_c1,msg_c1-summary,msg_a2" {
		t.Errorf("messages %s", got)
	}
	if _, err := OpenCodeSnapshotLines([]byte(`{"info":{},"messages":[`)); err == nil {
		t.Error("OpenCodeSnapshotLines of a truncated export: no error")
	}
}

// An assistant message with nothing to show yet is sent anyway (content []),
// so it keeps its place ahead of later messages until its content arrives.
func TestOpenCodeAdapterKeepsPlaceOfEmptyMessage(t *testing.T) {
	t.Parallel()
	before := `{"info":{"id":"ses_3"},"messages":[
	 {"info":{"id":"msg_a1","role":"assistant","time":{"created":1700000001000}},"parts":[{"id":"prt_1","type":"step-start"}]},
	 {"info":{"id":"msg_u2","role":"user","time":{"created":1700000002000}},"parts":[{"id":"prt_2","type":"text","text":"Still there?"}]}
	]}`
	after := strings.Replace(before, `{"id":"prt_1","type":"step-start"}`,
		`{"id":"prt_1","type":"step-start"},{"id":"prt_3","type":"text","text":"Yes."}`, 1)
	a := NewOpenCodeAdapter()
	records := a.Feed(openCodeSnapshotStream(t, before))
	requireOpenCodeFoldMatchesConvert(t, records, before)
	records = append(records, a.Feed(openCodeSnapshotStream(t, after))...)
	requireOpenCodeFoldMatchesConvert(t, records, after)
}

// withRevert is export with info.revert set to revert (JSON).
func withRevert(t *testing.T, export, revert string) string {
	t.Helper()
	out := strings.Replace(export, `"info":{"id":"ses_`, `"info":{"revert":`+revert+`,"id":"ses_`, 1)
	if out == export {
		t.Fatal("the export has no session info to set the revert in")
	}
	return out
}

// snapshotIDs is the message IDs of export's snapshot lines.
func snapshotIDs(t *testing.T, export string) string {
	t.Helper()
	lines, err := OpenCodeSnapshotLines([]byte(export))
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, line := range lines {
		msg, ok := decodeOpenCodeSnapshot(line)
		if !ok {
			t.Fatalf("not a message: %s", line)
		}
		ids = append(ids, msg.Info.ID)
	}
	return strings.Join(ids, ",")
}

// A pending revert (OpenCode's undo) hides the reverted message and every
// later one, as OpenCode shows the session; a revert of a part hides that
// message's parts from it on; a revert naming nothing hides nothing.
func TestOpenCodeSnapshotLinesHideARevert(t *testing.T) {
	t.Parallel()
	v1, v2 := openCodeV1Steps[2], openCodeV2Steps[2]
	for _, c := range []struct {
		name, export, revert, want string
	}{
		{"v1 message", v1, `{"messageID":"msg_u2","snapshot":"abc"}`, "msg_u1,msg_a1,msg_a2"},
		{"v1 first message", v1, `{"messageID":"msg_u1"}`, ""},
		{"v1 part", v1, `{"messageID":"msg_a1","partID":"prt_a1b"}`, "msg_u1,msg_a1"},
		{"v1 first part", v1, `{"messageID":"msg_a1","partID":"prt_a1s"}`, "msg_u1"},
		{"v1 unknown", v1, `{"messageID":"msg_gone"}`, "msg_u1,msg_a1,msg_a2,msg_u2,msg_a3"},
		{"v1 no message", v1, `{"snapshot":"abc"}`, "msg_u1,msg_a1,msg_a2,msg_u2,msg_a3"},
		{"v2 message", v2, `{"messageID":"msg_u2"}`, "msg_u1,msg_a1"},
		{"v2 idle after a message", v2, `{"messageID":"msg_i1"}`, "msg_u1,msg_a1"},
		{"v2 compaction", v2, `{"messageID":"msg_c1"}`, "msg_u1,msg_a1,msg_u2"},
	} {
		if got := snapshotIDs(t, withRevert(t, c.export, c.revert)); got != c.want {
			t.Errorf("%s: messages %q, want %q", c.name, got, c.want)
		}
	}
	lines, err := OpenCodeSnapshotLines([]byte(withRevert(t, v1, `{"messageID":"msg_a1","partID":"prt_a1b"}`)))
	if err != nil {
		t.Fatal(err)
	}
	msg, _ := decodeOpenCodeSnapshot(lines[1])
	if len(msg.Parts) != 2 {
		t.Errorf("msg_a1 keeps %d parts, want the 2 before the reverted one: %s", len(msg.Parts), lines[1])
	}
}

// Undo then redo while streaming: the undo drops the reverted messages,
// the redo (the revert cleared) brings them back in place, and the fold
// equals Convert of what OpenCode shows each time.
func TestOpenCodeAdapterFollowsUndoAndRedo(t *testing.T) {
	t.Parallel()
	full := openCodeV1Steps[2]
	// What OpenCode shows after the undo: full without msg_u2 and msg_a3.
	cut := strings.Index(full, `{"info":{"id":"msg_u2"`)
	if cut < 0 {
		t.Fatal("the fixture has no msg_u2")
	}
	shown := strings.TrimRight(full[:cut], ", \n\t") + "\n\t]}"
	a := NewOpenCodeAdapter()
	var records []Record
	feed := func(export string) []Record {
		t.Helper()
		got, err := a.FeedExport([]byte(export))
		if err != nil {
			t.Fatal(err)
		}
		records = append(records, got...)
		return got
	}
	feed(full)
	undone := withRevert(t, full, `{"messageID":"msg_u2"}`)
	drops := 0
	for _, r := range feed(undone) {
		if r.Op == OpDrop {
			drops++
		}
	}
	if drops != 2 {
		t.Fatalf("the undo dropped %d messages, want 2 (msg_u2, msg_a3)", drops)
	}
	requireOpenCodeFoldMatchesConvert(t, records, shown)
	feed(full) // redo
	requireOpenCodeFoldMatchesConvert(t, records, full)
}
