package summarize

import (
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/settings"
)

func TestResolveGenerator_NilSettings(t *testing.T) {
	t.Parallel()
	gen := ResolveGenerator(nil)
	if gen == nil {
		t.Fatal("expected non-nil generator")
	}
	if _, ok := gen.(*ClaudeGenerator); !ok {
		t.Errorf("expected *ClaudeGenerator, got %T", gen)
	}
}

func TestResolveGenerator_DefaultProvider(t *testing.T) {
	t.Parallel()
	s := &settings.EntireSettings{}
	gen := ResolveGenerator(s)
	if _, ok := gen.(*ClaudeGenerator); !ok {
		t.Errorf("expected *ClaudeGenerator for empty provider, got %T", gen)
	}
}

func TestResolveGenerator_ClaudeProvider(t *testing.T) {
	t.Parallel()
	s := &settings.EntireSettings{
		StrategyOptions: map[string]any{
			"summarize": map[string]any{
				"provider": "claude",
				"model":    "opus",
			},
		},
	}
	gen := ResolveGenerator(s)
	cg, ok := gen.(*ClaudeGenerator)
	if !ok {
		t.Fatalf("expected *ClaudeGenerator, got %T", gen)
	}
	if cg.Model != "opus" {
		t.Errorf("expected model opus, got %s", cg.Model)
	}
}

func TestResolveGenerator_OpenAIProvider(t *testing.T) {
	t.Parallel()
	s := &settings.EntireSettings{
		StrategyOptions: map[string]any{
			"summarize": map[string]any{
				"provider": "openai",
				"model":    "gpt-5-mini",
				"api_key":  "sk-test",
			},
		},
	}
	gen := ResolveGenerator(s)
	og, ok := gen.(*OpenAIGenerator)
	if !ok {
		t.Fatalf("expected *OpenAIGenerator, got %T", gen)
	}
	if og.Model != "gpt-5-mini" {
		t.Errorf("expected model gpt-5-mini, got %s", og.Model)
	}
	if og.APIKey != "sk-test" {
		t.Errorf("expected api_key sk-test, got %s", og.APIKey)
	}
}

func TestScopeTranscript_ClaudeLineBased(t *testing.T) {
	t.Parallel()
	transcript := []byte("line1\nline2\nline3\n")
	scoped := ScopeTranscript(transcript, 1, agent.AgentTypeClaudeCode)
	if string(scoped) != "line2\nline3\n" {
		t.Errorf("unexpected scoped transcript: %q", scoped)
	}
}

func TestScopeTranscript_ZeroOffset(t *testing.T) {
	t.Parallel()
	transcript := []byte("line1\nline2\n")
	scoped := ScopeTranscript(transcript, 0, agent.AgentTypeClaudeCode)
	if string(scoped) != "line1\nline2\n" {
		t.Errorf("expected full transcript, got: %q", scoped)
	}
}
