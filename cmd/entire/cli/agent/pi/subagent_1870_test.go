package pi

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
)

// Tests for issue #1870: every Pi subagent transcript is named session.jsonl,
// so extractSessionIDFromPath collapsed each run to the literal "session".
// That collision keyed both the staged capture and the shadow archive on one
// ID, so runs overwrote each other and checkpoints read the wrong transcript.
//
// Fix direction 1: model Pi subagents as subagents — roll the session ID up to
// the parent UUID, give each run a distinct SubagentID, and stage each run's
// transcript at its own path (surfaced via Event.SubagentTranscriptPath).

const (
	repro1870ParentUUID = "019fab4c-c147-7b3a-8cc9-9ebd7224663b"
	repro1870ParentSeg  = "2026-07-29T00-36-01-991Z_019fab4c-c147-7b3a-8cc9-9ebd7224663b"
)

// writePiParentFile lays down a parent transcript: <ts>_<uuid>.jsonl.
func writePiParentFile(t *testing.T, root, body string) string {
	t.Helper()
	p := filepath.Join(root, "sessions", "slug", repro1870ParentSeg+".jsonl")
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// writePiSubagentFile lays down a subagent run transcript at the real Pi shape:
// <ts>_<uuid>/<sub>/run-<n>/session.jsonl.
func writePiSubagentFile(t *testing.T, root, sub, run, body string) string {
	t.Helper()
	p := filepath.Join(root, "sessions", "slug", repro1870ParentSeg, sub, run, "session.jsonl")
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func mustParsePi(t *testing.T, hook, sessionFile string) *agent.Event {
	t.Helper()
	payload := `{"type":` + strconv.Quote(hook) + `,"session_file":` + strconv.Quote(sessionFile) + `}`
	ev, err := (&PiAgent{}).ParseHookEvent(context.Background(), hook, strings.NewReader(payload))
	if err != nil {
		t.Fatalf("ParseHookEvent(%s): %v", hook, err)
	}
	if ev == nil {
		t.Fatalf("ParseHookEvent(%s): nil event", hook)
	}
	return ev
}

// subagentTranscriptRef returns the reference a checkpoint would read for a
// subagent run: the dedicated subagent transcript path when set, else the
// generic transcript reference. Under the bug the latter is the shared
// session.json; the fix populates the former with a distinct per-run path.
func subagentTranscriptRef(ev *agent.Event) string {
	if ev.SubagentTranscriptPath != "" {
		return ev.SubagentTranscriptPath
	}
	return ev.SessionRef
}

func assertFileContent(t *testing.T, path, want string) {
	t.Helper()
	if path == "" {
		t.Fatalf("no transcript path to read (want content %q)", want)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if string(got) != want {
		t.Errorf("transcript %s = %q, want %q", path, string(got), want)
	}
}

// T1 — the parse must not collapse subagent paths to the role-named leaf.
// Direction 1: a subagent transcript's session ID rolls up to the parent UUID.
func TestExtractSessionIDFromPath_SubagentRollsUpToParent(t *testing.T) {
	t.Parallel()
	base := "/Users/x/.pi/agent/sessions/slug"
	parent := base + "/" + repro1870ParentSeg + ".jsonl"
	subA := base + "/" + repro1870ParentSeg + "/d93a3451/run-0/session.jsonl"
	subB := base + "/" + repro1870ParentSeg + "/716671d6/run-2/session.jsonl"

	if got := extractSessionIDFromPath(parent); got != repro1870ParentUUID {
		t.Fatalf("parent: got %q, want %q", got, repro1870ParentUUID)
	}
	for _, p := range []string{subA, subB} {
		got := extractSessionIDFromPath(p)
		if got == "session" {
			t.Fatalf("subagent path %q collapsed to literal %q (issue #1870)", p, got)
		}
		if got != repro1870ParentUUID {
			t.Errorf("subagent path %q: got %q, want parent UUID %q", p, got, repro1870ParentUUID)
		}
	}
}

// T2 — two subagent runs must be distinguishable as subagent events: same
// (parent) session ID, distinct SubagentID, routed through the subagent
// mechanism rather than arriving as ordinary turns.
func TestParseHookEvent_PiSubagentRunsRouteAsSubagents(t *testing.T) {
	// Not parallel: agent_end capture resolves the cache dir from cwd.
	root := t.TempDir()
	t.Chdir(root)

	run0 := writePiSubagentFile(t, root, "d93a3451", "run-0", `{"run":"0"}`+"\n")
	run2 := writePiSubagentFile(t, root, "716671d6", "run-2", `{"run":"2"}`+"\n")

	startEv := mustParsePi(t, HookNameBeforeAgentStart, run0)
	if startEv.Type != agent.SubagentStart {
		t.Errorf("before_agent_start subagent: Type = %v, want SubagentStart", startEv.Type)
	}

	end0 := mustParsePi(t, HookNameAgentEnd, run0)
	end2 := mustParsePi(t, HookNameAgentEnd, run2)

	for _, ev := range []*agent.Event{end0, end2} {
		if ev.Type != agent.SubagentEnd {
			t.Errorf("agent_end subagent: Type = %v, want SubagentEnd", ev.Type)
		}
		if ev.SessionID != repro1870ParentUUID {
			t.Errorf("SessionID = %q, want parent UUID %q (rolled up)", ev.SessionID, repro1870ParentUUID)
		}
	}
	if end0.SubagentID == "" || end2.SubagentID == "" {
		t.Fatalf("subagent runs must carry a SubagentID (got %q, %q)", end0.SubagentID, end2.SubagentID)
	}
	if end0.SubagentID == end2.SubagentID {
		t.Fatalf("distinct subagent runs share SubagentID %q (issue #1870 collision)", end0.SubagentID)
	}
}

// T3 — each participant (parent + every subagent run) must stage its own
// transcript. Under the bug every run collapses onto session.json.
func TestParseHookEvent_PiSubagentCaptureDoesNotClobber(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)

	parentBody := `{"who":"parent"}` + "\n"
	run0Body := `{"who":"run0"}` + "\n"
	run2Body := `{"who":"run2"}` + "\n"

	parent := writePiParentFile(t, root, parentBody)
	run0 := writePiSubagentFile(t, root, "d93a3451", "run-0", run0Body)
	run2 := writePiSubagentFile(t, root, "716671d6", "run-2", run2Body)

	parentEv := mustParsePi(t, HookNameAgentEnd, parent)
	end0 := mustParsePi(t, HookNameAgentEnd, run0)
	end2 := mustParsePi(t, HookNameAgentEnd, run2)

	parentRef := parentEv.SessionRef
	ref0 := subagentTranscriptRef(end0)
	ref2 := subagentTranscriptRef(end2)

	refs := map[string]string{"parent": parentRef, "run0": ref0, "run2": ref2}
	for name, ref := range refs {
		if ref == "" {
			t.Fatalf("%s: empty transcript reference", name)
		}
	}
	if parentRef == ref0 || parentRef == ref2 || ref0 == ref2 {
		t.Fatalf("transcript references collide: parent=%q run0=%q run2=%q", parentRef, ref0, ref2)
	}

	assertFileContent(t, parentRef, parentBody)
	assertFileContent(t, ref0, run0Body)
	assertFileContent(t, ref2, run2Body)
}

// T4 — the checkpoint for one subagent run must keep that run's transcript,
// even after a later run ends. This is the exact corruption in #1870: two
// commits from one session, the second condensed against the last writer.
func TestParseHookEvent_PiSubagentTranscriptSurvivesLaterRun(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)

	run0Body := `{"who":"run0","commit":"first"}` + "\n"
	run2Body := `{"who":"run2","commit":"second"}` + "\n"

	// Run-0 ends and its transcript is staged for the first commit.
	run0 := writePiSubagentFile(t, root, "d93a3451", "run-0", run0Body)
	end0 := mustParsePi(t, HookNameAgentEnd, run0)
	ref0 := subagentTranscriptRef(end0)
	assertFileContent(t, ref0, run0Body)

	// Later, a different subagent run ends (the "last writer" in the issue).
	run2 := writePiSubagentFile(t, root, "716671d6", "run-2", run2Body)
	mustParsePi(t, HookNameAgentEnd, run2)

	// Run-0's staged transcript must be untouched — not the later run's content.
	assertFileContent(t, ref0, run0Body)
}
