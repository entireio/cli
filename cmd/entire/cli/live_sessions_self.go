package cli

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
)

// liveSessionsSelf is which live sessions are the caller's own work, left out
// of the block: an agent stopping to ask the user about its own session, or
// the agent that started it, is the failure this exists to prevent.
type liveSessionsSelf struct {
	// Own is every ID the caller's own session may be streamed under
	// (liveSessionOwnIDs). Its subagents are left out with it.
	Own []string
	// Parents is the session of an agent that started this one and still
	// runs above it (liveSessionParentIDs). Left out, with its subagents,
	// only when the caller's account owns it.
	Parents []string
}

// owns reports whether e is one of the caller's own sessions; mine is
// whether it is the caller's account's (liveSessionsCache.callers). When the
// account is known (accountKnown), an own ID counts only for that account's
// sessions: a teammate live under an ID the caller resumed locally is
// theirs. When it isn't, the ID alone decides.
func (s liveSessionsSelf) owns(e liveSessionEntry, mine, accountKnown bool) bool {
	if liveSessionInFamily(e.SessionID, s.Own) && (mine || !accountKnown) {
		return true
	}
	return mine && liveSessionInFamily(e.SessionID, s.Parents)
}

// liveSessionInFamily reports whether id is one of ids or a subagent of one:
// entire-stream streams a Claude subagent as <session>:agent:<id>.
func liveSessionInFamily(id string, ids []string) bool {
	for _, o := range ids {
		if o != "" && (id == o || strings.HasPrefix(id, o+":")) {
			return true
		}
	}
	return false
}

// liveSessionSelfFor is the caller's own sessions for event's turn. cache is
// what the block is built from: the parent check, which lists processes,
// runs only when a session of the caller's account in it carries a parent's
// ID.
func liveSessionSelfFor(ctx context.Context, ag agent.Agent, event *agent.Event, cache *liveSessionsCache) liveSessionsSelf {
	return liveSessionsSelf{Own: liveSessionOwnIDs(ag, event), Parents: liveSessionParentIDs(ctx, ag, event, cache)}
}

// liveSessionHeaderIDer is an agent whose stream may know a session by an ID
// read from its session file rather than the one its hook gives (Pi: see
// (*pi.PiAgent).HeaderSessionID).
type liveSessionHeaderIDer interface {
	HeaderSessionID(sessionFile string) string
}

// liveSessionOwnIDs is every ID the caller's own live session may be streamed
// under: the hook's session ID and, for an agent whose stream reads the ID
// from the session file (event.SessionRef), that one.
func liveSessionOwnIDs(ag agent.Agent, event *agent.Event) []string {
	own := []string{event.SessionID}
	if h, ok := ag.(liveSessionHeaderIDer); ok && event.SessionRef != "" {
		if id := h.HeaderSessionID(event.SessionRef); id != "" && id != event.SessionID {
			own = append(own, id)
		}
	}
	return own
}

// liveSessionParentVar is a variable through which an agent program passes
// a session ID to the processes it starts.
type liveSessionParentVar struct{ program, env string }

// liveSessionParentVars are the parent agents' variables. Codex sets both of
// its own: CODEX_THREAD_ID names the thread that ran the command, a
// sub-agent's when a sub-agent did, and CODEX_SESSION_ID the root session,
// which is the one that streams (see the codex agent's
// CallerSessionEnvVar).
var liveSessionParentVars = []liveSessionParentVar{
	{liveSessionProgramCodex, "CODEX_SESSION_ID"},
	{liveSessionProgramCodex, "CODEX_THREAD_ID"},
	{liveSessionProgramClaude, "CLAUDE_CODE_SESSION_ID"},
}

// The agent programs liveSessionRunsProgram recognises.
const (
	liveSessionProgramCodex  = "codex"
	liveSessionProgramClaude = "claude"
)

// liveSessionParentIDs is the session of an agent that started the caller's
// (`claude -p` run by Codex, `codex exec` run by Claude) and still runs above
// this hook. The inherited variable alone isn't proof: a terminal
// multiplexer's server started by Codex keeps it for whatever it starts
// later, so the agent's program must be an ancestor
// (liveSessionAgentAncestors). Codex's own hooks ignore Codex's variables:
// they run in its shared background server, whose environment isn't the
// session's. A Claude above that server still counts, the ancestor check
// deciding. A variable holding the hook's own session ID (Claude Code's
// own, inside Claude Code) needs no check.
func liveSessionParentIDs(ctx context.Context, ag agent.Agent, event *agent.Event, cache *liveSessionsCache) []string {
	if cache == nil {
		return nil
	}
	candidates := map[string][]string{}
	for _, v := range liveSessionParentVars {
		if v.program == liveSessionProgramCodex && ag.Name() == agent.AgentNameCodex {
			continue
		}
		id := os.Getenv(v.env)
		if id == "" || id == event.SessionID || !liveSessionCacheHasOwn(cache, id) {
			continue
		}
		candidates[v.program] = append(candidates[v.program], id)
	}
	if len(candidates) == 0 {
		return nil
	}
	programs := make([]string, 0, len(candidates))
	for program := range candidates {
		programs = append(programs, program)
	}
	slices.Sort(programs)
	var parents []string
	ancestors := liveSessionAgentAncestors(ctx, programs...)
	for _, program := range programs {
		if ancestors[program] {
			parents = append(parents, candidates[program]...)
		}
	}
	return parents
}

// liveSessionCacheHasOwn reports whether cache holds a session of the
// caller's account in id's family.
func liveSessionCacheHasOwn(cache *liveSessionsCache, id string) bool {
	mine, _ := cache.callers()
	for _, e := range cache.Sessions {
		if mine(e) && liveSessionInFamily(e.SessionID, []string{id}) {
			return true
		}
	}
	return false
}

// liveSessionAncestorsMax bounds the walk up this process's parents.
const liveSessionAncestorsMax = 64

// liveSessionProcess is one process as the ancestor walk sees it.
type liveSessionProcess struct {
	ppid int
	argv []string
}

// liveSessionAgentAncestors reports which of programs ("codex", "claude")
// run as ancestors of this process (liveSessionRunsProgram). Where there is a
// /proc (Linux) it reads it along the chain; elsewhere one `ps` listing (about 20 ms,
// bounded to a second). Where that can't be told, every one is taken as an
// ancestor and the inherited ID trusted as given: on Windows, and when the
// process table can't be read (Codex's macOS sandbox refuses /bin/ps). A
// var for tests.
var liveSessionAgentAncestors = func(ctx context.Context, programs ...string) map[string]bool {
	all := map[string]bool{}
	for _, p := range programs {
		all[p] = true
	}
	if runtime.GOOS == windowsGOOS {
		return all
	}
	lookup := liveSessionProcFS
	if _, ok := liveSessionProcFS(os.Getpid()); !ok { // no /proc (not Linux)
		table, ok := liveSessionPSTable(ctx)
		if !ok {
			return all
		}
		lookup = func(pid int) (liveSessionProcess, bool) {
			p, ok := table[pid]
			return p, ok
		}
	}
	found := map[string]bool{}
	pid := os.Getppid()
	for walked := 0; walked < liveSessionAncestorsMax && pid > 1; walked++ {
		p, ok := lookup(pid)
		if !ok {
			if walked == 0 {
				return all // not even our parent: the table can't be read
			}
			break
		}
		for _, program := range programs {
			if liveSessionRunsProgram(p.argv, program) {
				found[program] = true
			}
		}
		pid = p.ppid
	}
	return found
}

// liveSessionProcFS reads pid's parent and argv from /proc.
func liveSessionProcFS(pid int) (liveSessionProcess, bool) {
	dir := "/proc/" + strconv.Itoa(pid)
	stat, err := os.ReadFile(dir + "/stat") //nolint:gosec // a /proc path built from a pid
	if err != nil {
		return liveSessionProcess{}, false
	}
	// pid (comm) state ppid …: comm may hold spaces and parentheses, so
	// the fields are counted from its last ')'.
	i := bytes.LastIndexByte(stat, ')')
	if i < 0 {
		return liveSessionProcess{}, false
	}
	fields := strings.Fields(string(stat[i+1:]))
	if len(fields) < 2 {
		return liveSessionProcess{}, false
	}
	ppid, err := strconv.Atoi(fields[1])
	if err != nil {
		return liveSessionProcess{}, false
	}
	cmdline, err := os.ReadFile(dir + "/cmdline") //nolint:gosec // a /proc path built from a pid
	if err != nil {
		return liveSessionProcess{}, false
	}
	argv := strings.Split(strings.TrimRight(string(cmdline), "\x00"), "\x00")
	return liveSessionProcess{ppid: ppid, argv: argv}, true
}

// liveSessionPSTable lists every process with `ps`: once for the executable
// path (comm, the full path on macOS, spaces and all, as in Claude's desktop
// app's …/Library/Application Support/Claude/…/claude), once for the
// arguments, which only a script runtime's need (parseLiveSessionPSTable).
func liveSessionPSTable(ctx context.Context) (map[int]liveSessionProcess, bool) {
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	comm, err := exec.CommandContext(ctx, "ps", "-A", "-o", "pid=,ppid=,comm=").Output()
	if err != nil {
		return nil, false
	}
	args, err := exec.CommandContext(ctx, "ps", "-A", "-o", "pid=,ppid=,args=").Output()
	if err != nil {
		args = nil
	}
	procs := parseLiveSessionPSTable(string(comm), string(args))
	return procs, len(procs) > 0
}

// parseLiveSessionPSTable builds the process table from ps's
// "pid ppid comm" and "pid ppid args" listings: argv[0] is the whole
// executable path from comm; the rest is args past that path, split on
// spaces (so a script runtime's script path with a space isn't recognised).
func parseLiveSessionPSTable(comm, args string) map[int]liveSessionProcess {
	rest := map[int]string{}
	for _, line := range strings.Split(args, "\n") {
		if pid, _, tail, ok := splitPSLine(line); ok {
			rest[pid] = tail
		}
	}
	procs := map[int]liveSessionProcess{}
	for _, line := range strings.Split(comm, "\n") {
		pid, ppid, exe, ok := splitPSLine(line)
		if !ok || exe == "" {
			continue
		}
		argv := []string{exe}
		tail := rest[pid]
		if after, found := strings.CutPrefix(tail, exe); found {
			argv = append(argv, strings.Fields(after)...)
		} else if f := strings.Fields(tail); len(f) > 1 {
			argv = append(argv, f[1:]...)
		}
		procs[pid] = liveSessionProcess{ppid: ppid, argv: argv}
	}
	return procs
}

// splitPSLine splits "  pid  ppid rest" into its parts, rest trimmed.
func splitPSLine(line string) (pid, ppid int, rest string, ok bool) {
	f := strings.Fields(line)
	if len(f) < 2 {
		return 0, 0, "", false
	}
	pid, err1 := strconv.Atoi(f[0])
	ppid, err2 := strconv.Atoi(f[1])
	if err1 != nil || err2 != nil {
		return 0, 0, "", false
	}
	line = strings.TrimSpace(line)
	line = strings.TrimSpace(strings.TrimPrefix(line, f[0]))
	line = strings.TrimSpace(strings.TrimPrefix(line, f[1]))
	return pid, ppid, line, true
}

// liveSessionRunsProgram reports whether a process with argv runs program,
// judged by the executable only, never by an argument a shell or editor was
// given (`vim claude`, `zsh ~/src/codex/dev.sh`):
//   - its executable is named program (`codex`, `codex app-server`,
//     `claude`);
//   - Claude Code's native install: …/claude/versions/<version>;
//   - Codex's npm package's vendored binary: …/@openai/codex/…;
//   - a script runtime (node, bun, deno) running the agent's npm package
//     (…/@anthropic-ai/claude-code/…, …/@openai/codex/…) or a script named
//     program (the npm bin link, `node /usr/local/bin/claude`).
func liveSessionRunsProgram(argv []string, program string) bool {
	if len(argv) == 0 {
		return false
	}
	exe := filepath.ToSlash(argv[0])
	if filepath.Base(exe) == program {
		return true
	}
	pkg := map[string]string{liveSessionProgramClaude: "/@anthropic-ai/claude-code/", liveSessionProgramCodex: "/@openai/codex/"}[program]
	switch {
	case program == liveSessionProgramClaude && strings.Contains(exe, "/claude/versions/"):
		return true
	case program == liveSessionProgramCodex && strings.Contains(exe, pkg):
		return true
	}
	switch filepath.Base(exe) {
	case "node", "bun", "deno":
		if len(argv) < 2 {
			return false
		}
		script := filepath.ToSlash(argv[1])
		return filepath.Base(script) == program || (pkg != "" && strings.Contains(script, pkg))
	}
	return false
}
