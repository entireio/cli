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
type DeclaredCaps struct {
	Hooks                  bool `json:"hooks"`
	TranscriptAnalyzer     bool `json:"transcript_analyzer"`
	TranscriptPreparer     bool `json:"transcript_preparer"`
	TokenCalculator        bool `json:"token_calculator"`
	TextGenerator          bool `json:"text_generator"`
	HookResponseWriter     bool `json:"hook_response_writer"`
	SubagentAwareExtractor bool `json:"subagent_aware_extractor"`
}

// AsHookSupport returns the agent as HookSupport if it both implements the
// interface and (for CapabilityDeclarer agents) has declared the capability.
func AsHookSupport(ag Agent) (HookSupport, bool) { //nolint:ireturn // type-assertion helper must return interface
	if ag == nil {
		return nil, false
	}
	hs, ok := ag.(HookSupport)
	if !ok {
		return nil, false
	}
	if cd, ok := ag.(CapabilityDeclarer); ok {
		return hs, cd.DeclaredCapabilities().Hooks
	}
	return hs, true
}

// AsTranscriptAnalyzer returns the agent as TranscriptAnalyzer if it both
// implements the interface and (for CapabilityDeclarer agents) has declared the capability.
func AsTranscriptAnalyzer(ag Agent) (TranscriptAnalyzer, bool) { //nolint:ireturn // type-assertion helper must return interface
	if ag == nil {
		return nil, false
	}
	ta, ok := ag.(TranscriptAnalyzer)
	if !ok {
		return nil, false
	}
	if cd, ok := ag.(CapabilityDeclarer); ok {
		return ta, cd.DeclaredCapabilities().TranscriptAnalyzer
	}
	return ta, true
}

// AsTranscriptPreparer returns the agent as TranscriptPreparer if it both
// implements the interface and (for CapabilityDeclarer agents) has declared the capability.
func AsTranscriptPreparer(ag Agent) (TranscriptPreparer, bool) { //nolint:ireturn // type-assertion helper must return interface
	if ag == nil {
		return nil, false
	}
	tp, ok := ag.(TranscriptPreparer)
	if !ok {
		return nil, false
	}
	if cd, ok := ag.(CapabilityDeclarer); ok {
		return tp, cd.DeclaredCapabilities().TranscriptPreparer
	}
	return tp, true
}

// AsTokenCalculator returns the agent as TokenCalculator if it both
// implements the interface and (for CapabilityDeclarer agents) has declared the capability.
func AsTokenCalculator(ag Agent) (TokenCalculator, bool) { //nolint:ireturn // type-assertion helper must return interface
	if ag == nil {
		return nil, false
	}
	tc, ok := ag.(TokenCalculator)
	if !ok {
		return nil, false
	}
	if cd, ok := ag.(CapabilityDeclarer); ok {
		return tc, cd.DeclaredCapabilities().TokenCalculator
	}
	return tc, true
}

// AsTextGenerator returns the agent as TextGenerator if it both
// implements the interface and (for CapabilityDeclarer agents) has declared the capability.
func AsTextGenerator(ag Agent) (TextGenerator, bool) { //nolint:ireturn // type-assertion helper must return interface
	if ag == nil {
		return nil, false
	}
	tg, ok := ag.(TextGenerator)
	if !ok {
		return nil, false
	}
	if cd, ok := ag.(CapabilityDeclarer); ok {
		return tg, cd.DeclaredCapabilities().TextGenerator
	}
	return tg, true
}

// AsHookResponseWriter returns the agent as HookResponseWriter if it both
// implements the interface and (for CapabilityDeclarer agents) has declared the capability.
func AsHookResponseWriter(ag Agent) (HookResponseWriter, bool) { //nolint:ireturn // type-assertion helper must return interface
	if ag == nil {
		return nil, false
	}
	hrw, ok := ag.(HookResponseWriter)
	if !ok {
		return nil, false
	}
	if cd, ok := ag.(CapabilityDeclarer); ok {
		return hrw, cd.DeclaredCapabilities().HookResponseWriter
	}
	return hrw, true
}

// AsSubagentAwareExtractor returns the agent as SubagentAwareExtractor if it both
// implements the interface and (for CapabilityDeclarer agents) has declared the capability.
func AsSubagentAwareExtractor(ag Agent) (SubagentAwareExtractor, bool) { //nolint:ireturn // type-assertion helper must return interface
	if ag == nil {
		return nil, false
	}
	sae, ok := ag.(SubagentAwareExtractor)
	if !ok {
		return nil, false
	}
	if cd, ok := ag.(CapabilityDeclarer); ok {
		return sae, cd.DeclaredCapabilities().SubagentAwareExtractor
	}
	return sae, true
}
