//go:build !windows

package cli

import (
	"fmt"
	"io"
	"os"

	"github.com/muesli/cancelreader"
	"golang.org/x/term"
)

// Inherited Unix stdin can be a blocking descriptor that os.File.Close does
// not interrupt. cancelreader polls it alongside a cancellation pipe instead.
func newMCPInput(in io.ReadCloser) (io.Reader, func() bool, func(), error) {
	if file, ok := in.(*os.File); ok {
		info, err := file.Stat()
		if err != nil {
			return nil, nil, nil, fmt.Errorf("stat mcp input: %w", err)
		}
		// kqueue does not reliably signal EOF on regular files or /dev/null.
		// These finite inputs need no readiness wait before reading.
		if info.Mode().IsRegular() || info.Mode()&os.ModeCharDevice != 0 && !term.IsTerminal(int(file.Fd())) {
			return in, func() bool { return false }, func() {}, nil
		}
	}
	reader, err := cancelreader.NewReader(in)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("create cancellable reader: %w", err)
	}
	return reader, reader.Cancel, func() {
		_ = reader.Close() // best-effort release of cancellation descriptors after reads stop
	}, nil
}
