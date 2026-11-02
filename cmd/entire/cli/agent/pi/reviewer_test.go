package pi

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/review"
	reviewtypes "github.com/entireio/cli/cmd/entire/cli/review/types"
)

var _ reviewtypes.AgentReviewer = (*reviewtypes.ReviewerTemplate)(nil)

func TestPiReviewer_NameMatchesRegistryKey(t *testing.T) {
	t.Parallel()
	if got := NewReviewer().Name(); got != string(agent.AgentNamePi) {
		t.Fatalf("Name() = %q, want %q", got, agent.AgentNamePi)
	}
}

func TestPiReviewer_BuildCmd(t *testing.T) {
	t.Parallel()
	cfg := reviewtypes.RunConfig{
		Model:        "anthropic/claude-sonnet-4-5:high",
		Task:         "Review the change.",
		AlwaysPrompt: "Focus on API regressions.",
		StartingSHA:  "abc123",
	}
	cmd := buildPiReviewCmd(context.Background(), cfg)

	if cmd.Args[0] != "pi" {
		t.Fatalf("Args[0] = %q, want pi; args=%v", cmd.Args[0], cmd.Args)
	}
	wantPrefix := []string{"pi", "--mode", "json", "--print", "--append-system-prompt", review.ReviewerGuardrail, "--model", "anthropic/claude-sonnet-4-5:high"}
	if len(cmd.Args) != len(wantPrefix) {
		t.Fatalf("args len = %d, want %d (prompt must not be in argv): %v", len(cmd.Args), len(wantPrefix), cmd.Args)
	}
	for i, want := range wantPrefix {
		if cmd.Args[i] != want {
			t.Fatalf("Args[%d] = %q, want %q; args=%v", i, cmd.Args[i], want, cmd.Args)
		}
	}
	if cmd.Stdin == nil {
		t.Fatal("cmd.Stdin is nil, want the composed prompt")
	}
	stdin, err := io.ReadAll(cmd.Stdin)
	if err != nil {
		t.Fatalf("read stdin: %v", err)
	}
	if prompt := string(stdin); !strings.Contains(prompt, "Review the change.") || !strings.Contains(prompt, "Focus on API regressions.") {
		t.Fatalf("stdin missing composed review content: %q", prompt)
	}

	env := envMap(cmd.Env)
	if env[review.EnvSession] != "1" {
		t.Errorf("%s = %q, want 1", review.EnvSession, env[review.EnvSession])
	}
	if env[review.EnvAgent] != string(agent.AgentNamePi) {
		t.Errorf("%s = %q, want %q", review.EnvAgent, env[review.EnvAgent], agent.AgentNamePi)
	}
	if env[review.EnvStartingSHA] != "abc123" {
		t.Errorf("%s = %q, want abc123", review.EnvStartingSHA, env[review.EnvStartingSHA])
	}
}

func TestPiReviewer_ParseJSONEventStream(t *testing.T) {
	t.Parallel()
	input := strings.Join([]string{
		`{"type":"session","version":3,"id":"s1","cwd":"/repo"}`,
		`{"type":"agent_start"}`,
		`{"type":"turn_start"}`,
		`{"type":"message_update","message":{"role":"assistant"},"assistantMessageEvent":{"type":"text_delta","delta":"Finding "}}`,
		`{"type":"tool_execution_start","toolName":"bash","args":{"command":"git diff --stat"}}`,
		`{"type":"message_update","message":{"role":"assistant"},"assistantMessageEvent":{"type":"text_delta","delta":"one"}}`,
		`{"type":"message_end","message":{"role":"assistant","usage":{"input":10,"output":4,"cacheRead":2,"cacheWrite":3},"stopReason":"stop"}}`,
		`{"type":"agent_end","messages":[]}`,
	}, "\n")

	events := collectPiReviewEvents(input)
	if len(events) != 6 {
		t.Fatalf("events len = %d, want 6: %#v", len(events), events)
	}
	if _, ok := events[0].(reviewtypes.Started); !ok {
		t.Fatalf("events[0] = %T, want Started", events[0])
	}
	if got, ok := events[1].(reviewtypes.AssistantText); !ok || got.Text != "Finding " || got.Delta {
		t.Fatalf("events[1] = %#v, want AssistantText{Finding } starting a message", events[1])
	}
	tool, ok := events[2].(reviewtypes.ToolCall)
	if !ok || tool.Name != "bash" || !strings.Contains(tool.Args, "git diff --stat") {
		t.Fatalf("events[2] = %#v, want ToolCall(bash)", events[2])
	}
	if got, ok := events[3].(reviewtypes.AssistantText); !ok || got.Text != "one" || !got.Delta {
		t.Fatalf("events[3] = %#v, want AssistantText{one} continuing the message", events[3])
	}
	tokens, ok := events[4].(reviewtypes.Tokens)
	if !ok || tokens.In != 10 || tokens.Out != 4 {
		t.Fatalf("events[4] = %#v, want Tokens{In:10 Out:4}", events[4])
	}
	finished, ok := events[5].(reviewtypes.Finished)
	if !ok || !finished.Success {
		t.Fatalf("events[5] = %#v, want Finished{Success:true}", events[5])
	}
}

// A delta continues the previous text only within one message, so separate
// messages and retry attempts never join into one line.
func TestPiReviewer_ParseDeltaStartsEachMessageAndRetry(t *testing.T) {
	t.Parallel()
	input := strings.Join([]string{
		`{"type":"agent_start"}`,
		`{"type":"message_update","message":{"role":"assistant"},"assistantMessageEvent":{"type":"text_delta","delta":"I'll read "}}`,
		`{"type":"message_update","message":{"role":"assistant"},"assistantMessageEvent":{"type":"text_delta","delta":"the diff."}}`,
		`{"type":"message_end","message":{"role":"assistant","stopReason":"toolUse"}}`,
		`{"type":"tool_execution_start","toolName":"bash","args":{"command":"git diff"}}`,
		`{"type":"message_update","message":{"role":"assistant"},"assistantMessageEvent":{"type":"text_delta","delta":"request changes"}}`,
		`{"type":"auto_retry_start"}`,
		`{"type":"message_update","message":{"role":"assistant"},"assistantMessageEvent":{"type":"text_delta","delta":"approve"}}`,
		`{"type":"message_end","message":{"role":"assistant","stopReason":"stop"}}`,
		`{"type":"agent_end"}`,
	}, "\n")

	var texts []reviewtypes.AssistantText
	for _, ev := range collectPiReviewEvents(input) {
		if text, ok := ev.(reviewtypes.AssistantText); ok {
			texts = append(texts, text)
		}
	}
	want := []reviewtypes.AssistantText{
		{Text: "I'll read "},
		{Text: "the diff.", Delta: true},
		{Text: "request changes"},
		{Text: "approve"},
	}
	if len(texts) != len(want) {
		t.Fatalf("texts = %#v, want %#v", texts, want)
	}
	for i := range want {
		if texts[i] != want[i] {
			t.Errorf("texts[%d] = %#v, want %#v", i, texts[i], want[i])
		}
	}
}

func TestPiReviewer_ParseMessageEndTextWithoutDeltas(t *testing.T) {
	t.Parallel()
	input := strings.Join([]string{
		`{"type":"agent_start"}`,
		`{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"Final review text"}],"stopReason":"stop"}}`,
		`{"type":"agent_end"}`,
	}, "\n")

	events := collectPiReviewEvents(input)
	var found bool
	for _, ev := range events {
		text, ok := ev.(reviewtypes.AssistantText)
		if ok && text.Text == "Final review text" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected AssistantText from message_end content, got %#v", events)
	}
}

func TestPiReviewer_ParseTokensAreCumulative(t *testing.T) {
	t.Parallel()
	input := strings.Join([]string{
		`{"type":"agent_start"}`,
		`{"type":"message_end","id":"m1","message":{"id":"m1","role":"assistant","usage":{"input":100,"output":50,"cacheRead":10,"cacheWrite":5},"stopReason":"toolUse"}}`,
		`{"type":"message_end","id":"m2","message":{"id":"m2","role":"assistant","usage":{"input":200,"output":30,"cacheRead":0,"cacheWrite":0},"stopReason":"stop"}}`,
		`{"type":"agent_end"}`,
	}, "\n")

	events := collectPiReviewEvents(input)
	var tokens []reviewtypes.Tokens
	for _, ev := range events {
		if tok, ok := ev.(reviewtypes.Tokens); ok {
			tokens = append(tokens, tok)
		}
	}
	if len(tokens) != 2 {
		t.Fatalf("token events = %d, want 2: %#v", len(tokens), events)
	}
	if got := tokens[0]; got.In != 100 || got.Out != 50 {
		t.Fatalf("first Tokens = %#v, want In=100 Out=50", got)
	}
	if got := tokens[1]; got.In != 300 || got.Out != 80 {
		t.Fatalf("final Tokens = %#v, want In=300 Out=80", got)
	}
}

func TestPiReviewer_ParseTokensDedupesTurnEndForSameMessage(t *testing.T) {
	t.Parallel()
	input := strings.Join([]string{
		`{"type":"agent_start"}`,
		`{"type":"message_end","id":"m1","message":{"id":"m1","role":"assistant","usage":{"input":10,"output":5},"stopReason":"stop"}}`,
		`{"type":"turn_end","id":"m1","message":{"id":"m1","role":"assistant","usage":{"input":10,"output":5},"stopReason":"stop"}}`,
		`{"type":"agent_end"}`,
	}, "\n")

	events := collectPiReviewEvents(input)
	var tokens []reviewtypes.Tokens
	for _, ev := range events {
		if tok, ok := ev.(reviewtypes.Tokens); ok {
			tokens = append(tokens, tok)
		}
	}
	if len(tokens) != 1 {
		t.Fatalf("token events = %d, want 1: %#v", len(tokens), events)
	}
	if got := tokens[0]; got.In != 10 || got.Out != 5 {
		t.Fatalf("Tokens = %#v, want In=10 Out=5", got)
	}
}

func TestPiReviewer_ParseTokensDedupesNoIDTurnEndForSameUsage(t *testing.T) {
	t.Parallel()
	input := strings.Join([]string{
		`{"type":"agent_start"}`,
		`{"type":"turn_start"}`,
		`{"type":"message_end","message":{"role":"assistant","usage":{"input":10,"output":5,"cacheRead":2,"cacheWrite":1},"stopReason":"stop"}}`,
		`{"type":"turn_end","message":{"role":"assistant","usage":{"input":10,"output":5,"cacheRead":2,"cacheWrite":1},"stopReason":"stop"}}`,
		`{"type":"agent_end"}`,
	}, "\n")

	events := collectPiReviewEvents(input)
	var tokens []reviewtypes.Tokens
	for _, ev := range events {
		if tok, ok := ev.(reviewtypes.Tokens); ok {
			tokens = append(tokens, tok)
		}
	}
	if len(tokens) != 1 {
		t.Fatalf("token events = %d, want 1: %#v", len(tokens), events)
	}
	if got := tokens[0]; got.In != 10 || got.Out != 5 {
		t.Fatalf("Tokens = %#v, want In=10 Out=5", got)
	}
}

func collectPiReviewEvents(input string) []reviewtypes.Event {
	ch := parsePiReviewOutput(strings.NewReader(input))
	var events []reviewtypes.Event
	for ev := range ch {
		events = append(events, ev)
	}
	return events
}

func envMap(env []string) map[string]string {
	out := map[string]string{}
	for _, kv := range env {
		idx := strings.IndexByte(kv, '=')
		if idx < 0 {
			continue
		}
		out[kv[:idx]] = kv[idx+1:]
	}
	return out
}

// A provider error (here, an exhausted API balance) used to leave the review
// with an empty "Failed" report. It must surface once, though Pi repeats the
// failed message in message_end and turn_end.
func TestPiReviewer_ParseSurfacesProviderError(t *testing.T) {
	t.Parallel()
	const failed = `{"role":"assistant","content":[],"stopReason":"error","errorMessage":"You have no credits remaining."}`
	input := `{"type":"agent_start"}` + "\n" +
		`{"type":"turn_start"}` + "\n" +
		`{"type":"message_end","message":` + failed + `}` + "\n" +
		`{"type":"turn_end","message":` + failed + `}` + "\n" +
		`{"type":"agent_end"}` + "\n"

	var errs []string
	var finished *reviewtypes.Finished
	for _, ev := range collectPiReviewEvents(input) {
		switch e := ev.(type) {
		case reviewtypes.RunError:
			errs = append(errs, e.Err.Error())
		case reviewtypes.Finished:
			finished = &e
		}
	}
	if len(errs) != 1 || errs[0] != "pi: You have no credits remaining." {
		t.Fatalf("RunErrors = %q, want the provider error once", errs)
	}
	if finished == nil || finished.Success {
		t.Fatalf("Finished = %+v, want a failure", finished)
	}
}

// Pi auto-retries errors such as "overloaded". A run that recovers must not
// report the attempt that failed.
func TestPiReviewer_ParseRecoveredRetryIsNotAFailure(t *testing.T) {
	t.Parallel()
	const failed = `{"role":"assistant","content":[],"stopReason":"error","errorMessage":"overloaded"}`
	const ok = `{"role":"assistant","content":[{"type":"text","text":"approve"}],"stopReason":"stop"}`
	input := `{"type":"agent_start"}` + "\n" +
		`{"type":"message_end","message":` + failed + `}` + "\n" +
		`{"type":"turn_end","message":` + failed + `}` + "\n" +
		`{"type":"agent_end","willRetry":true}` + "\n" +
		`{"type":"auto_retry_start"}` + "\n" +
		`{"type":"message_end","message":` + ok + `}` + "\n" +
		`{"type":"turn_end","message":` + ok + `}` + "\n" +
		`{"type":"agent_end"}` + "\n"

	finishes := 0
	for _, ev := range collectPiReviewEvents(input) {
		switch e := ev.(type) {
		case reviewtypes.RunError:
			t.Fatalf("recovered retry reported an error: %v", e.Err)
		case reviewtypes.Finished:
			finishes++
			if !e.Success {
				t.Fatal("recovered retry reported as failed")
			}
		}
	}
	if finishes != 1 {
		t.Fatalf("Finished emitted %d times, want once", finishes)
	}
}

// A failure without an errorMessage still says why the review failed.
func TestPiReviewer_ParseErrorWithoutMessage(t *testing.T) {
	t.Parallel()
	const failed = `{"role":"assistant","content":[],"stopReason":"aborted"}`
	input := `{"type":"message_end","message":` + failed + `}` + "\n" + `{"type":"agent_end"}` + "\n"
	var errs []string
	for _, ev := range collectPiReviewEvents(input) {
		if e, ok := ev.(reviewtypes.RunError); ok {
			errs = append(errs, e.Err.Error())
		}
	}
	if len(errs) != 1 || errs[0] != "pi: stopped with aborted" {
		t.Fatalf("RunErrors = %q, want the stop reason", errs)
	}
}

func TestCheckPiReviewVersion(t *testing.T) {
	t.Parallel()

	errNoPi := errors.New("pi: executable file not found")
	tests := []struct {
		name    string
		out     string
		err     error
		wantErr bool
	}{
		// Before 0.65.1, piped stdin with --mode json printed plain text, which
		// parsePiReviewOutput reports as malformed JSON and a failed review.
		{name: "too old", out: "0.64.3\n", wantErr: true},
		{name: "minimum", out: MinPiStdinJSONVersion + "\n"},
		{name: "newer", out: "0.70.2\n"},
		{name: "v prefix", out: "v0.60.0", wantErr: true},
		{name: "name prefix", out: "pi 0.64.0\n", wantErr: true},
		{name: "trailing notice", out: "0.64.0\nUpdate available: 0.70.2\n", wantErr: true},
		// A settings warning printed before the version must not be read as
		// one (semver accepts the shorthand v25).
		{name: "settings warning first", out: "Unexpected token in JSON at position 25 (line 3 column 1)\n0.64.0\n", wantErr: true},
		{name: "shorthand only", out: "pi 1.2\n"},
		{name: "unparseable", out: "pi dev build\n"},
		{name: "empty", out: ""},
		{name: "probe failed", err: errNoPi},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := checkPiReviewVersion(context.Background(), func(context.Context) (string, error) {
				return tt.out, tt.err
			})
			if (err != nil) != tt.wantErr {
				t.Fatalf("checkPiReviewVersion(%q) = %v, wantErr %v", tt.out, err, tt.wantErr)
			}
			if tt.wantErr && !strings.Contains(err.Error(), MinPiStdinJSONVersion) {
				t.Errorf("error %q should name the minimum version", err)
			}
		})
	}
}

func TestPiReviewer_HasVersionPreflight(t *testing.T) {
	t.Parallel()
	if NewReviewer().Preflight == nil {
		t.Fatal("pi reviewer must check the pi version before piping the prompt on stdin")
	}
}

// Some Pi versions end the stream after the final turn without agent_end. A
// turn that was not waiting on tool results still finishes the review; a
// stream cut after a tool-use turn does not.
func TestPiReviewer_ParseEndsWithoutAgentEnd(t *testing.T) {
	t.Parallel()
	finished := func(input string) (bool, bool) {
		for _, ev := range collectPiReviewEvents(input) {
			if f, ok := ev.(reviewtypes.Finished); ok {
				return true, f.Success
			}
		}
		return false, false
	}
	stopped := `{"type":"turn_end","message":{"role":"assistant","stopReason":"stop"}}` + "\n"
	if seen, ok := finished(stopped); !seen || !ok {
		t.Errorf("final stop turn: finished=%v success=%v, want a successful finish", seen, ok)
	}
	errored := `{"type":"turn_end","message":{"role":"assistant","stopReason":"error","errorMessage":"boom"}}` + "\n"
	if _, ok := finished(errored); ok {
		t.Error("stream ending on an error turn counted as success")
	}
	retrying := errored + `{"type":"auto_retry_start"}` + "\n"
	if _, ok := finished(retrying); ok {
		t.Error("stream cut off during a retry counted as success")
	}
	cutOff := stopped + `{"type":"turn_start"}` + "\n"
	if _, ok := finished(cutOff); ok {
		t.Error("stream cut off mid-turn counted as success")
	}
	toolUse := `{"type":"turn_end","message":{"role":"assistant","stopReason":"toolUse"}}` + "\n"
	if _, ok := finished(toolUse); ok {
		t.Error("stream cut after a tool-use turn counted as success")
	}
}

// A turn cut off at the output token limit is a truncated review, with or
// without agent_end, and says so.
func TestPiReviewer_ParseLengthStopFails(t *testing.T) {
	t.Parallel()
	turn := `{"type":"turn_end","message":{"role":"assistant","stopReason":"length"}}` + "\n"
	for _, input := range []string{turn, turn + `{"type":"agent_end"}` + "\n"} {
		var errs []string
		success := true
		for _, ev := range collectPiReviewEvents(input) {
			switch e := ev.(type) {
			case reviewtypes.RunError:
				errs = append(errs, e.Err.Error())
			case reviewtypes.Finished:
				success = e.Success
			}
		}
		if success || len(errs) != 1 || !strings.Contains(errs[0], "output token limit") {
			t.Errorf("success=%v errs=%q, want a token-limit failure", success, errs)
		}
	}
}

// Pi recovers from a tool call cut off at the token limit by asking the model
// to re-issue it; a run that then finishes normally succeeds.
func TestPiReviewer_ParseRecoveredLengthStopSucceeds(t *testing.T) {
	t.Parallel()
	recovered := `{"type":"turn_end","message":{"role":"assistant","stopReason":"length"}}` + "\n" +
		`{"type":"turn_start"}` + "\n" +
		`{"type":"turn_end","message":{"role":"assistant","stopReason":"stop"}}` + "\n"
	for _, input := range []string{recovered, recovered + `{"type":"agent_end"}` + "\n"} {
		for _, ev := range collectPiReviewEvents(input) {
			switch e := ev.(type) {
			case reviewtypes.RunError:
				t.Errorf("unexpected error %v", e.Err)
			case reviewtypes.Finished:
				if !e.Success {
					t.Error("recovered length stop failed the review")
				}
			}
		}
	}
}
