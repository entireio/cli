package codexitem

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

func TestScriptOf(t *testing.T) {
	t.Parallel()

	argvCases := []struct {
		argv []string
		want string
	}{
		{[]string{"/bin/zsh", "-lc", "ls -la"}, "ls -la"},
		{[]string{"bash", "-c", "echo hi"}, "echo hi"},
		{[]string{"git", "status"}, "git status"},
		{[]string{"grep", "-c", "foo"}, "grep -c foo"},
		{[]string{"/usr/local/bin/fish", "-c", "ls"}, "ls"},
	}
	for _, c := range argvCases {
		if got := scriptOfArgv(c.argv); got != c.want {
			t.Errorf("scriptOfArgv(%q) = %q, want %q", c.argv, got, c.want)
		}
	}

	stringCases := []struct {
		cmd  string
		want string
	}{
		{`/bin/zsh -lc 'ls -la'`, `ls -la`},
		{`/bin/zsh -lc "sed -n '1,20p' /work/repo/README.md"`, `sed -n '1,20p' /work/repo/README.md`},
		{`/bin/zsh -lc 'echo it'"'"'s'`, `echo it's`},
		{"/bin/zsh -lc \"echo \\\"a\\\" \\$HOME \\\\ \\` x\"", "echo \"a\" $HOME \\ ` x"},
		{`/bin/bash -c echo\ hi`, `echo hi`},
		{`git status`, `git status`},
		{`grep -c foo`, `grep -c foo`},
		{`/bin/bash -c $'a\nb'`, "a\nb"},
		{`/bin/bash -lc $'cat <<EOF\nhi\tit\'s \\ \x41\101\nEOF'`, "cat <<EOF\nhi\tit's \\ AA\nEOF"},
		// As in bash, a NUL ends the quoted text; the word goes on after the quote.
		{`/bin/bash -lc $'a\0b'c`, "ac"},
		{`/bin/bash -lc $'x\x00y\'z'w`, "xw"},
		{`/bin/bash -lc pre$'x\ny'post`, "prex\nypost"},
		{`/bin/bash -lc $'\u00e9'`, `/bin/bash -lc $'\u00e9'`},
		{`/bin/bash -lc $'\cA'`, `/bin/bash -lc $'\cA'`},
		{`/bin/bash -lc $'\xZZ'`, `/bin/bash -lc $'\xZZ'`},
		{`/bin/bash -lc $'\777'`, `/bin/bash -lc $'\777'`},
		{`/bin/bash -lc $'abc`, `/bin/bash -lc $'abc`},
		{`/bin/bash -c 'echo $x'`, `echo $x`},
		{`/bin/zsh -lc 'x`, `/bin/zsh -lc 'x`},
		{`'x`, `'x`},
		{`/bin/zsh -lc "x`, `/bin/zsh -lc "x`},
	}
	// A heredoc quoted ANSI-C style by the daemon is the rollout's argv script.
	heredoc := "cat <<EOF\nhi\nEOF"
	if got, want := scriptOfString(`/bin/bash -lc $'cat <<EOF\nhi\nEOF'`), scriptOfArgv([]string{"/bin/bash", "-lc", heredoc}); got != want || got != heredoc {
		t.Errorf("ANSI-C heredoc: scriptOfString = %q, scriptOfArgv = %q, want both %q", got, want, heredoc)
	}
	for _, c := range stringCases {
		if got := scriptOfString(c.cmd); got != c.want {
			t.Errorf("scriptOfString(%q) = %q, want %q", c.cmd, got, c.want)
		}
	}
}

func TestDecodeRolloutSkips(t *testing.T) {
	t.Parallel()
	for name, line := range map[string]string{
		"not JSON":          `{"timestamp":`,
		"response_item":     `{"timestamp":"x","type":"response_item","payload":{"type":"message","role":"user","content":[]}}`,
		"other event":       `{"timestamp":"x","type":"event_msg","payload":{"type":"task_started","turn_id":"t1"}}`,
		"item not readable": `{"timestamp":"x","type":"event_msg","payload":{"type":"item_completed","turn_id":"t1","item":{"type":"CommandExecution","id":"e1","command":7}}}`,
		"no item":           `{"timestamp":"x","type":"event_msg","payload":{"type":"item_completed","turn_id":"t1"}}`,
		"item without id":   `{"timestamp":"x","type":"event_msg","payload":{"type":"item_completed","turn_id":"t1","item":{"type":"AgentMessage","content":[{"type":"Text","text":"hi"}]}}}`,
	} {
		if _, ok := DecodeRollout([]byte(line)); ok {
			t.Errorf("%s: DecodeRollout ok = true, want false", name)
		}
	}
}

func TestDecodeDaemonSkipsAndStarted(t *testing.T) {
	t.Parallel()
	if _, ok := DecodeDaemon("item/agentMessage/delta", []byte(`{"itemId":"m1","delta":"Hi","threadId":"th1","turnId":"t1"}`)); ok {
		t.Error("item/agentMessage/delta: ok = true, want false")
	}
	if _, ok := DecodeDaemon("item/completed", []byte(`{"item":`)); ok {
		t.Error("truncated params: ok = true, want false")
	}
	if _, ok := DecodeDaemon("item/completed", []byte(`{"item":{"type":"agentMessage","id":"","text":"hi"},"turnId":"t1"}`)); ok {
		t.Error("item without id: ok = true, want false")
	}

	got, ok := DecodeDaemon("item/started", []byte(`{"item":{"type":"agentMessage","id":"m1","text":""},"threadId":"th1","turnId":"t1","startedAtMs":100}`))
	if !ok {
		t.Fatal("item/started agentMessage: ok = false")
	}
	assertItem(t, got, Item{ID: "m1", TurnID: "t1", Kind: KindAgent, Status: StatusInProgress, StartedAtMs: 100})

	got, ok = DecodeDaemon("item/started", []byte(`{"item":{"type":"commandExecution","id":"e1","command":"/bin/zsh -lc 'ls'","cwd":"/w","status":"inProgress","aggregatedOutput":null,"exitCode":null},"threadId":"th1","turnId":"t1","startedAtMs":100}`))
	if !ok {
		t.Fatal("item/started commandExecution: ok = false")
	}
	assertItem(t, got, Item{ID: "e1", TurnID: "t1", Kind: KindCommand, Status: StatusInProgress, StartedAtMs: 100, Command: "ls", CWD: "/w"})
}

func intp(n int) *int { return &n }

func TestFailed(t *testing.T) {
	t.Parallel()
	cases := []struct {
		item Item
		want bool
	}{
		{Item{Status: StatusCompleted, ExitCode: intp(0)}, false},
		{Item{Status: StatusCompleted, ExitCode: intp(2)}, true},
		{Item{Status: StatusFailed}, true},
		{Item{Status: StatusDeclined}, true},
		{Item{Status: StatusCompleted, Error: "tool reported an error"}, true},
		{Item{Status: StatusInProgress}, false},
	}
	for _, c := range cases {
		if got := c.item.Failed(); got != c.want {
			t.Errorf("%+v.Failed() = %v, want %v", c.item, got, c.want)
		}
	}
}

func assertItem(t *testing.T, got, want Item) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Errorf("decoded item differs (got / want):%s", diffItems(got, want))
	}
}

// diffItems lists the fields in which a and b differ.
func diffItems(a, b Item) string {
	var out strings.Builder
	va, vb := reflect.ValueOf(a), reflect.ValueOf(b)
	for i := range va.NumField() {
		fa, fb := va.Field(i).Interface(), vb.Field(i).Interface()
		if reflect.DeepEqual(fa, fb) {
			continue
		}
		fmt.Fprintf(&out, "\n  %s: %s / %s", va.Type().Field(i).Name, show(fa), show(fb))
	}
	return out.String()
}

func show(v any) string {
	switch v := v.(type) {
	case *int:
		if v == nil {
			return "nil"
		}
		return strconv.Itoa(*v)
	case json.RawMessage:
		return string(v)
	}
	return fmt.Sprintf("%#v", v)
}
