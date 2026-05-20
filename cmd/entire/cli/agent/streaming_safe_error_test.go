package agent_test

import (
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
)

func TestSafeErrorMessage(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		line string
		want string
	}{
		{"top_level_message", `{"type":"error","message":"model not found"}`, "model not found"},
		{"nested_error_message", `{"type":"turn.failed","error":{"message":"timed out"}}`, "timed out"},
		{"nested_data_message", `{"type":"error","data":{"message":"rate limited"}}`, "rate limited"},
		{"prefers_top_level_over_nested", `{"message":"top","error":{"message":"nested"}}`, "top"},
		{"prefers_error_over_data", `{"error":{"message":"e"},"data":{"message":"d"}}`, "e"},
		{"empty_strings_fall_through", `{"message":"","error":{"message":""},"data":{"message":""}}`, "unspecified error"},
		{"no_message_paths", `{"type":"thread.started","thread_id":"t"}`, "unspecified error"},
		{"malformed_json", `{not json at all`, "unspecified error"},
		{"empty_line", ``, "unspecified error"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := agent.SafeErrorMessage([]byte(tc.line))
			if got != tc.want {
				t.Errorf("SafeErrorMessage(%q) = %q, want %q", tc.line, got, tc.want)
			}
		})
	}
}
