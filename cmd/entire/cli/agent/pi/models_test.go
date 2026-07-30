package pi

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
)

func TestParsePiModelList(t *testing.T) {
	raw := "provider      model                       context  max-out  thinking  images\n" +
		"anthropic     claude-opus-4-0             200K     32K      yes       yes   \n" +
		"openai        gpt-5                       400K     128K     yes       no    \n" +
		"\n" +
		"google        gemini-2.5-pro              1M       64K      yes       yes   \n"

	got := parsePiModelList(raw)
	if len(got) != 3 {
		t.Fatalf("parsed %d models, want 3: %#v", len(got), got)
	}
	want := []struct{ id, note string }{
		{"anthropic/claude-opus-4-0", "200K ctx"},
		{"openai/gpt-5", "400K ctx"},
		{"google/gemini-2.5-pro", "1M ctx"},
	}
	for i, w := range want {
		if got[i].ID != w.id {
			t.Errorf("model[%d].ID = %q, want %q", i, got[i].ID, w.id)
		}
		if got[i].Note != w.note {
			t.Errorf("model[%d].Note = %q, want %q", i, got[i].Note, w.note)
		}
	}
}

func TestParsePiModelList_HeaderAndBlanksSkipped(t *testing.T) {
	if got := parsePiModelList("provider model\n\n   \n"); len(got) != 0 {
		t.Fatalf("expected no models, got %#v", got)
	}
}

// TestListModels_PreservesCauseAndStaysUntyped pins two things about the
// ListModels error path that are easy to get wrong in opposite directions.
//
//  1. The cause survives. It is wrapped with %w, not interpolated with %s, so
//     callers can still ask errors.Is(err, exec.ErrNotFound) — i.e. "is pi
//     installed?" — programmatically.
//  2. It is NOT an *agent.TextGenError. Listing models is not summary
//     generation; routing it through the summary classifier would let a
//     model-listing failure render as "Pi failed to generate the summary" if it
//     ever reached formatCheckpointSummaryError.
func TestListModels_PreservesCauseAndStaysUntyped(t *testing.T) {
	t.Parallel()
	a := &PiAgent{CommandRunner: func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		return exec.CommandContext(ctx, "definitely-no-such-binary-xyz")
	}}
	_, err := a.ListModels(context.Background())
	if err == nil {
		t.Fatal("expected an error")
	}
	if !errors.Is(err, exec.ErrNotFound) {
		t.Errorf("errors.Is(exec.ErrNotFound) = false; the cause was dropped from the chain: %v", err)
	}
	var tge *agent.TextGenError
	if errors.As(err, &tge) {
		t.Error("ListModels must not return *agent.TextGenError; that is the summary-path error surface")
	}
	if !strings.Contains(err.Error(), "pi --list-models") {
		t.Errorf("err = %v; want the operation named", err)
	}
	// The cause must appear once, not twice.
	if strings.Count(err.Error(), "executable file not found") != 1 {
		t.Errorf("err = %q; cause should appear exactly once", err.Error())
	}
}
