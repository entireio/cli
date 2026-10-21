package uiform

import (
	"sync/atomic"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"
)

// BackNav keeps a paged form's "back" key working on a page whose field fails
// validation.
//
// huh (v2.0.3) blurs the focused field and then asks the form for the previous
// page; the form refuses while the page has an error, and nothing refocuses
// the field it just blurred. The page then ignores every key: one invalid
// answer followed by Shift+Tab strands the user. BackNav watches each key on
// its way to the form, and a validator wrapped with Lenient passes while the
// key being handled is the back key — going back needs no valid answer, and
// the answer is checked again on the way forward.
//
// Wire it with form.WithProgramOptions(nav.ProgramOption()). It has no effect
// in accessible mode, which has no back key.
type BackNav struct {
	back atomic.Bool
	prev key.Binding
}

// NewBackNav returns a BackNav for huh's default "back" binding.
func NewBackNav() *BackNav {
	return &BackNav{prev: huh.NewDefaultKeyMap().Input.Prev}
}

// ProgramOption installs the key watcher on the form's program.
func (b *BackNav) ProgramOption() tea.ProgramOption {
	return tea.WithFilter(b.filter)
}

func (b *BackNav) filter(_ tea.Model, msg tea.Msg) tea.Msg {
	if k, ok := msg.(tea.KeyPressMsg); ok {
		b.back.Store(key.Matches(k, b.prev))
	}
	return msg
}

// Going reports whether the key being handled is the back key.
func (b *BackNav) Going() bool { return b != nil && b.back.Load() }

// Lenient wraps a validator so that it passes while b reports the back key.
// A nil b wraps nothing.
func Lenient[T any](b *BackNav, validate func(T) error) func(T) error {
	if b == nil {
		return validate
	}
	return func(v T) error {
		if b.Going() {
			return nil
		}
		return validate(v)
	}
}
