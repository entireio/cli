package cli

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/external"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// Uses a real protocol subprocess to enforce the external agent's byte-offset
// contract. CWD and environment are isolated by setupAttachTestRepo.
func TestAttach_ExternalTokenByteOffset(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fixture uses a Unix shebang")
	}
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 required for protocol fixture")
	}
	setupAttachTestRepo(t)
	path := filepath.Join(t.TempDir(), "transcript.jsonl")
	t.Setenv("ATTACH_TEST_TRANSCRIPT", path)
	binary := filepath.Join(t.TempDir(), "entire-agent-attach-byte-offset")
	script := fmt.Sprintf(`#!%s
import base64, json, os, sys
cmd = sys.argv[1]
if cmd == "info":
 print(json.dumps({"protocol_version":1,"name":"attach-byte-offset","type":"Attach Byte Offset","capabilities":{"token_calculator":True}}))
elif cmd == "get-session-dir":
 print(json.dumps({"session_dir":os.path.dirname(os.environ["ATTACH_TEST_TRANSCRIPT"])}))
elif cmd == "resolve-session-file":
 print(json.dumps({"session_file":os.environ["ATTACH_TEST_TRANSCRIPT"]}))
elif cmd == "read-transcript":
 sys.stdout.buffer.write(open(os.environ["ATTACH_TEST_TRANSCRIPT"], "rb").read())
elif cmd == "chunk-transcript":
 print(json.dumps({"chunks":[base64.b64encode(sys.stdin.buffer.read()).decode()]}))
elif cmd == "reassemble-transcript":
 sys.stdout.buffer.write(b"".join(base64.b64decode(c) for c in json.load(sys.stdin)["chunks"]))
elif cmd == "calculate-tokens":
 data = sys.stdin.buffer.read()[int(sys.argv[3]):]
 total = sum(json.loads(line).get("message",{}).get("usage",{}).get("input_tokens",0) for line in data.splitlines() if line.strip())
 print(json.dumps({"input_tokens":total}))
else:
 sys.exit(2)
`, python)
	if err := os.WriteFile(binary, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	ea, err := external.New(t.Context(), binary)
	if err != nil {
		t.Fatal(err)
	}
	ag, err := external.Wrap(ea)
	if err != nil {
		t.Fatal(err)
	}
	agent.Register(ag.Name(), func() agent.Agent { return ag })
	const sid = "external-byte-offset"
	var out bytes.Buffer
	// Redaction changes the stored byte length; offsets must still address raw input.
	first := strings.Replace(attachFirstTurn, "first", "first clé sk-ant-api03-xK9mZ2vL8nQ5rT1wY4bC7dF0gH3jE6pA", 1)
	for i, data := range []string{first, first + attachNextTurn} {
		if i > 0 {
			testutil.WriteFile(t, mustGetwd(t), "next.txt", "next change")
			testutil.GitAdd(t, mustGetwd(t), "next.txt")
			testutil.GitCommit(t, mustGetwd(t), "next commit")
		}
		if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := runAttach(t.Context(), &out, &out, sid, ag.Name(), attachOptions{Force: true}); err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			_, _, snapshot := readAttachSnapshot(t, sid)
			if len(snapshot.Transcript) == len(data) {
				t.Fatal("fixture did not change stored byte length through redaction")
			}
		}
	}
	state, _, content := readAttachSnapshot(t, sid)
	if content.Metadata.GetTranscriptStart() != 2 {
		t.Fatalf("start=%d, want 2", content.Metadata.GetTranscriptStart())
	}
	if content.Metadata.TokenUsage == nil || content.Metadata.TokenUsage.InputTokens != 20 {
		t.Errorf("checkpoint usage=%+v, want input=20", content.Metadata.TokenUsage)
	}
	if state.TokenUsage == nil || state.TokenUsage.InputTokens != 30 {
		t.Errorf("session usage=%+v, want input=30", state.TokenUsage)
	}
}
