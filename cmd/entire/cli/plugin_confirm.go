package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"

	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"
	"github.com/muesli/cancelreader"

	"github.com/entireio/cli/cmd/entire/cli/interactive"
)

// pluginPromptTerminal is the controlling terminal a confirmation prompt uses.
//
// in is the terminal rather than os.Stdin, so a confirmation never consumes
// bytes the plugin was piped. out is the terminal's own output handle, used
// only when the writer the caller supplied is not itself a terminal — see
// runPluginConfirm. It is nil when there is nothing to fall back to, which is
// the case in tests that replace the opener.
type pluginPromptTerminal struct {
	in  io.ReadCloser
	out io.Writer
	// closeOut releases out when it is a handle of its own. Unix hands back
	// one file for both directions, so closing in covers it there; Windows
	// opens CONIN$ and CONOUT$ separately, and writing to CONIN$ renders
	// nothing — which is why the pair cannot be collapsed to one handle.
	closeOut func()
}

// Tests replace the opener rather than redirecting the command's data stream.
var openPluginPromptTerminal = func() (pluginPromptTerminal, error) {
	in, out, err := tea.OpenTTY()
	if err != nil {
		return pluginPromptTerminal{}, fmt.Errorf("open confirmation terminal: %w", err)
	}
	t := pluginPromptTerminal{in: in, out: out}
	if out != in {
		t.closeOut = func() { _ = out.Close() }
	}
	return t, nil
}

func runPluginConfirm(ctx context.Context, out io.Writer, prompt string, defaultYes bool) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, fmt.Errorf("confirmation cancelled: %w", err)
	}
	term, err := openPluginPromptTerminal()
	if err != nil {
		return false, err
	}
	input := term.in
	closeInput := sync.OnceFunc(func() { _ = input.Close() })
	defer closeInput()
	if term.closeOut != nil {
		defer term.closeOut()
	}
	// The answer is read from the terminal, so the question has to be visible
	// there. A supplied writer that is not a terminal — `entire graph 2>log`,
	// or any wrapper capturing stderr — left the prompt invisible while the
	// terminal sat in raw mode waiting for a keypress, and with Yes as the
	// default an idle Enter authorized a download-and-exec nobody was shown.
	// Render on the terminal the answer comes from instead; the escape
	// sequences had no business in the redirect either way.
	render := out
	if term.out != nil && !interactive.IsTerminalWriter(out) {
		render = term.out
	}
	answer := defaultYes
	form := NewAccessibleForm(huh.NewGroup(huh.NewConfirm().Title(prompt).Value(&answer))).WithOutput(render).WithInput(input)
	if IsAccessibleMode() {
		// Huh's accessible scanner ignores context and treats EOF as the default.
		// Make the read cancellable and retain EOF so it cannot authorize an install.
		reader, readErr := cancelreader.NewReader(input)
		if readErr != nil {
			return false, fmt.Errorf("confirmation input: %w", readErr)
		}
		defer reader.Close()
		cancelled := make(chan struct{})
		stop := context.AfterFunc(ctx, func() {
			if !reader.Cancel() {
				// Some platforms cannot cancel reads on a separately opened
				// terminal. This descriptor belongs to the prompt, so closing
				// it is safe and also releases a blocked read.
				closeInput()
			}
			close(cancelled)
		})
		defer func() {
			if !stop() {
				<-cancelled
			}
		}()
		checked := &pluginConfirmReader{Reader: reader}
		err = form.WithInput(checked).RunWithContext(ctx)
		if ctx.Err() != nil {
			return false, fmt.Errorf("confirmation cancelled: %w", ctx.Err())
		}
		if checked.err != nil {
			if errors.Is(checked.err, io.EOF) {
				return false, nil
			}
			return false, fmt.Errorf("confirmation input: %w", checked.err)
		}
	} else {
		err = form.RunWithContext(ctx)
	}
	if ctx.Err() != nil {
		return false, fmt.Errorf("confirmation cancelled: %w", ctx.Err())
	}
	if err != nil {
		return false, fmt.Errorf("confirmation form: %w", err)
	}
	return answer, nil
}

type pluginConfirmReader struct {
	io.Reader

	err error
}

func (r *pluginConfirmReader) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	if n == 0 {
		r.err = err
	}
	return n, err //nolint:wrapcheck // preserve io.Reader EOF semantics for the scanner
}
