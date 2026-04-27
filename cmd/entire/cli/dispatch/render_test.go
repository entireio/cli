package dispatch

import "testing"

func TestRenderMarkdown_PassesThroughGeneratedText(t *testing.T) {
	t.Parallel()

	got := RenderMarkdown(&Dispatch{GeneratedText: "  # hello\n\n"})
	if got != "# hello\n" {
		t.Fatalf("unexpected passthrough output: %q", got)
	}
}

func TestRenderMarkdown_NilReturnsEmpty(t *testing.T) {
	t.Parallel()

	if got := RenderMarkdown(nil); got != "" {
		t.Fatalf("expected empty string for nil dispatch, got %q", got)
	}
}
