//go:build windows

package cli

import (
	"context"
	"log/slog"
	"os/exec"
	"sync"
	"unsafe"

	"github.com/entireio/cli/cmd/entire/cli/logging"
	"golang.org/x/sys/windows"
)

// pluginJobOnce makes the first plugin launch put this process in its job.
var pluginJobOnce sync.Once

// configurePluginProcess puts this process — not the plugin — in a job object
// that kills its members when the job's last handle closes, which happens when
// this process exits however it exits (taskkill /F included). The plugin, and
// whatever it starts, inherit membership at creation, so there is no window
// between starting the plugin and assigning it.
//
// It happens only on the plugin dispatch path, never process-wide: entire's
// own detached workers (execx.SpawnDetached: telemetry, session sweeps) must
// outlive it, and do so by breaking away from the job, which the job allows.
// A plugin that wants a process to outlive the CLI can do the same with
// CREATE_BREAKAWAY_FROM_JOB.
//
// Failure is logged, not fatal: the plugin still runs, as it did before.
func configurePluginProcess(ctx context.Context, _ *exec.Cmd) {
	pluginJobOnce.Do(func() {
		if err := joinPluginKillJob(); err != nil {
			logging.Warn(ctx, "plugin will not be ended if entire is killed",
				slog.String("error", err.Error()))
		}
	})
}

// joinPluginKillJob creates the kill-on-close job and assigns this process to
// it. The handle is deliberately never closed: closing it is what kills the
// job, so it must live exactly as long as this process.
func joinPluginKillJob() error {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return err //nolint:wrapcheck // logged as is
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{
		BasicLimitInformation: windows.JOBOBJECT_BASIC_LIMIT_INFORMATION{
			LimitFlags: windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE | windows.JOB_OBJECT_LIMIT_BREAKAWAY_OK,
		},
	}
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil { //nolint:gosec // the documented way to pass the struct
		_ = windows.CloseHandle(job) //nolint:errcheck // the error being returned is the one that matters
		return err                   //nolint:wrapcheck // logged as is
	}
	// The assignment can fail when this process is already in a job that
	// forbids nesting or breakaway (rare since Windows 8 supports nesting).
	if err := windows.AssignProcessToJobObject(job, windows.CurrentProcess()); err != nil {
		_ = windows.CloseHandle(job) //nolint:errcheck // the error being returned is the one that matters
		return err                   //nolint:wrapcheck // logged as is
	}
	return nil
}
