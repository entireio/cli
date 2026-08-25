package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/internal/flock"
)

const userHookLockTimeout = 5 * time.Second

// AcquireUserHookConfigLock serializes a complete user-config read/modify/write
// transaction. The bounded wait keeps foreground setup responsive while still
// preserving unrelated concurrent agent-config edits.
func AcquireUserHookConfigLock(ctx context.Context, settingsPath string) (func(), error) {
	if err := os.MkdirAll(filepath.Dir(settingsPath), 0o750); err != nil {
		return nil, fmt.Errorf("create user hook config directory: %w", err)
	}
	lockCtx, cancel := context.WithTimeout(ctx, userHookLockTimeout)
	release, err := flock.AcquireContext(lockCtx, settingsPath+".entire.lock")
	if err != nil {
		cancel()
		return nil, fmt.Errorf("lock user hook config: %w", err)
	}
	return func() {
		release()
		cancel()
	}, nil
}

// UserHookSupport is implemented only by agents with a verified user-level
// config and cross-scope deduplication contract.
type UserHookSupport interface {
	HookSupport

	// InstallUserHooks idempotently installs the complete user inventory.
	InstallUserHooks(ctx context.Context) (UserHookInstallResult, error)

	// UninstallUserHooks removes only Entire's entries; missing is a no-op.
	UninstallUserHooks(ctx context.Context) error

	// AreUserHooksInstalled distinguishes missing from unreadable or invalid.
	AreUserHooksInstalled(ctx context.Context) (bool, error)
}

// UserHookInstallResult distinguishes a no-op from additions and repairs.
type UserHookInstallResult struct {
	// Installed counts newly added current-form entries.
	Installed int
	// Repaired reports normalization of pre-existing Entire entries.
	Repaired bool
}

// AsUserHookSupport returns the agent as UserHookSupport if it implements the
// interface. Built-in-only capability: external plugin agents have no
// user-level install protocol, so this resolves by type assertion alone.
func AsUserHookSupport(ag Agent) (UserHookSupport, bool) {
	return builtinCapability[UserHookSupport](ag)
}

// UserHookAgent pairs a registered agent's name with its user-level hook
// support.
type UserHookAgent struct {
	Name    types.AgentName
	Support UserHookSupport
}

// UserHookSupports partitions registered non-test agents by capability.
func UserHookSupports() (supports []UserHookAgent, unsupported []types.AgentName) {
	for _, name := range List() {
		ag, err := Get(name)
		if err != nil {
			continue
		}
		if to, ok := ag.(TestOnly); ok && to.IsTestOnly() {
			continue
		}
		if uhs, ok := AsUserHookSupport(ag); ok {
			supports = append(supports, UserHookAgent{Name: name, Support: uhs})
		} else {
			unsupported = append(unsupported, name)
		}
	}
	return supports, unsupported
}
