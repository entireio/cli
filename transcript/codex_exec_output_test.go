package transcript

import (
	"strings"
	"testing"
)

// Older Codex shell tools, and forks built on them, wrap a command's output
// as {"output":...,"metadata":{"exit_code":N}}; the exit code decides status.
func TestCompact_CodexExecOutputEnvelope(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		tool   string
		output string
		result string
	}{
		{
			name:   "non-zero exit with empty output is an error",
			output: `{"output":"","metadata":{"exit_code":1,"duration_seconds":0.1}}`,
			result: `{"output":"","status":"error","exitCode":1}`,
		},
		{
			name:   "zero exit unwraps the output",
			output: `{"output":"hello\n","metadata":{"exit_code":0}}`,
			result: `{"output":"hello\n","status":"success","exitCode":0}`,
		},
		{
			name:   "JSON without metadata.exit_code is left as is",
			output: `{"output":"x","metadata":{}}`,
			result: `{"output":"{\"output\":\"x\",\"metadata\":{}}","status":"success"}`,
		},
		{
			name:   "a non-shell built-in tool's envelope is unwrapped",
			tool:   "write_file",
			output: `{"output":"Success","metadata":{"exit_code":0}}`,
			result: `{"output":"Success","status":"success","exitCode":0}`,
		},
		{
			name:   "an MCP tool returning the same shape is left as is",
			tool:   "mcp__ci__run",
			output: `{"output":"","metadata":{"exit_code":1}}`,
			result: `{"output":"{\"output\":\"\",\"metadata\":{\"exit_code\":1}}","status":"success"}`,
		},
		{
			name:   "an object with keys beyond output and metadata is left as is",
			output: `{"output":"","metadata":{"exit_code":1},"job":"j1"}`,
			result: `{"output":"{\"output\":\"\",\"metadata\":{\"exit_code\":1},\"job\":\"j1\"}","status":"success"}`,
		},
		{
			name:   "plain text is left as is",
			output: "Process exited with code 1",
			result: `{"output":"Process exited with code 1","status":"success"}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			call := cxCall("t1", "c1")
			if tt.tool != "" {
				call = strings.Replace(call, `"name":"exec_command"`, `"name":"`+tt.tool+`"`, 1)
			}
			input := cxJoin(
				`{"timestamp":"t0","type":"session_meta","payload":{"id":"s1"}}`,
				call,
				cxOutput("t2", "c1", tt.output),
			)
			out, err := Convert(input, agentOpts("codex"))
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(out), `"result":`+tt.result) {
				t.Errorf("result: want %s in\n%s", tt.result, out)
			}
		})
	}
}
