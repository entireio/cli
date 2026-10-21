package cli

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"
)

// Rows align their values at the given column, counting runes, so a recap
// whose labels start with "✓ " lines up as well as a plain summary.
func TestWizardRows_AlignValues(t *testing.T) {
	t.Parallel()
	rows := []wizardRow{{"Name", "web"}, {"Object format", "sha1"}}
	require.Equal(t, "Name           web\nObject format  sha1", wizardRows(rows, wizardLabelWidth("Name", "Object format")+2))

	recap := []wizardRow{{"✓ Owner", "acme"}, {"✓ Name", "widgets"}}
	require.Equal(t, "✓ Owner  acme\n✓ Name   widgets", wizardRows(recap, wizardLabelWidth("✓ Owner", "✓ Name")+2))
}

// The recap sits above the heading, dimmed line by line so no line is padded
// to the widest one.
func TestWizardPageTitle_RecapAboveHeading(t *testing.T) {
	t.Parallel()
	title := wizardPageTitle("✓ Owner  acme\n✓ Name   widgets", "Region")
	require.Equal(t, []string{"✓ Owner  acme", "✓ Name   widgets", "Region"}, strings.Split(ansi.Strip(title), "\n"))
}

// huh's accessible runner validates the empty answer before keeping the
// current value, so a pre-filled value must validate as itself — and the
// question names the value Enter keeps.
func TestWizardDefaultInput_EnterKeepsTheValue(t *testing.T) {
	t.Parallel()
	taken := errors.New("taken")
	validate := func(v string) error {
		if v == "web" {
			return taken
		}
		return nil
	}

	value := "tools"
	require.NoError(t, wizardDefaultInput(huh.NewInput(), &value, "Name", validate).RunAccessible(io.Discard, strings.NewReader("\n")))
	require.Equal(t, "tools", value)

	value = "web"
	var out bytes.Buffer
	require.NoError(t, wizardDefaultInput(huh.NewInput(), &value, "Name", validate).RunAccessible(&out, strings.NewReader("\nfresh\n")))
	require.Contains(t, out.String(), `Name (press Enter for "web")`)
	require.Contains(t, out.String(), "taken", "the kept value is still checked")
	require.Equal(t, "fresh", value)
}

// A declined summary prints the flow's cancellation line where the prompt
// was, and reports no error.
func TestCreateWizard_DeclineSaysCancelled(t *testing.T) {
	t.Parallel()
	w := &createWizard{action: "Widget create", confirmed: true}
	var out bytes.Buffer
	require.True(t, w.confirm(&out))
	require.Empty(t, out.String())
	w.confirmed = false
	require.False(t, w.confirm(&out))
	require.Equal(t, "Widget create cancelled.\n", out.String())
}

// The paged form renders a note as markdown; the summary escapes what it
// treats as formatting, so `my_app` does not show as an italic "myapp".
func TestEscapeNoteMarkdown(t *testing.T) {
	t.Parallel()
	require.Equal(t, "my\\_app \\*x\\* \\`y\\` a\\\\b", escapeNoteMarkdown("my_app *x* `y` a\\b"))
	require.Equal(t, "/et/acme/web", escapeNoteMarkdown("/et/acme/web"))
}

// A stage that builds no groups is skipped, and the after hook runs once per
// answered stage.
func TestCreateWizard_RunStagesSkipsEmptyStages(t *testing.T) {
	t.Parallel()
	w := &createWizard{action: "Widget create", confirmed: true}
	built := 0
	ok, err := w.runStages(nil, func() { t.Error("after must not run: no stage was answered") },
		func() []*huh.Group { built++; return nil },
		func() []*huh.Group { built++; return nil },
	)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, 2, built, "every stage is built when its turn comes")
}

// formCmdCutoff bounds how long driveForm waits for a command a form returns.
// A cursor-blink timer re-arms forever and must be dropped, but a command the
// form needs — a select's options loading — can take longer than an instant on
// a loaded CI runner, and a Select ignores Enter until its options are in.
// 250ms is well under the 530ms blink, above huh's 100ms spinner tick (which
// stops once the options load), and far above what an immediate command takes.
const formCmdCutoff = 250 * time.Millisecond

// driveForm runs a huh form the way its program loop would, without a
// terminal. send delivers messages and then runs every command the form asks
// for in reply (focus moves, page changes, options loading) until none is
// left; run executes one command, dropping it if it does not answer within
// formCmdCutoff.
func driveForm(form *huh.Form) (send func(...tea.Msg), run func(tea.Cmd) tea.Msg) {
	var model huh.Model = form
	run = func(cmd tea.Cmd) tea.Msg {
		out := make(chan tea.Msg, 1)
		go func() { out <- cmd() }()
		select {
		case msg := <-out:
			return msg
		case <-time.After(formCmdCutoff):
			return nil
		}
	}
	send = func(msgs ...tea.Msg) {
		for _, msg := range msgs {
			var cmd tea.Cmd
			model, cmd = model.Update(msg)
			for steps, queue := 0, []tea.Cmd{cmd}; len(queue) > 0 && steps < 200; steps++ {
				next := queue[0]
				queue = queue[1:]
				if next == nil {
					continue
				}
				switch m := run(next).(type) {
				case tea.BatchMsg:
					queue = append(queue, m...)
				case nil:
				default:
					var more tea.Cmd
					model, more = model.Update(m)
					queue = append(queue, more)
				}
			}
		}
	}
	return send, run
}
