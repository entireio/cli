package plugins

// Hook names subscribable via entire.on. These are the stable, documented
// identifiers third-party plugins bind to; they are decoupled from the internal
// agent.EventType enum so the enum can evolve without breaking plugins.
//
// Observer hooks run for side effects only and cannot change CLI behavior; a
// failing observer is logged and ignored. Mutating hooks (prepare_commit_msg,
// pre_push) can influence the outcome and are gated behind capabilities and
// explicit ordering — see docs/architecture/plugins-lua.md.
const (
	// HookSessionStart fires when an agent session begins.
	HookSessionStart = "session_start"
	// HookTurnStart fires when the user submits a prompt (turn begins).
	HookTurnStart = "turn_start"
	// HookTurnEnd fires after the agent finishes responding to a prompt.
	HookTurnEnd = "turn_end"
	// HookCheckpointSaved fires after a checkpoint step is written.
	HookCheckpointSaved = "checkpoint_saved"
	// HookPostCommit fires after a git commit that carries a checkpoint.
	HookPostCommit = "post_commit"
	// HookPrePush fires before a git push (observer variant).
	HookPrePush = "pre_push"
	// HookSubagentEnd fires after a subagent/task completes.
	HookSubagentEnd = "subagent_end"
	// HookSessionEnd fires when a session ends.
	HookSessionEnd = "session_end"
	// HookCompaction fires when the agent compacts its context window.
	HookCompaction = "compaction"
	// HookModelUpdate fires when the agent reports the active model.
	HookModelUpdate = "model_update"

	// HookPrepareCommitMsg is a mutating hook: callbacks may return a trailer
	// string appended to the commit message. Capability/ordering gated.
	HookPrepareCommitMsg = "prepare_commit_msg"
)

// observerHooks is the set of hooks that run for side effects only.
var observerHooks = map[string]struct{}{
	HookSessionStart:    {},
	HookTurnStart:       {},
	HookTurnEnd:         {},
	HookCheckpointSaved: {},
	HookPostCommit:      {},
	HookPrePush:         {},
	HookSubagentEnd:     {},
	HookSessionEnd:      {},
	HookCompaction:      {},
	HookModelUpdate:     {},
}

// mutatingHooks is the set of hooks whose callbacks can influence CLI behavior.
var mutatingHooks = map[string]struct{}{
	HookPrepareCommitMsg: {},
	// pre_push additionally supports a mutating (veto) variant; it is listed in
	// observerHooks for its observer semantics and treated as vetoable at the
	// mutating call site (see phase 4).
}

// IsKnownHook reports whether name is a hook plugins may subscribe to.
func IsKnownHook(name string) bool {
	if _, ok := observerHooks[name]; ok {
		return true
	}
	_, ok := mutatingHooks[name]
	return ok
}

// IsObserverHook reports whether name is an observer-only hook.
func IsObserverHook(name string) bool {
	_, ok := observerHooks[name]
	return ok
}

// KnownHooks returns all subscribable hook names (unordered).
func KnownHooks() []string {
	out := make([]string, 0, len(observerHooks)+len(mutatingHooks))
	for h := range observerHooks {
		out = append(out, h)
	}
	for h := range mutatingHooks {
		out = append(out, h)
	}
	return out
}
