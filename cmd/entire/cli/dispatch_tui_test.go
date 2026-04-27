package cli

import (
	"context"
	"io"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	dispatchpkg "github.com/entireio/cli/cmd/entire/cli/dispatch"
)

type fakeDispatchProgram struct {
	model tea.Model
}

//nolint:ireturn // dispatchProgram interface contract (mirrors tea.Program)
func (p fakeDispatchProgram) Run() (tea.Model, error) {
	model, ok := p.model.(dispatchStatusModel)
	if !ok {
		return p.model, nil
	}
	model.result = dispatchRenderResult{markdown: "# generated dispatch\n"}
	return model, nil
}

func TestDefaultRunInteractiveDispatch_DoesNotUseAltScreen(t *testing.T) {
	t.Parallel()

	oldProgramFactory := newDispatchProgram
	newDispatchProgram = func(model tea.Model, _ io.Writer, altScreen bool) dispatchProgram {
		if altScreen {
			t.Fatal("did not expect alt-screen for dispatch loading state")
		}
		return fakeDispatchProgram{model: model}
	}
	t.Cleanup(func() {
		newDispatchProgram = oldProgramFactory
	})

	markdown, err := defaultRunInteractiveDispatch(context.Background(), io.Discard, dispatchpkg.Options{
		Since: "7d",
	})
	if err != nil {
		t.Fatal(err)
	}
	if markdown != "# generated dispatch\n" {
		t.Fatalf("unexpected markdown: %q", markdown)
	}
}

func TestDispatchStatusModel_ViewRendersInlineCard(t *testing.T) {
	t.Parallel()

	model := newDispatchStatusModel(io.Discard, dispatchpkg.Options{
		Since: "7d",
	}, func(context.Context) (string, error) {
		return "", nil
	})
	model.width = 80
	model.height = 24

	view := model.View()
	if !strings.HasPrefix(view, "\n") {
		t.Fatalf("expected inline view with a leading blank line: %q", view)
	}
	if strings.HasPrefix(strings.TrimPrefix(view, "\n"), " ") {
		t.Fatalf("expected inline view without leading padding: %q", view)
	}
	if got := strings.Count(view, "\n"); got >= 20 {
		t.Fatalf("expected compact inline card, got %d lines", got+1)
	}
}

func TestDefaultRunInteractiveDispatch_ClearsLoadingCardBeforeReturn(t *testing.T) {
	t.Parallel()

	oldProgramFactory := newDispatchProgram
	newDispatchProgram = func(model tea.Model, _ io.Writer, _ bool) dispatchProgram {
		return fakeDispatchProgram{model: model}
	}
	t.Cleanup(func() {
		newDispatchProgram = oldProgramFactory
	})

	var out strings.Builder
	markdown, err := defaultRunInteractiveDispatch(context.Background(), &out, dispatchpkg.Options{
		Since: "7d",
	})
	if err != nil {
		t.Fatal(err)
	}
	if markdown != "# generated dispatch\n" {
		t.Fatalf("unexpected markdown: %q", markdown)
	}
	if !strings.Contains(out.String(), "\x1b[1A\x1b[2K\r") {
		t.Fatalf("expected loading card cleanup escape sequences, got %q", out.String())
	}
}
