package agent

import (
	"os"
	"slices"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/validation"
)

// CallerSessionIdentifier is implemented by agents that publish their own
// session ID into the environment of the processes they spawn — the shell tool
// an agent runs `entire` from, and any hook or git hook underneath it.
//
// It answers a question nothing else can: "which session is running me right
// now?" Session state is keyed on the agent's session ID, so an agent that
// hands its ID to its children lets a command identify its caller exactly,
// rather than inferring one from which state file moved most recently — which
// is a different question with a different answer whenever more than one
// session shares a checkpoint store (see strategy.ResolveCallerSession).
//
// Not every agent publishes one. Gemini CLI passes its session ID to its shell
// executor for background-process bookkeeping but never into the child
// environment, and opencode's shell tool performs no environment augmentation
// at all; both are absent here on purpose rather than by omission, and callers
// must degrade rather than assume.
//
// Deliberately built-in only, so it has no DeclaredCaps entry: the external
// agent protocol has no field for it, and an external plugin already receives
// its session ID through the hook payload it is handed.
type CallerSessionIdentifier interface {
	// CallerSessionEnvVar returns the environment variable this agent
	// publishes its session ID under.
	//
	// The agent declares the name and the package does the reading, so
	// validation happens in exactly one place and the set of names is
	// enumerable — which is what lets tests clear them all and keeps a future
	// diagnostic from re-deriving the list.
	CallerSessionEnvVar() string
}

// AsCallerSessionIdentifier returns the agent as CallerSessionIdentifier if it
// implements it. Built-in-only capability, so no DeclaredCaps gate.
func AsCallerSessionIdentifier(ag Agent) (CallerSessionIdentifier, bool) {
	return builtinCapability[CallerSessionIdentifier](ag)
}

// callerSessionAgent pairs an agent with its caller-session capability, so an
// enumeration resolves both once instead of asserting or re-instantiating per
// consumer.
type callerSessionAgent struct {
	agent Agent
	ident CallerSessionIdentifier
}

// callerSessionAgents returns every registered agent that publishes a session
// ID, in sorted agent-name order.
//
// Copies the factories under one read lock and instantiates outside it, the
// same shape as AllProtectedDirs and its siblings: one lock span per
// enumeration rather than a List() plus a Get() per agent, so the whole
// enumeration sees a single consistent registry snapshot.
func callerSessionAgents() []callerSessionAgent {
	registryMu.RLock()
	names := make([]types.AgentName, 0, len(registry))
	for name := range registry {
		names = append(names, name)
	}
	slices.Sort(names)
	factories := make([]Factory, 0, len(names))
	for _, name := range names {
		factories = append(factories, registry[name])
	}
	registryMu.RUnlock()

	out := make([]callerSessionAgent, 0, len(factories))
	for _, factory := range factories {
		ag := factory()
		if ident, ok := AsCallerSessionIdentifier(ag); ok {
			out = append(out, callerSessionAgent{agent: ag, ident: ident})
		}
	}
	return out
}

// callerSessionEnvVars is every variable name a built-in agent publishes.
//
// Static rather than registry-derived, which is the opposite of what it looks
// like it should be. Its consumers are test harnesses isolating themselves
// from the developer's real agent session, and not every test binary links
// every agent implementation — the e2e harness links eight of the nine. A
// registry-derived list therefore shortens to whatever that particular binary
// happened to import, **silently**: a missing name is not an error, it is one
// variable left set, so the developer's own session leaks into the spawned CLI
// and the failure surfaces as an unrelated assertion on their machine only.
// Registration cannot be the source of truth for "every name that exists".
//
// TestCallerSessionEnvVars_MatchesTheRegistry pins this against the live
// enumeration inside this package, where every agent IS registered, so the two
// cannot drift.
var callerSessionEnvVars = []string{
	"CLAUDE_CODE_SESSION_ID",
	"CODEX_SESSION_ID",
	"COPILOT_AGENT_SESSION_ID",
	"CURSOR_CONVERSATION_ID",
	"PI_SESSION_ID",
}

// CallerSessionEnvVars returns every caller-session variable name a built-in
// agent publishes, complete regardless of which agents the calling binary
// links. Use it to clear the set; see callerSessionEnvVars for why it is not
// derived from the registry.
func CallerSessionEnvVars() []string {
	return slices.Clone(callerSessionEnvVars)
}

// callerSessionEnvVarsFromRegistry enumerates the variable names of the agents
// registered in THIS binary. Only the guard test uses it — production and test
// isolation want the complete set above, not this binary's subset.
func callerSessionEnvVarsFromRegistry() []string {
	agents := callerSessionAgents()
	names := make([]string, 0, len(agents))
	for _, a := range agents {
		names = append(names, a.ident.CallerSessionEnvVar())
	}
	return names
}

// callerSessionIDFromEnv reads a session ID an agent published under name.
//
// The value is validated with validation.ValidateAgentSessionID, and this is
// not a formality: the ID it returns reaches ResolveSessionFile and becomes a
// path component, so an unvalidated one read straight out of the environment
// would be a traversal sink. An ID that fails validation is reported absent —
// the caller then degrades to a weaker tier, which is the same outcome as the
// variable never having been set, and strictly better than resolving a path
// from it.
func callerSessionIDFromEnv(name string) (string, bool) {
	id := strings.TrimSpace(os.Getenv(name))
	if validation.ValidateAgentSessionID(id) != nil {
		return "", false
	}
	return id, true
}

// CallerSessionCandidate is one agent's claim that it spawned this process.
//
// AgentType rather than the registry name, because the only consumer reports
// it to a user ("running inside Codex session X") and AgentType is the spelling
// session state and commit trailers already use.
type CallerSessionCandidate struct {
	SessionID string
	AgentType types.AgentType
}

// CallerSessionCandidates returns every registered agent's claim about which
// session spawned this process, sorted by agent name so the result is stable.
//
// More than one claim is normal rather than a conflict: agents nest, and a
// `codex exec` started from Claude Code's shell tool sees both agents'
// variables, since the outer one is inherited through the inner agent's own
// process. The environment alone cannot say which is nearer, so it does not
// try — resolving between candidates needs process ancestry, and belongs to
// the caller that has session state to match against.
func CallerSessionCandidates() []CallerSessionCandidate {
	agents := callerSessionAgents()
	candidates := make([]CallerSessionCandidate, 0, len(agents))
	for _, a := range agents {
		id, ok := callerSessionIDFromEnv(a.ident.CallerSessionEnvVar())
		if !ok {
			continue
		}
		candidates = append(candidates, CallerSessionCandidate{
			SessionID: id,
			AgentType: a.agent.Type(),
		})
	}
	return candidates
}
