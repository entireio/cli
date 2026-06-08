package cli

import (
	"context"
	"strings"
	"testing"
)

// TestHandleClaudeCodePostTodoFromReader_RejectsUnsafeInput verifies the
// PostTodo bypass handler validates raw payload fields before they are used
// to build filesystem paths. The PostTodo path is dispatched outside
// DispatchLifecycleEvent (see hook_registry.go), so the central guard does
// not run here.
//
// The validation gate runs before GetCurrentHookAgent / disk access, so these
// cases do not require hook-agent setup or a repository.
func TestHandleClaudeCodePostTodoFromReader_RejectsUnsafeInput(t *testing.T) {
	t.Parallel()

	const nulTranscriptPayload = "{\"session_id\":\"ok\",\"tool_use_id\":\"toolu_ok\",\"transcript_path\":\"/tmp/t\\u0000.jsonl\"}"

	cases := []struct {
		name    string
		payload string
		errSub  string
	}{
		{
			name:    "traversal session ID",
			payload: `{"session_id":"../../etc/evil","tool_use_id":"toolu_ok","transcript_path":"/tmp/t.jsonl"}`,
			errSub:  "invalid session ID",
		},
		{
			name:    "traversal tool use ID",
			payload: `{"session_id":"ok","tool_use_id":"../evil","transcript_path":"/tmp/t.jsonl"}`,
			errSub:  "invalid tool use ID",
		},
		{
			name:    "null-byte transcript path",
			payload: nulTranscriptPayload,
			errSub:  "invalid transcript path",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := handleClaudeCodePostTodoFromReader(context.Background(), strings.NewReader(tc.payload))
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.errSub)
			}
			if !strings.Contains(err.Error(), tc.errSub) {
				t.Errorf("error = %q, want substring %q", err.Error(), tc.errSub)
			}
		})
	}
}
