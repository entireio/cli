package cli

import (
	"context"
	"io"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	dispatchpkg "github.com/entireio/cli/cmd/entire/cli/dispatch"
)

type fakeDispatchProgram struct {
	model tea.Model
}

func (p fakeDispatchProgram) Run() (tea.Model, error) {
	model, ok := p.model.(dispatchStatusModel)
	if !ok {
		return p.model, nil
	}
	model.result = dispatchRenderResult{markdown: "# generated dispatch\n"}
	return model, nil
}

type dispatchProgramFunc func() (tea.Model, error)

func (f dispatchProgramFunc) Run() (tea.Model, error) {
	return f()
}

func TestDispatchTerminal_ResolvesProviderBeforeProgramRun(t *testing.T) {
	oldPrepare := prepareLocalDispatch
	oldProvider := resolveDispatchProvider
	oldRunDispatch := runDispatch
	oldTerminalMode := dispatchTerminalMode
	oldInteractiveDispatch := runInteractiveDispatch
	oldRenderTerminal := renderTerminalMarkdown
	oldProgramFactory := newDispatchProgram
	providerResolved := false
	programRunning := false
	generator := &stubTextAgent{}
	prepareLocalDispatch = func(_ context.Context, opts dispatchpkg.Options) (dispatchpkg.Options, error) {
		return opts, nil
	}
	resolveDispatchProvider = func(context.Context, io.Writer, string) (*checkpointSummaryProvider, error) {
		if programRunning {
			t.Fatal("provider resolution ran after Bubble Tea took terminal ownership")
		}
		providerResolved = true
		return &checkpointSummaryProvider{TextGenerator: generator, Model: "terminal-model"}, nil
	}
	runDispatch = func(_ context.Context, opts dispatchpkg.Options) (*dispatchpkg.Dispatch, error) {
		if !programRunning {
			t.Fatal("interactive dispatch callback ran before program Run")
		}
		if opts.TextGenerator != generator || opts.Model != "terminal-model" {
			t.Fatalf("provider options not passed to interactive dispatch: generator=%T model=%q", opts.TextGenerator, opts.Model)
		}
		return &dispatchpkg.Dispatch{GeneratedText: "# terminal dispatch\n"}, nil
	}
	dispatchTerminalMode = func(io.Writer) bool { return true }
	runInteractiveDispatch = defaultRunInteractiveDispatch
	renderTerminalMarkdown = func(_ io.Writer, markdown string) (string, error) { return markdown, nil }
	newDispatchProgram = func(model tea.Model, _ io.Writer, _ bool) dispatchProgram {
		if !providerResolved {
			t.Fatal("Bubble Tea program was created before provider resolution completed")
		}
		return dispatchProgramFunc(func() (tea.Model, error) {
			programRunning = true
			status, ok := model.(dispatchStatusModel)
			if !ok {
				t.Fatalf("unexpected model type %T", model)
			}
			markdown, err := status.run(context.Background())
			status.result = dispatchRenderResult{markdown: markdown, err: err}
			return status, nil
		})
	}
	t.Cleanup(func() {
		prepareLocalDispatch = oldPrepare
		resolveDispatchProvider = oldProvider
		runDispatch = oldRunDispatch
		dispatchTerminalMode = oldTerminalMode
		runInteractiveDispatch = oldInteractiveDispatch
		renderTerminalMarkdown = oldRenderTerminal
		newDispatchProgram = oldProgramFactory
	})

	cmd := newDispatchCmd()
	cmd.SetArgs([]string{"--local", "--all-branches"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if !providerResolved {
		t.Fatal("provider was not resolved")
	}
}

func TestDefaultRunInteractiveDispatch_DoesNotUseAltScreen(t *testing.T) {
	// Cannot run in parallel: mutates package-level newDispatchProgram, which
	// races with TestDefaultRunInteractiveDispatch_ClearsLoadingCardBeforeReturn.
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

	view := model.View().Content
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

func TestDefaultRenderTerminalMarkdown_RendersHyperlinks(t *testing.T) {
	t.Parallel()

	rendered, err := defaultRenderTerminalMarkdown(io.Discard, "[Entire](https://entire.dev)\n")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(rendered, "\x1b]8;") {
		t.Fatalf("expected OSC 8 hyperlink sequence, got %q", rendered)
	}
	if !strings.Contains(rendered, ";https://entire.dev\x07") {
		t.Fatalf("expected hyperlink target, got %q", rendered)
	}
	if !strings.Contains(rendered, "\x1b]8;;\x07") {
		t.Fatalf("expected OSC 8 hyperlink reset, got %q", rendered)
	}
}

func TestDefaultRunInteractiveDispatch_ClearsLoadingCardBeforeReturn(t *testing.T) {
	// Cannot run in parallel: mutates package-level newDispatchProgram, which
	// races with TestDefaultRunInteractiveDispatch_DoesNotUseAltScreen.
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
