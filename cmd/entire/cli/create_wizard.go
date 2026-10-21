package cli

import (
	"cmp"
	"fmt"
	"hash/fnv"
	"io"
	"strings"
	"unicode/utf8"

	"charm.land/huh/v2"
	"charm.land/lipgloss/v2"
	"github.com/spf13/cobra"

	"github.com/entireio/cli/cmd/entire/cli/uiform"
)

// createWizard is the scaffolding the `<noun> create` wizards share (project
// create, repo create): running a form, the accessible stage loop, the
// summary page, and the text helpers below. Each wizard embeds it in its own
// state and keeps everything domain-specific — what it asks, how it checks
// answers, what it creates — to itself.
//
// It exists because each of these was a subtle huh behaviour that had to be
// found, and fixed, once per wizard: a summary that went stale after
// Shift+Tab, markdown in the summary, a Shift+Tab that stranded an invalid
// page, accessible mode dropping descriptions and pre-filled answers.
type createWizard struct {
	// action names the flow in its cancellation line ("<action> cancelled.")
	// and in interruption errors.
	action string
	// nav keeps Shift+Tab working on a page that fails validation; nil
	// outside the paged form. Set by startPaged before the pages are built,
	// because a page's validator wraps it (uiform.Lenient).
	nav *uiform.BackNav
	// confirmed is the summary's Create/Cancel answer.
	confirmed bool
}

// startPaged prepares the paged (non-accessible) form; call it before
// building the pages. The summary's answer starts at Create.
func (w *createWizard) startPaged() {
	w.nav = uiform.NewBackNav()
	w.confirmed = true
}

// paged reports whether the pages are being built for the paged form.
func (w *createWizard) paged() bool { return w.nav != nil }

// runForm runs one form and classifies how it ended: a cancelled context is
// an interruption and comes back as an error wrapping it; a user abort prints
// the cancellation line where the prompt was and reports (false, nil); so
// does a declined summary.
func (w *createWizard) runForm(cmd *cobra.Command, groups ...*huh.Group) (bool, error) {
	ctx := cmd.Context()
	if err := ctx.Err(); err != nil {
		return false, fmt.Errorf("%s: %w", strings.ToLower(w.action), err)
	}
	form := NewAccessibleForm(groups...)
	if w.nav != nil {
		// WithProgramOptions replaces huh's option list rather than adding
		// to it; the one default it drops (output) runPromptForm sets anyway,
		// which is why this comes before runPromptForm.
		form = form.WithProgramOptions(w.nav.ProgramOption())
	}
	render, err := runPromptForm(cmd, form)
	// Before the form error is looked at: handleFormCancellation treats a
	// cancelled context as a clean abort, which would report a signal as an
	// answer.
	if ctxErr := ctx.Err(); ctxErr != nil {
		return false, fmt.Errorf("%s: %w", strings.ToLower(w.action), ctxErr)
	}
	if err != nil {
		return false, handleFormCancellation(render, w.action, err)
	}
	return w.confirm(render), nil
}

// confirm turns a declined summary into the cancellation line, written where
// the prompt was drawn.
func (w *createWizard) confirm(render io.Writer) bool {
	if !w.confirmed {
		fmt.Fprintln(render, w.action+" cancelled.")
	}
	return w.confirmed
}

// runStages runs the accessible wizard: huh's accessible runner evaluates
// neither OptionsFunc nor DescriptionFunc/TitleFunc, so each stage is its own
// form, built only when it runs — a stage built up front reads answers from
// before the earlier stages were answered. A stage may build no groups (one
// a previous answer made unnecessary) and is then skipped. after runs once
// each stage has been answered, to apply answers the stage bound to plain
// values (see the accessible pickers).
func (w *createWizard) runStages(cmd *cobra.Command, after func(), stages ...func() []*huh.Group) (bool, error) {
	// runForm reports a decline after every stage, so the summary's answer
	// must start at Create, or the first stage would read as cancelled.
	w.confirmed = true
	for _, stage := range stages {
		groups := stage()
		if len(groups) == 0 {
			continue
		}
		if ok, err := w.runForm(cmd, groups...); !ok || err != nil {
			return ok, err
		}
		if after != nil {
			after()
		}
	}
	return true, nil
}

// summaryPage is the last page: what will be created, then Create or Cancel
// (default Create). summary must keep its line count while the form runs.
//
// In the paged form the text follows revisited answers through a
// DescriptionFunc, which huh re-evaluates only when its binding's hash
// changes. The binding is the summary text itself (summaryBinding), so any
// change to what the summary shows refreshes it — whatever state it reads.
// The static text is set as well: huh sizes every page from the first render,
// before a DescriptionFunc has run. The paged form renders a note as
// markdown, so the text is escaped there; the accessible runner prints it
// raw. hint, when not empty, is shown under the question in the paged form
// (accessible mode drops descriptions).
func (w *createWizard) summaryPage(summary func() string, question, hint string) *huh.Group {
	note := huh.NewNote().Description(summary())
	if w.paged() {
		escaped := func() string { return escapeNoteMarkdown(summary()) }
		note.Description(escaped()).DescriptionFunc(escaped, summaryBinding{summary})
	}
	confirm := huh.NewConfirm().
		Title(question).
		Affirmative("Create").
		Negative("Cancel").
		Value(&w.confirmed)
	if w.paged() && hint != "" {
		confirm.Description(hint)
	}
	return huh.NewGroup(note, confirm).Title("Summary")
}

// summaryBinding is a summary page's DescriptionFunc binding. huh hashes a
// binding with hashstructure, which skips unexported fields and so cannot see
// a change in a struct of them — the cause of a summary that kept its first
// render after Shift+Tab. Hashing the summary text instead (hashstructure
// calls Hash for a type that has it) means the summary refreshes exactly when
// what it shows changes.
type summaryBinding struct{ summary func() string }

// Hash implements hashstructure's Hashable, whose signature carries the
// error; hashing a string never fails.
func (b summaryBinding) Hash() (uint64, error) { //nolint:unparam // hashstructure.Hashable's signature
	h := fnv.New64a()
	_, _ = h.Write([]byte(b.summary()))
	return h.Sum64(), nil
}

// wizardDefaultInput adapts an input to huh's accessible runner, which keeps
// the current value on an empty answer but validates the empty answer first,
// so a pre-filled value could not be accepted with Enter. It also never shows
// the value it would keep, so the question names it.
func wizardDefaultInput(in *huh.Input, value *string, title string, validate func(string) error) *huh.Input {
	in.Value(value)
	current := strings.TrimSpace(*value)
	if current == "" {
		return in.Title(title).Validate(validate)
	}
	return in.
		Title(fmt.Sprintf("%s (press Enter for %q)", title, current)).
		Validate(func(v string) error {
			return validate(cmp.Or(strings.TrimSpace(v), current))
		})
}

// wizardRow is one label/value line of a summary or a recap.
type wizardRow struct{ label, value string }

// wizardRows lays rows out one per line with the values aligned at width
// (a column width in runes, label included).
func wizardRows(rows []wizardRow, width int) string {
	lines := make([]string, len(rows))
	for i, r := range rows {
		lines[i] = fmt.Sprintf("%-*s%s", width, r.label, r.value)
	}
	return strings.Join(lines, "\n")
}

// wizardLabelWidth is the widest label in runes. Pass every label the text
// can show, not only the ones shown now, so a recap that grows page by page
// keeps its alignment.
func wizardLabelWidth(labels ...string) int {
	width := 0
	for _, l := range labels {
		width = max(width, utf8.RuneCountInString(l))
	}
	return width
}

// wizardRecapDim sets a page's recap apart from the heading it sits above.
var wizardRecapDim = lipgloss.NewStyle().Faint(true)

// wizardPageTitle is a page's heading with the recap of earlier answers,
// dimmed, above it:
//
//	✓ Owner  acme (organization)
//	✓ Name   widgets
//	Region
//
// A group title has no TitleFunc, but huh reads it afresh on every render, so
// a wizard rewrites it on the live group when an answer changes. The recap's
// line count must not change while the form runs (huh measures page heights
// up front).
func wizardPageTitle(recap, heading string) string {
	// Line by line: rendering the block at once pads every line to the
	// widest one.
	lines := strings.Split(recap, "\n")
	for i, l := range lines {
		lines[i] = wizardRecapDim.Render(l)
	}
	return strings.Join(append(lines, heading), "\n")
}

// escapeNoteMarkdown escapes the characters huh's note renderer treats as
// markdown (italic, bold, code), so a summary shows the answers verbatim:
// `my_app` must not render as an italic "myapp".
func escapeNoteMarkdown(text string) string {
	var b strings.Builder
	for _, r := range text {
		switch r {
		case '\\', '_', '*', '`':
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}
