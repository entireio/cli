package agent

import (
	"fmt"
	"strings"
	"testing"
)

func TestStreamingFallbackLogAttrs_DoNotContainRawStderr(t *testing.T) {
	t.Parallel()

	const secret = "prompt-content-secret"
	attrs := streamingFallbackLogAttrs("fake", "unknown flag: --stream "+secret)

	var rendered strings.Builder
	for _, attr := range attrs {
		fmt.Fprintf(&rendered, "%s=%v ", attr.Key, attr.Value.Any())
	}
	if strings.Contains(rendered.String(), secret) {
		t.Fatalf("fallback log attrs leaked raw stderr: %s", rendered.String())
	}
	if !strings.Contains(rendered.String(), "stderr_bytes=") {
		t.Fatalf("fallback log attrs should retain safe stderr size metadata: %s", rendered.String())
	}
}
