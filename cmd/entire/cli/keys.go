package cli

import "charm.land/bubbles/v2/key"

// keyMap defines the keybindings used across the CLI's TUIs. Single source of
// truth so help text and matching logic stay aligned, and so the strings "esc",
// "ctrl+c", etc. live in exactly one place.
type keyMap struct {
	Quit    key.Binding
	Back    key.Binding
	Search  key.Binding
	Confirm key.Binding
	Up      key.Binding
	Down    key.Binding
	NextTab key.Binding
	PrevTab key.Binding
	Home    key.Binding
	End     key.Binding
}

var keys = keyMap{
	Quit: key.NewBinding(
		key.WithKeys("q", "ctrl+c"),
		key.WithHelp("q", "quit"),
	),
	Back: key.NewBinding(
		key.WithKeys("esc"),
		key.WithHelp("esc", "back"),
	),
	Search: key.NewBinding(
		key.WithKeys("/"),
		key.WithHelp("/", "search"),
	),
	Confirm: key.NewBinding(
		key.WithKeys("enter"),
		key.WithHelp("enter", "select"),
	),
	Up: key.NewBinding(
		key.WithKeys("up", "k"),
		key.WithHelp("↑/k", "up"),
	),
	Down: key.NewBinding(
		key.WithKeys("down", "j"),
		key.WithHelp("↓/j", "down"),
	),
	NextTab: key.NewBinding(
		key.WithKeys("right", "tab"),
		key.WithHelp("→", "next type"),
	),
	PrevTab: key.NewBinding(
		key.WithKeys("left", "shift+tab"),
		key.WithHelp("←", "prev type"),
	),
	Home: key.NewBinding(
		key.WithKeys("home", "g"),
		key.WithHelp("g/home", "top"),
	),
	End: key.NewBinding(
		key.WithKeys("end", "G"),
		key.WithHelp("G/end", "bottom"),
	),
}
