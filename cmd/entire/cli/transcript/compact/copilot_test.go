package compact

import (
	"strings"
	"testing"

	"github.com/entireio/cli/redact"
)

func TestCompact_CopilotFixture(t *testing.T) {
	t.Parallel()

	assertFixtureTransform(t, agentOpts("copilot-cli"), "testdata/copilot_full.jsonl", "testdata/copilot_expected.jsonl")
}

// Newer Copilot CLI releases drop assistant.message outputTokens and write a
// session.usage_record per call instead, joined to the message by apiCallId.
func TestCompact_CopilotUsageRecordTokens(t *testing.T) {
	t.Parallel()

	const record = `{"type":"session.usage_record","timestamp":"2026-10-13T20:03:51Z","data":{"usage":{"apiCallId":"call-1","inputTokens":20195,"outputTokens":396,"cacheReadTokens":0,"cacheWriteTokens":20185}}}`
	const message = `{"type":"assistant.message","timestamp":"2026-10-13T20:03:52Z","data":{"messageId":"m1","apiCallId":"call-1","content":"done"}}`
	const unrelated = `{"type":"assistant.message","timestamp":"2026-10-13T20:03:53Z","data":{"messageId":"m2","apiCallId":"call-2","content":"more"}}`
	want := []string{
		`{"v":1,"agent":"copilot-cli","cli_version":"0.5.1","type":"assistant","ts":"2026-10-13T20:03:52Z","id":"m1","input_tokens":10,"output_tokens":396,"content":[{"type":"text","text":"done"}]}`,
		`{"v":1,"agent":"copilot-cli","cli_version":"0.5.1","type":"assistant","ts":"2026-10-13T20:03:53Z","id":"m2","content":[{"type":"text","text":"more"}]}`,
	}

	for name, lines := range map[string][]string{
		"record before message": {record, message, unrelated},
		"record after message":  {message, record, unrelated},
		// A concurrent call's message lands between this call's message and
		// its record, flushing the line before the record arrives.
		"record after an interleaved call": {message, unrelated, record},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			result, err := Compact(redact.AlreadyRedacted([]byte(strings.Join(lines, "\n")+"\n")), agentOpts("copilot-cli"))
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			assertJSONLines(t, result, want)
		})
	}
}
