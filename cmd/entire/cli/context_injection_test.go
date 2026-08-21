package cli

import (
	"strings"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/strategy"
)

func TestCrossRepoContextEligibilityCadenceAndTopicChange(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)
	state := strategy.CrossRepoContextState{}
	if !crossRepoContextEligible(state, "debug oauth refresh failure", now) {
		t.Fatal("first non-empty prompt should be eligible")
	}
	state.PacketCount = 1
	state.LastSuccessfulAt = now.Add(-time.Minute)
	state.LastPromptTokenHashes = promptTokenHashes("debug oauth refresh failure")
	if crossRepoContextEligible(state, "debug oauth refresh error", now) {
		t.Fatal("similar prompt inside two-minute cadence should not be eligible")
	}
	if !crossRepoContextEligible(state, "redesign database migration planner", now.Add(3*time.Minute)) {
		t.Fatal("topic change after cadence should be eligible")
	}
	state.PacketCount = 4
	if crossRepoContextEligible(state, "entirely different topic", now.Add(time.Hour)) {
		t.Fatal("fifth packet should never be eligible")
	}
}

func TestCrossRepoContextEligibilityPendingAndFailureBackoff(t *testing.T) {
	t.Parallel()
	now := time.Now()
	for name, state := range map[string]strategy.CrossRepoContextState{
		"pending": {PendingUntil: now.Add(time.Minute)},
		"backoff": {FailureBackoffUntil: now.Add(time.Minute)},
	} {
		if crossRepoContextEligible(state, "new useful prompt", now) {
			t.Errorf("%s state was eligible", name)
		}
	}
}

func TestPromptHashJaccardIsSymmetric(t *testing.T) {
	t.Parallel()
	a := promptTokenHashes("debug oauth refresh failure")
	b := promptTokenHashes("oauth refresh timeout")
	if got, want := promptHashJaccard(a, b), promptHashJaccard(b, a); got != want {
		t.Fatalf("jaccard similarity is asymmetric: a,b=%v b,a=%v", got, want)
	}
}

func TestRenderCrossRepoContextPacketIsBoundedAndUntrusted(t *testing.T) {
	t.Parallel()
	evidence := []contextEvidence{{
		ID: "ctx_a", SourceType: "checkpoint", RepoName: "acme/api", Summary: "Useful\x00 summary",
		Excerpt: stringsOfLength(9000), DrillDown: "entire checkpoint explain cp --repo acme/api",
	}}
	packet := renderCrossRepoContextPacket(evidence)
	if len(packet) > 6<<10 {
		t.Fatalf("packet length = %d, want <= 6KiB", len(packet))
	}
	if !containsAll(packet, "untrusted", "ctx_a", "<entire-context>", "</entire-context>") {
		t.Fatalf("packet missing safety markers: %q", packet)
	}
}

func stringsOfLength(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = 'x'
	}
	return string(b)
}

func containsAll(s string, values ...string) bool {
	for _, value := range values {
		if !strings.Contains(s, value) {
			return false
		}
	}
	return true
}
