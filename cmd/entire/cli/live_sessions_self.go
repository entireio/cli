package cli

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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

// owns reports whether e is one of the caller's own sessions.
func (s liveSessionsSelf) owns(e liveSessionEntry) bool {
	return liveSessionInFamily(e.SessionID, s.Own) || (e.You && liveSessionInFamily(e.SessionID, s.Parents))
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
// what the block is built from: the parent check, which runs ps, runs only
// when a session of the caller's account in it carries a parent's ID.
func liveSessionSelfFor(ctx context.Context, ag agent.Agent, event *agent.Event, cache *liveSessionsCache) liveSessionsSelf {
	return liveSessionsSelf{Own: liveSessionOwnIDs(ag, event), Parents: liveSessionParentIDs(ctx, ag, cache)}
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

// liveSessionParentEnv is, per agent program, the variable through which it
// passes its session ID to the processes it starts.
var liveSessionParentEnv = map[string]string{
	"codex":  "CODEX_THREAD_ID",
	"claude": "CLAUDE_CODE_SESSION_ID",
}

// liveSessionParentIDs is the session of an agent that started the caller's
// (`claude -p` run by Codex, say) and still runs above this hook. The
// inherited variable alone isn't proof: a terminal multiplexer's server
// started by Codex keeps it for whatever it starts later, so the agent must
// be an ancestor (liveSessionAgentAncestors). Never for Codex's own hooks:
// they run in Codex's shared background server, whose environment isn't
// the session's.
func liveSessionParentIDs(ctx context.Context, ag agent.Agent, cache *liveSessionsCache) []string {
	if ag.Name() == agent.AgentNameCodex || cache == nil {
		return nil
	}
	ids := map[string]string{}
	var programs []string
	for program, env := range liveSessionParentEnv {
		id := os.Getenv(env)
		if id == "" || !liveSessionCacheHasOwn(cache, id) {
			continue
		}
		ids[program] = id
		programs = append(programs, program)
	}
	if len(programs) == 0 {
		return nil
	}
	var parents []string
	for program := range liveSessionAgentAncestors(ctx, programs...) {
		if id, ok := ids[program]; ok {
			parents = append(parents, id)
		}
	}
	return parents
}

// liveSessionCacheHasOwn reports whether cache holds a session of the
// caller's account in id's family.
func liveSessionCacheHasOwn(cache *liveSessionsCache, id string) bool {
	for _, e := range cache.Sessions {
		if e.You && liveSessionInFamily(e.SessionID, []string{id}) {
			return true
		}
	}
	return false
}

// liveSessionAncestorsMax bounds the walk up this process's parents.
const liveSessionAncestorsMax = 64

// liveSessionAgentAncestors reports which of programs ("codex", "claude")
// run as ancestors of this process, from one `ps` listing (about 20 ms,
// bounded to a second). Where that can't be told, every one is taken as an
// ancestor and the inherited ID trusted as given: on Windows, and when ps
// can't run (Codex's macOS sandbox refuses /bin/ps). The same check as
// entire-stream's per-edit one (entireio/entire-stream#70). A var for
// tests.
var liveSessionAgentAncestors = func(ctx context.Context, programs ...string) map[string]bool {
	all := func() map[string]bool {
		found := map[string]bool{}
		for _, p := range programs {
			found[p] = true
		}
		return found
	}
	if runtime.GOOS == windowsGOOS {
		return all()
	}
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ps", "-A", "-o", "pid=,ppid=,command=").Output()
	if err != nil {
		return all()
	}
	type proc struct {
		ppid    int
		command string
	}
	procs := map[int]proc{}
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) < 3 {
			continue
		}
		pid, err1 := strconv.Atoi(f[0])
		ppid, err2 := strconv.Atoi(f[1])
		if err1 == nil && err2 == nil {
			procs[pid] = proc{ppid, strings.Join(f[2:], " ")}
		}
	}
	if len(procs) == 0 {
		return all()
	}
	found := map[string]bool{}
	pid := os.Getppid()
	for range liveSessionAncestorsMax {
		p, ok := procs[pid]
		if !ok || pid <= 1 {
			break
		}
		for _, program := range programs {
			if liveSessionRunsProgram(p.command, program) {
				found[program] = true
			}
		}
		pid = p.ppid
	}
	return found
}

// liveSessionRunsProgram reports whether a process's command line runs
// program: its program is named so (codex, claude, node …/claude), or its
// program's path is in a folder named so (Claude Code's
// …/claude/versions/2.1.x).
func liveSessionRunsProgram(command, program string) bool {
	for i, f := range strings.Fields(command) {
		if i > 1 {
			break // the program, or an interpreter's script
		}
		if filepath.Base(f) == program || strings.Contains(filepath.ToSlash(f), "/"+program+"/") {
			return true
		}
	}
	return false
}
