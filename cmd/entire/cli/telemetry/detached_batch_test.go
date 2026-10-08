package telemetry

import (
	"encoding/json"
	"strings"
	"testing"
)

// captureSends swaps in the spawn seam and returns the payloads each detached
// send would have carried.
func captureSends(t *testing.T) *[]string {
	t.Helper()
	sends := []string{}
	prev := spawnAnalyticsHook
	spawnAnalyticsHook = func(payloadJSON string) { sends = append(sends, payloadJSON) }
	t.Cleanup(func() { spawnAnalyticsHook = prev })
	return &sends
}

func skillInvocations(n int) []SkillInvocation {
	out := make([]SkillInvocation, 0, n)
	for range n {
		out = append(out, SkillInvocation{
			Skill:     "entire:search",
			Agent:     "claude-code",
			Signal:    "skill_tool_use",
			EventType: "tool_invocation",
		})
	}
	return out
}

// eventsIn counts the events across every send, asserting each is a JSON array.
func eventsIn(t *testing.T, sends []string) int {
	t.Helper()
	total := 0
	for _, send := range sends {
		var batch []EventPayload
		if err := json.Unmarshal([]byte(send), &batch); err != nil {
			t.Fatalf("send is not a JSON array of events: %v", err)
		}
		total += len(batch)
	}
	return total
}

func TestTrackSkillInvocationsDetached_BatchesIntoOneSend(t *testing.T) {
	sends := captureSends(t)

	TrackSkillInvocationsDetached(skillInvocations(5), true, "1.2.3")

	if len(*sends) != 1 {
		t.Fatalf("spawned %d detached sends, want 1", len(*sends))
	}
	if got := eventsIn(t, *sends); got != 5 {
		t.Errorf("sent %d events, want 5", got)
	}
}

// The 10-event cap used to truncate here. Since condensation extracts from
// transcript offset 0, the first condensation of a session drains its whole
// backlog in one call, and a dropped event is never re-reported — session
// state has already recorded it.
func TestTrackSkillInvocationsDetached_DropsNothingAboveTheOldCap(t *testing.T) {
	sends := captureSends(t)

	const count = 25
	TrackSkillInvocationsDetached(skillInvocations(count), true, "1.2.3")

	if got := eventsIn(t, *sends); got != count {
		t.Errorf("sent %d events, want all %d", got, count)
	}
}

func TestTrackSkillInvocationsDetached_SplitsOversizedBatchInsteadOfDropping(t *testing.T) {
	sends := captureSends(t)

	// Enough events that the JSON exceeds one argv budget.
	const count = 600
	TrackSkillInvocationsDetached(skillInvocations(count), true, "1.2.3")

	if len(*sends) < 2 {
		t.Fatalf("expected the batch to split across sends, got %d send(s)", len(*sends))
	}
	for i, send := range *sends {
		if len(send) > maxDetachedPayloadBytes {
			t.Errorf("send %d is %d bytes, over the %d budget", i, len(send), maxDetachedPayloadBytes)
		}
	}
	if got := eventsIn(t, *sends); got != count {
		t.Errorf("sent %d events across %d sends, want all %d", got, len(*sends), count)
	}
}

func TestTrackSkillInvocationsDetached_EnvOptOutSendsNothing(t *testing.T) {
	t.Setenv("ENTIRE_TELEMETRY_OPTOUT", "1")
	sends := captureSends(t)

	TrackSkillInvocationsDetached(skillInvocations(3), true, "1.2.3")

	if len(*sends) != 0 {
		t.Errorf("spawned %d sends despite the env opt-out, want 0", len(*sends))
	}
}

func TestTrackSkillInvocationsDetached_NoInvocationsNoSend(t *testing.T) {
	sends := captureSends(t)

	TrackSkillInvocationsDetached(nil, true, "1.2.3")

	if len(*sends) != 0 {
		t.Errorf("spawned %d sends for zero invocations, want 0", len(*sends))
	}
}

func TestDecodeEventPayloads(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input string
		want  int
	}{
		{"array", `[{"event":"a"},{"event":"b"}]`, 2},
		{"empty array", `[]`, 0},
		// A self-update can replace the binary between spawn and exec, so the
		// child must still read a payload written by a single-event build.
		{"single object", `{"event":"a"}`, 1},
		{"leading whitespace", "  \n[{\"event\":\"a\"}]", 1},
		{"malformed", `{"event":`, 0},
		{"empty", ``, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := len(decodeEventPayloads(tt.input)); got != tt.want {
				t.Errorf("decodeEventPayloads(%q) returned %d payloads, want %d", tt.input, got, tt.want)
			}
		})
	}
}

func TestSendEventsAcceptsBothShapes(_ *testing.T) {
	// No network in tests; this only asserts neither shape panics.
	SendEvents(`[{"event":"a","distinct_id":"x"}]`)
	SendEvents(`{"event":"a","distinct_id":"x"}`)
	SendEvents(strings.Repeat("x", 10))
}
