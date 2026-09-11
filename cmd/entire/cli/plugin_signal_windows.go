//go:build windows

package cli

import "os"

// pluginTerminatingSignal always reports nil on Windows: there are no signals
// to propagate. A process ended by TerminateProcess reports an ordinary exit
// code, which ProcessState.ExitCode returns as-is, so a killed child never
// reaches the ExitPluginSignalled path here in the first place.
//
// See plugin_signal_unix.go for why this is a build-tagged pair rather than a
// runtime.GOOS branch.
func pluginTerminatingSignal(*os.ProcessState) os.Signal { return nil }
