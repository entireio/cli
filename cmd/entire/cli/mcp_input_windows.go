package cli

import "io"

// Go's os.File.Close cancels pending pipe I/O on Windows. Do not use
// cancelreader here: for os.Stdin it opens CONIN$, replacing an MCP host's
// redirected input with console input (or failing in a headless process).
func newMCPInput(in io.ReadCloser) (io.Reader, func() bool, func(), error) {
	return in, func() bool { return false }, func() {}, nil
}
