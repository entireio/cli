//go:build !unix

package cli

// terminalSentInterrupt: see plugin_tty_unix.go. Windows never forwards a
// signal (configurePluginCancel), so this is never consulted there.
func terminalSentInterrupt(int) bool { return false }
