//go:build !darwin && !linux

package main

// holdTerminalLikeTUI is a no-op here: interactive E2E sessions run under tmux,
// which these platforms do not have. See terminal_unix.go.
func holdTerminalLikeTUI() (restore func()) {
	return func() {}
}
