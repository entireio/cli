package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"

	"charm.land/huh/v2"
	"github.com/muesli/cancelreader"

	"github.com/entireio/cli/cmd/entire/cli/interactive"
)

// pluginPromptTerminal is the controlling terminal a confirmation prompt uses.
//
// in is the terminal rather than os.Stdin, so a confirmation never consumes
// bytes the plugin was piped. out is the terminal's own output handle, used
// only when the writer the caller supplied is not itself a terminal — see
// runPluginConfirm. close releases both handles. Both are nil when there is
// nothing behind them, which is the case in tests that replace the opener.
type pluginPromptTerminal struct {
	in    io.Reader
	out   io.Writer
	close func() error
}

// Tests replace the opener rather than redirecting the command's data stream.
//
// interactive.OpenPromptTTY rather than tea.OpenTTY: its Close releases the
// read Bubble Tea leaves pending on a separately opened console handle, which
// otherwise made the user press a key a second time on Windows before the
// confirmed action started.
var openPluginPromptTerminal = func() (pluginPromptTerminal, error) {
	tty, err := interactive.OpenPromptTTY()
	if err != nil {
		return pluginPromptTerminal{}, fmt.Errorf("open confirmation terminal: %w", err)
	}
	return pluginPromptTerminal{in: tty.Input(), out: tty.Output(), close: tty.Close}, nil
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
	closeTerminal := sync.OnceFunc(func() {
		if term.close != nil {
			_ = term.close() //nolint:errcheck // best-effort cleanup after terminal interaction, as login.go does
		}
	})
	defer closeTerminal()
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
				// terminal. The terminal belongs to the prompt, so closing
				// it is safe and also releases a blocked read.
				closeTerminal()
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
