package agent

// CapabilityDeclarer is implemented by agents that declare their capabilities
// at registration time (e.g., external plugin agents). The As* helper functions
// below use this interface to gate capability access: an agent must both implement
// the optional interface AND declare the capability as true.
//
// Built-in agents (Claude Code, Gemini CLI, etc.) do NOT implement this interface.
// For those agents, the As* helpers fall through to a direct type assertion,
// preserving existing behavior.
type CapabilityDeclarer interface {
	DeclaredCapabilities() DeclaredCaps
}

// DeclaredCaps enumerates the optional interfaces an agent claims to support.
// JSON tags match the external agent protocol schema so external.InfoResponse
// can deserialize directly into this type.
//
// Not every optional interface appears here: built-in-only capabilities that
// have no external-protocol equivalent (SessionBaseDirProvider, ModelExtractor,
// SkillEventExtractor, TranscriptSanitizer) are intentionally excluded — their
// As* helpers resolve by type assertion alone (see builtinCapability), with no
// DeclaredCaps gate.
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

// builtinCapability returns the agent as T by type assertion alone, for
// built-in-only capabilities that have no DeclaredCaps gate.
func builtinCapability[T any](ag Agent) (T, bool) {
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
	if ag == nil {
		return nil, false
	}
	p, ok := ag.(SidecarImageProvider)
	return p, ok
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

// AsTokenCalculator returns the agent as TokenCalculator if it both
// implements the interface and (for CapabilityDeclarer agents) has declared the capability.
func AsTokenCalculator(ag Agent) (TokenCalculator, bool) {
	return declaredCapability[TokenCalculator](ag, func(c DeclaredCaps) bool { return c.TokenCalculator })
}

// AsTextGenerator returns the agent as TextGenerator if it both
// implements the interface and (for CapabilityDeclarer agents) has declared the capability.
func AsTextGenerator(ag Agent) (TextGenerator, bool) {
	return declaredCapability[TextGenerator](ag, func(c DeclaredCaps) bool { return c.TextGenerator })
}

// AsStreamingTextGenerator returns the agent as StreamingTextGenerator if it both
// implements the interface and (for CapabilityDeclarer agents) has declared the capability.
func AsStreamingTextGenerator(ag Agent) (StreamingTextGenerator, bool) {
	if ag == nil {
		return nil, false
	}
	stg, ok := ag.(StreamingTextGenerator)
	if !ok {
		return nil, false
	}
	if cd, ok := ag.(CapabilityDeclarer); ok {
		return stg, cd.DeclaredCapabilities().StreamingTextGenerator
	}
	return stg, true
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

// AsSubagentAwareExtractor returns the agent as SubagentAwareExtractor if it both
// implements the interface and (for CapabilityDeclarer agents) has declared the capability.
func AsSubagentAwareExtractor(ag Agent) (SubagentAwareExtractor, bool) {
	return declaredCapability[SubagentAwareExtractor](ag, func(c DeclaredCaps) bool { return c.SubagentAwareExtractor })
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
