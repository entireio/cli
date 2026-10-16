package agent

// CapabilityDeclarer is implemented by agents that declare their capabilities
// at registration time (e.g., external plugin agents). Each As* helper resolves
// through one of three functions, which treat a CapabilityDeclarer differently:
//
//   - declaredCapability: it must implement the interface AND declare the
//     matching DeclaredCaps field as true.
//   - builtinCapability: implementing the interface is enough; there is no
//     DeclaredCaps field to check.
//   - nonDeclarerCapability: it is refused even when it implements the
//     interface.
//
// Built-in agents (Claude Code, Codex, etc.) do NOT implement this interface,
// so for them every helper is a plain type assertion.
type CapabilityDeclarer interface {
	DeclaredCapabilities() DeclaredCaps
}

// DeclaredCaps enumerates the optional interfaces an agent claims to support.
// JSON tags match the external agent protocol schema so external.InfoResponse
// can deserialize directly into this type.
//
// Not every optional interface appears here. These capabilities intentionally
// have no DeclaredCaps field:
//
//   - CallerSessionIdentifier
//   - ContextInjector
//   - EffectiveHookDiagnostics
//   - HomeLayoutProvider
//   - HomeScopedInventoryExtractor
//   - HookConfigLocator
//   - HookFreshness
//   - HookTimeoutRepairer
//   - InventoryAwareExtractor
//   - LateTranscriptWriter (refuses CapabilityDeclarer agents)
//   - ModelExtractor
//   - ModelLister
//   - OutOfBandTokenSource (refuses CapabilityDeclarer agents)
//   - PermissionConfigOwner
//   - ProtectedFilesProvider
//   - RestoredSessionPathResolver
//   - SessionBaseDirProvider
//   - SessionEndBudgeter
//   - SessionFileCandidatesProvider
//   - SidecarImageProvider
//   - SkillDiscoverer
//   - SkillEventExtractor
//   - StaleHookReporter
//   - SubagentSessionResolver
//   - SubagentTranscriptFetcher
//   - TaskTranscriptMatcher
//   - ToolInvocationScanner
//   - TranscriptFetcher
//   - TranscriptSanitizer
//   - TurnEndHookTimeouter
//
// Their As* helpers resolve through builtinCapability, or through
// nonDeclarerCapability for the entries marked as refusing CapabilityDeclarer
// agents. Callers reach every capability through its As* helper, never by
// type-asserting an agent directly.
type DeclaredCaps struct {
	Hooks                  bool `json:"hooks"`
	TranscriptAnalyzer     bool `json:"transcript_analyzer"`
	TranscriptPreparer     bool `json:"transcript_preparer"`
	TokenCalculator        bool `json:"token_calculator"`
	CompactTranscript      bool `json:"compact_transcript"`
	TextGenerator          bool `json:"text_generator"`
	StreamingTextGenerator bool `json:"streaming_text_generator"`
	HookResponseWriter     bool `json:"hook_response_writer"`
	SubagentAwareExtractor bool `json:"subagent_aware_extractor"`
}

// declaredCapability returns the agent as T if it both implements T and (for
// CapabilityDeclarer agents) has the capability selected by declared set to true.
func declaredCapability[T any](ag Agent, declared func(DeclaredCaps) bool) (T, bool) {
	t, ok := builtinCapability[T](ag)
	if !ok {
		return t, false
	}
	if cd, ok := ag.(CapabilityDeclarer); ok {
		return t, declared(cd.DeclaredCapabilities())
	}
	return t, true
}

// builtinCapability returns the agent as T by type assertion alone, with no
// DeclaredCaps gate. CapabilityDeclarer agents that implement T are not
// refused; use nonDeclarerCapability when they must be. ag is a SessionLocator,
// the narrowest view of an agent a helper resolves from, so every Agent and a
// SessionStore's locator both fit while unrelated values are rejected at
// compile time.
func builtinCapability[T any](ag SessionLocator) (T, bool) {
	var zero T
	if ag == nil {
		return zero, false
	}
	t, ok := ag.(T)
	if !ok {
		return zero, false
	}
	return t, true
}

// nonDeclarerCapability returns the agent as T like builtinCapability, but
// refuses CapabilityDeclarer agents even when they implement T, for
// capabilities external agents must never take on.
func nonDeclarerCapability[T any](ag Agent) (T, bool) {
	if _, isDeclarer := ag.(CapabilityDeclarer); isDeclarer {
		var zero T
		return zero, false
	}
	return builtinCapability[T](ag)
}

// AsHookSupport returns the agent as HookSupport if it both implements the
// interface and (for CapabilityDeclarer agents) has declared the capability.
func AsHookSupport(ag Agent) (HookSupport, bool) {
	return declaredCapability[HookSupport](ag, func(c DeclaredCaps) bool { return c.Hooks })
}

// AsHookFreshness returns the agent as HookFreshness if it implements the
// interface. No capability declaration is needed: hook-config drift detection
// is built-in only, since it compares against a template the CLI itself
// embeds. External agents own their hook config and report installation state
// through their own protocol.
func AsHookFreshness(ag Agent) (HookFreshness, bool) {
	return builtinCapability[HookFreshness](ag)
}

// AsStaleHookReporter returns the agent as StaleHookReporter if it can report
// stale Entire hooks its next install will remove.
func AsStaleHookReporter(ag Agent) (StaleHookReporter, bool) {
	return builtinCapability[StaleHookReporter](ag)
}

// AsHookTimeoutRepairer returns the agent as HookTimeoutRepairer if it can
// add the hook timeouts an older CLI wrote its config without.
func AsHookTimeoutRepairer(ag Agent) (HookTimeoutRepairer, bool) {
	return builtinCapability[HookTimeoutRepairer](ag)
}

// AsEffectiveHookDiagnostics returns the agent as EffectiveHookDiagnostics if
// it owns diagnostics for its effective hook configuration.
func AsEffectiveHookDiagnostics(ag Agent) (EffectiveHookDiagnostics, bool) {
	return builtinCapability[EffectiveHookDiagnostics](ag)
}

// AsTranscriptAnalyzer returns the agent as TranscriptAnalyzer if it both
// implements the interface and (for CapabilityDeclarer agents) has declared the capability.
func AsTranscriptAnalyzer(ag Agent) (TranscriptAnalyzer, bool) {
	return declaredCapability[TranscriptAnalyzer](ag, func(c DeclaredCaps) bool { return c.TranscriptAnalyzer })
}

// AsTranscriptPreparer returns the agent as TranscriptPreparer if it both
// implements the interface and (for CapabilityDeclarer agents) has declared the capability.
func AsTranscriptPreparer(ag Agent) (TranscriptPreparer, bool) {
	return declaredCapability[TranscriptPreparer](ag, func(c DeclaredCaps) bool { return c.TranscriptPreparer })
}

// AsSidecarImageProvider returns the agent as SidecarImageProvider if it
// implements the interface. This is a best-effort, optional capability (image
// capture from a store outside the transcript, e.g. Cursor's SQLite blob store),
// so it resolves by type assertion alone with no DeclaredCaps gate.
func AsSidecarImageProvider(ag Agent) (SidecarImageProvider, bool) {
	return builtinCapability[SidecarImageProvider](ag)
}

// AsProtectedFilesProvider returns the agent as ProtectedFilesProvider if it
// implements the interface. External agents report protected files through
// their info response, which the external adapter forwards, so there is no
// DeclaredCaps gate.
func AsProtectedFilesProvider(ag Agent) (ProtectedFilesProvider, bool) {
	return builtinCapability[ProtectedFilesProvider](ag)
}

// AsRestoredSessionPathResolver returns the agent as RestoredSessionPathResolver
// if it implements the interface. The external protocol has no restored-path
// hook, so there is no DeclaredCaps gate.
func AsRestoredSessionPathResolver(ag Agent) (RestoredSessionPathResolver, bool) {
	return builtinCapability[RestoredSessionPathResolver](ag)
}

// AsSkillDiscoverer returns the agent as SkillDiscoverer if it implements the
// interface. The review skill picker and configured-skill validation both use
// it. The external protocol has no skill discovery, so there is no DeclaredCaps
// gate.
func AsSkillDiscoverer(ag Agent) (SkillDiscoverer, bool) {
	return builtinCapability[SkillDiscoverer](ag)
}

// AsTranscriptSanitizer returns the agent as TranscriptSanitizer if it implements
// the interface. This is a pure local byte transform with no external process to
// negotiate with, so it needs no DeclaredCaps gate.
func AsTranscriptSanitizer(ag Agent) (TranscriptSanitizer, bool) {
	return builtinCapability[TranscriptSanitizer](ag)
}

// SanitizeTranscriptForStorage applies the agent's storage sanitizer when it has one
// and returns data unchanged otherwise. Every path that stores a transcript copy
// should call this BEFORE redaction — see TranscriptSanitizer for why. A no-op for
// agents without the capability and idempotent for those with it, so it is safe to
// call on any transcript from any path.
func SanitizeTranscriptForStorage(ag Agent, data []byte) []byte {
	if len(data) == 0 {
		return data
	}
	s, ok := AsTranscriptSanitizer(ag)
	if !ok {
		return data
	}
	sanitized := s.SanitizeTranscriptForStorage(data)
	if sanitized == nil {
		// Defensive: the interface forbids this, but a nil return would silently
		// drop the whole session. Prefer the unsanitized transcript over none.
		return data
	}
	return sanitized
}

// AsTranscriptFetcher returns the agent as TranscriptFetcher if it implements
// the interface. This is an optional capability (materializing a transcript on
// demand for sessions with no hook-cached file), so it resolves by type
// assertion alone with no DeclaredCaps gate.
func AsTranscriptFetcher(ag Agent) (TranscriptFetcher, bool) {
	return builtinCapability[TranscriptFetcher](ag)
}

// AsSubagentTranscriptFetcher returns the agent as SubagentTranscriptFetcher if
// it implements the interface. Built-in only, like TranscriptFetcher.
func AsSubagentTranscriptFetcher(ag Agent) (SubagentTranscriptFetcher, bool) {
	return builtinCapability[SubagentTranscriptFetcher](ag)
}

// AsTokenCalculator returns the agent as TokenCalculator if it both
// implements the interface and (for CapabilityDeclarer agents) has declared the capability.
func AsTokenCalculator(ag Agent) (TokenCalculator, bool) {
	return declaredCapability[TokenCalculator](ag, func(c DeclaredCaps) bool { return c.TokenCalculator })
}

// AsLateTranscriptWriter returns the agent as LateTranscriptWriter if supported.
// External (CapabilityDeclarer) agents are excluded: the late-transcript trait
// is wire-format knowledge the external protocol does not currently express,
// and DeclaredCaps has no field for this capability to opt into.
func AsLateTranscriptWriter(ag Agent) (LateTranscriptWriter, bool) {
	return nonDeclarerCapability[LateTranscriptWriter](ag)
}

// AsOutOfBandTokenSource returns the agent as OutOfBandTokenSource if supported.
// External (CapabilityDeclarer) agents are excluded because the out-of-band
// store is fed by a built-in shim subcommand they cannot provide, and
// DeclaredCaps has no field for this capability to opt into.
func AsOutOfBandTokenSource(ag Agent) (OutOfBandTokenSource, bool) {
	return nonDeclarerCapability[OutOfBandTokenSource](ag)
}

// AsInventoryAwareExtractor returns the agent as InventoryAwareExtractor when
// it implements the built-in-only inventory protocol. External agents cannot
// declare this capability because its authoritative child ledger is internal to
// Entire rather than the external-agent protocol.
func AsInventoryAwareExtractor(ag Agent) (InventoryAwareExtractor, bool) {
	return builtinCapability[InventoryAwareExtractor](ag)
}

// AsHomeScopedInventoryExtractor returns the agent as a
// HomeScopedInventoryExtractor when it implements the built-in-only interface.
func AsHomeScopedInventoryExtractor(ag Agent) (HomeScopedInventoryExtractor, bool) {
	return builtinCapability[HomeScopedInventoryExtractor](ag)
}

// AsTextGenerator returns the agent as TextGenerator if it both
// implements the interface and (for CapabilityDeclarer agents) has declared the capability.
func AsTextGenerator(ag Agent) (TextGenerator, bool) {
	return declaredCapability[TextGenerator](ag, func(c DeclaredCaps) bool { return c.TextGenerator })
}

// AsStreamingTextGenerator returns the agent as StreamingTextGenerator if it both
// implements the interface and (for CapabilityDeclarer agents) has declared the capability.
func AsStreamingTextGenerator(ag Agent) (StreamingTextGenerator, bool) {
	return declaredCapability[StreamingTextGenerator](ag, func(c DeclaredCaps) bool {
		return c.StreamingTextGenerator
	})
}

// AsTranscriptCompactor returns the agent as TranscriptCompactor if it both
// implements the interface and (for CapabilityDeclarer agents) has declared the capability.
func AsTranscriptCompactor(ag Agent) (TranscriptCompactor, bool) {
	return declaredCapability[TranscriptCompactor](ag, func(c DeclaredCaps) bool { return c.CompactTranscript })
}

// AsHookResponseWriter returns the agent as HookResponseWriter if it both
// implements the interface and (for CapabilityDeclarer agents) has declared the capability.
func AsHookResponseWriter(ag Agent) (HookResponseWriter, bool) {
	return declaredCapability[HookResponseWriter](ag, func(c DeclaredCaps) bool { return c.HookResponseWriter })
}

// AsPromptExtractor returns the agent as PromptExtractor if it both implements
// the interface and (for CapabilityDeclarer agents) has declared TranscriptAnalyzer.
// ExtractPrompts is conceptually part of transcript analysis, so it shares the same
// capability gate — this prevents calling extract-prompts on external agent binaries
// that never declared transcript_analyzer support.
func AsPromptExtractor(ag Agent) (PromptExtractor, bool) {
	return declaredCapability[PromptExtractor](ag, func(c DeclaredCaps) bool { return c.TranscriptAnalyzer })
}

// AsTranscriptPromptExtractor returns the agent as TranscriptPromptExtractor
// under the same capability gate as AsPromptExtractor: it is transcript
// analysis over bytes instead of a path.
func AsTranscriptPromptExtractor(ag Agent) (TranscriptPromptExtractor, bool) {
	return declaredCapability[TranscriptPromptExtractor](ag, func(c DeclaredCaps) bool { return c.TranscriptAnalyzer })
}

// AsSubagentAwareExtractor returns the agent as SubagentAwareExtractor if it both
// implements the interface and (for CapabilityDeclarer agents) has declared the capability.
func AsSubagentAwareExtractor(ag Agent) (SubagentAwareExtractor, bool) {
	return declaredCapability[SubagentAwareExtractor](ag, func(c DeclaredCaps) bool { return c.SubagentAwareExtractor })
}

// AsSubagentSessionResolver returns the agent as SubagentSessionResolver if it
// implements the interface. No capability declaration is needed: whether an
// agent's subagents run as sessions of their own is a property of the agent's
// own hook protocol, which only built-in agents model. External agents report
// subagent boundaries through their own hooks.
func AsSubagentSessionResolver(ag Agent) (SubagentSessionResolver, bool) {
	return builtinCapability[SubagentSessionResolver](ag)
}

// AsSessionBaseDirProvider returns the agent as SessionBaseDirProvider if it implements
// the interface. No capability declaration is needed since this is a built-in-only feature
// (external agents use the agent binary's own session resolution).
func AsSessionBaseDirProvider(ag Agent) (SessionBaseDirProvider, bool) {
	return builtinCapability[SessionBaseDirProvider](ag)
}

// AsModelExtractor returns the agent as ModelExtractor if it implements the
// interface. No capability declaration is needed: transcript-based model
// extraction is a built-in-only fallback for agents whose hooks omit the model
// (e.g., Pi). External agents report the model through their own hook protocol.
func AsModelExtractor(ag Agent) (ModelExtractor, bool) {
	return builtinCapability[ModelExtractor](ag)
}

// AsSkillEventExtractor returns the agent as SkillEventExtractor if it implements
// the interface. Skill-event extraction is currently built-in only; external
// agents do not expose this optional interface through declared capabilities.
func AsSkillEventExtractor(ag Agent) (SkillEventExtractor, bool) {
	return builtinCapability[SkillEventExtractor](ag)
}

// AsToolInvocationScanner returns the agent as ToolInvocationScanner if it
// implements the interface. Built-in only: reading tool calls out of a
// transcript needs knowledge of that transcript's shape, which an external
// agent's parse-hook does not convey.
func AsToolInvocationScanner(ag Agent) (ToolInvocationScanner, bool) {
	return builtinCapability[ToolInvocationScanner](ag)
}

// AsSessionEndBudgeter returns the agent as SessionEndBudgeter if it implements
// the interface. Built-in only because no external agent needs it yet — not
// because external agents could enforce a budget themselves: a plugin supplies
// only parse-hook, while the work being bounded (endSessionNow, the condense)
// runs in the entire process. Widen DeclaredCaps with a session_end_budget
// field when an external agent has a host that clamps its session-end hook.
func AsSessionEndBudgeter(ag Agent) (SessionEndBudgeter, bool) {
	return builtinCapability[SessionEndBudgeter](ag)
}

// AsTurnEndHookTimeouter returns the agent as TurnEndHookTimeouter if it
// implements the interface. Built-in only: the bounded work runs in the entire
// process, and the external-agent protocol carries no host hook timeout.
func AsTurnEndHookTimeouter(ag Agent) (TurnEndHookTimeouter, bool) {
	return builtinCapability[TurnEndHookTimeouter](ag)
}
