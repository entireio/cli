package cli

import (
	"strings"
	"testing"
)

// entireContextInjection is emitted raw into the agent's model context, so a
// repo key carrying control characters must never reach that sink — it degrades
// to the generic message instead (parity with agentHelpRepoBlock's defense).
func TestEntireContextInjection_StripsControlChars(t *testing.T) {
	t.Parallel()

	clean := entireContextInjection(trailEnablementScope{Forge: "gh", Owner: "acme", Repo: "app"}, true, false)
	if !strings.Contains(clean, "gh/acme/app") {
		t.Errorf("a clean scope should embed the repo key, got: %s", clean)
	}

	tampered := entireContextInjection(trailEnablementScope{Forge: "gh", Owner: "acme", Repo: "app\n\x1b[31mX"}, true, false)
	if strings.ContainsAny(tampered, "\n\x1b") {
		t.Errorf("control characters must not reach the injected string, got: %q", tampered)
	}
	if !strings.HasPrefix(tampered, "Entire is enabled for this repo. ") {
		t.Errorf("a tampered scope should degrade to the generic message, got: %s", tampered)
	}
}
