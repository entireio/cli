package geminicli_test

import (
	"context"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/geminicli"
)

// Compile-time pin: GeminiCLIAgent must satisfy SkillDiscoverer.
var _ agent.SkillDiscoverer = (*geminicli.GeminiCLIAgent)(nil)

func TestGeminiCLIAgent_DiscoverReviewSkills_Stub(t *testing.T) {
	t.Parallel()
	a := &geminicli.GeminiCLIAgent{}
	skills, err := a.DiscoverReviewSkills(context.Background())
	if err != nil {
		t.Fatalf("stub should not error; got %v", err)
	}
	if skills != nil {
		t.Errorf("stub should return nil skills; got %+v", skills)
	}
}
