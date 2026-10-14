//go:build integration

package integration

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/stretchr/testify/require"
)

// externalTokenAgentScript is a protocol-v1 external agent, entire-agent-faketok,
// in POSIX sh + sed + awk. Its transcript is JSONL with one object per line:
//
//	{"kind":"user","text":"..."}
//	{"kind":"file","path":"f1.txt"}
//	{"kind":"tok","in":1,"out":2,"cr":3,"cw":4}
//
// and hooks receive {"session_id":...,"transcript_path":...,"prompt":...}.
// %[1]s is the agent's session directory.
const externalTokenAgentScript = `#!/bin/sh
SESSION_DIR=%[1]q
field() { sed -n 's/.*"'"$1"'":"\([^"]*\)".*/\1/p'; }
lines() { if [ -f "$1" ]; then wc -l <"$1" | tr -d ' '; else echo 0; fi; }
case "$1" in
info)
  echo '{"protocol_version":1,"name":"faketok","type":"Faketok Agent","description":"token harness fake","protected_dirs":[],"hook_names":["session-start","user-prompt-submit","stop","session-end"],"capabilities":{"hooks":true,"transcript_analyzer":true,"token_calculator":true}}' ;;
detect) echo '{"present":true}' ;;
parse-hook)
  in=$(cat)
  sid=$(printf '%%s' "$in" | field session_id)
  tp=$(printf '%%s' "$in" | field transcript_path)
  prompt=$(printf '%%s' "$in" | field prompt)
  ts=$(date -u +%%Y-%%m-%%dT%%H:%%M:%%SZ)
  case "$3" in
  session-start) t=1 ;;
  user-prompt-submit) t=2 ;;
  stop) t=3 ;;
  session-end) t=5 ;;
  *) echo null; exit 0 ;;
  esac
  printf '{"type":%%d,"session_id":"%%s","session_ref":"%%s","prompt":"%%s","timestamp":"%%s"}\n' "$t" "$sid" "$tp" "$prompt" "$ts" ;;
get-session-id) printf '{"session_id":"%%s"}\n' "$(cat | field session_id)" ;;
get-session-dir) printf '{"session_dir":"%%s"}\n' "$SESSION_DIR" ;;
resolve-session-file) printf '{"session_file":"%%s/%%s.jsonl"}\n' "$3" "$5" ;;
read-transcript) cat "$3" ;;
get-transcript-position) printf '{"position":%%s}\n' "$(lines "$3")" ;;
extract-modified-files)
  files=$(tail -n +$(($5 + 1)) "$3" | sed -n 's/.*"kind":"file","path":"\([^"]*\)".*/"\1"/p' | paste -sd, -)
  printf '{"files":[%%s],"current_position":%%s}\n' "$files" "$(lines "$3")" ;;
extract-prompts)
  prompts=$(tail -n +$(($5 + 1)) "$3" | sed -n 's/.*"kind":"user","text":"\([^"]*\)".*/"\1"/p' | paste -sd, -)
  printf '{"prompts":[%%s]}\n' "$prompts" ;;
calculate-tokens)
  awk -v off="$3" '
    function f(k,  re) { re = "\"" k "\":[0-9]+"; if (match($0, re)) return substr($0, RSTART + length(k) + 3, RLENGTH - length(k) - 3) + 0; return 0 }
    NR > off && /"kind":"tok"/ { a += f("in"); b += f("out"); c += f("cr"); d += f("cw"); n++ }
    END { printf "{\"input_tokens\":%%d,\"output_tokens\":%%d,\"cache_read_tokens\":%%d,\"cache_creation_tokens\":%%d,\"api_call_count\":%%d}\n", a, b, c, d, n }' ;;
chunk-transcript) printf '{"chunks":["%%s"]}\n' "$(base64 | tr -d '\n')" ;;
reassemble-transcript) sed -n 's/.*"chunks":\["\([^"]*\)".*/\1/p' | base64 -d ;;
format-resume-command) printf '{"command":"faketok --resume %%s"}\n' "$3" ;;
install-hooks) echo '{"hooks_installed":0}' ;;
uninstall-hooks|write-session) cat >/dev/null; exit 0 ;;
are-hooks-installed) echo '{"installed":true}' ;;
*) echo "unknown subcommand: $1" >&2; exit 1 ;;
esac
`

type externalTokenDriver struct{ tokenDriverBase }

func newExternalTokenDriver() externalTokenDriver {
	return externalTokenDriver{tokenDriverBase{name: "external", agent: "faketok",
		caps: []tokenCap{capAttach, capSessionEnd, capTranscriptTokens, capMidTurnTokens}}}
}

// Setup installs entire-agent-faketok on PATH and trusts external agents in
// the local settings layer, the only one that may enable them.
func (externalTokenDriver) Setup(h *tokenHarness) {
	h.agentDir = realDir(h)
	sessions := filepath.Join(h.agentDir, "sessions")
	require.NoError(h.t, os.MkdirAll(sessions, 0o755))
	bin := filepath.Join(h.agentDir, "bin")
	require.NoError(h.t, os.MkdirAll(bin, 0o755))
	require.NoError(h.t, os.WriteFile(filepath.Join(bin, "entire-agent-faketok"),
		[]byte(fmt.Sprintf(externalTokenAgentScript, sessions)), 0o755))
	require.NoError(h.t, os.WriteFile(filepath.Join(h.env.RepoDir, ".entire", "settings.local.json"),
		[]byte(`{"external_agents":true}`+"\n"), 0o644))
	// A minimal PATH: every hook's external-agent discovery scans PATH under a
	// short timeout, and a developer PATH with other entire-agent-* binaries can
	// exhaust it under the parallel matrix. Keep the fake agent, the binary under
	// test, git and the POSIX tools the fake agent's script uses.
	gitPath, err := exec.LookPath("git")
	require.NoError(h.t, err)
	dirs := []string{bin, filepath.Dir(getTestBinary()), filepath.Dir(gitPath), "/usr/bin", "/bin"}
	for i, kv := range h.env.ExtraEnv {
		if strings.HasPrefix(kv, "PATH=") {
			h.env.ExtraEnv[i] = "PATH=" + strings.Join(dirs, string(os.PathListSeparator))
		}
	}
}

func (externalTokenDriver) NewSession(h *tokenHarness) tokenSession {
	id := fmt.Sprintf("faketok-%d", h.nextSessionNumber())
	s := &externalTokenSession{unsupportedSession: unsupportedSession{"external"}, h: h, id: id,
		path: filepath.Join(h.agentDir, "sessions", id+".jsonl")}
	appendJSONLines(h, s.path, map[string]any{"kind": "start"})
	s.hook("session-start", nil)
	return s
}

type externalTokenSession struct {
	unsupportedSession

	h        *tokenHarness
	id, path string
}

func (s *externalTokenSession) ID() string             { return s.id }
func (s *externalTokenSession) TranscriptPath() string { return s.path }

func (s *externalTokenSession) hook(name string, extra map[string]any) {
	in := mergeMaps(map[string]any{"session_id": s.id, "transcript_path": s.path}, extra)
	hookCall(s.h, s.h.sessionDir(s.id), "faketok", name, in)
}

func (s *externalTokenSession) StartTurn(prompt string) {
	s.hook("user-prompt-submit", map[string]any{"prompt": prompt})
	appendJSONLines(s.h, s.path, map[string]any{"kind": "user", "text": prompt})
}

func (s *externalTokenSession) ToolCall(u tokenCounts, tool toolUse, run func()) {
	s.Spend(u)
	if tool.path != "" {
		appendJSONLines(s.h, s.path, map[string]any{"kind": "file", "path": tool.path})
	}
	run()
}

func (s *externalTokenSession) Spend(u tokenCounts) {
	appendJSONLines(s.h, s.path, map[string]any{"kind": "tok", "in": u.In, "out": u.Out, "cr": u.CacheRead, "cw": u.CacheWrite})
}

func (s *externalTokenSession) Stop()       { s.hook("stop", nil) }
func (s *externalTokenSession) SessionEnd() { s.hook("session-end", nil) }
