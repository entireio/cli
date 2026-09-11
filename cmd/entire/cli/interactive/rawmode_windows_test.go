//go:build windows

package interactive

import (
	"testing"

	"golang.org/x/sys/windows"
)

func TestConsoleModeInRawMode(t *testing.T) {
	t.Parallel()

	if consoleModeInRawMode(windows.ENABLE_LINE_INPUT) {
		t.Error("line-input console reported as raw")
	}
	if !consoleModeInRawMode(0) {
		t.Error("console without line input reported as canonical")
	}
}
