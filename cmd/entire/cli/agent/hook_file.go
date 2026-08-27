package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"

	"github.com/entireio/cli/cmd/entire/cli/jsonutil"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/paths"
)

// HookArtifactRemoval strips Entire's own artifacts out of an agent config
// file's raw bytes and reports whether it removed any.
//
// This is the single description of what Entire owns in that file, and both
// halves of the hook lifecycle are derived from it: HookArtifactsInstalled
// answers "are this agent's hooks installed here?" with it, and
// RemoveHookArtifacts performs the removal with it. Deriving both from one
// function is the point. Written independently, detection drifted narrower than
// removal — Claude Code probed the Stop hook while removal covered six hook
// types and a permissions rule — and every caller that skips removal on a
// negative answer (`entire agent remove`, the `entire enable` deselect path)
// then left artifacts behind, still invoking an Entire the repo no longer
// configures.
//
// changed must be true only for artifacts Entire owns: a hook command
// IsManagedHookCommand recognises, or a settings rule InstallHooks writes. A
// removal may also tidy unowned scaffolding on its way out — pruning a container
// it just emptied, dropping a format-version key it added when nothing else is
// left — but that tidying must not set changed. Counting it would report an
// installation in every repo that merely carries an empty hooks stanza, and
// would keep reporting one forever after a successful uninstall.
//
// out is the file's new content, or nil to delete the file entirely. It is read
// only when changed is true, so a removal that changes nothing need not build it.
type HookArtifactRemoval func(data []byte) (out []byte, changed bool, err error)

// HookConfigPath resolves repo-root-relative config path elements against the
// current worktree root, falling back to the working directory outside a
// repository. Install, detection and removal have to agree on this path, so they
// share one resolution of it.
func HookConfigPath(ctx context.Context, elements ...string) string {
	repoRoot, err := paths.WorktreeRoot(ctx)
	if err != nil {
		repoRoot = "." // Fall back to the working directory outside a repository.
	}
	return filepath.Join(append([]string{repoRoot}, elements...)...)
}

// HookArtifactsPresent reports whether remove would strip anything from the file
// at path — which is what "this agent's hooks are installed here" means.
//
// Read-only, so `entire status`, `entire doctor` and the agent pickers can ask
// freely. A missing file is an answer, not a failure: that file is where the
// state lives, so its absence means no hooks. Anything that stops us reading the
// answer comes back as an error, because "we could not tell" and "there are
// none" are different things to a caller deciding whether hooks can be left
// alone.
func HookArtifactsPresent(path string, remove HookArtifactRemoval) (bool, error) {
	data, err := readHookConfigFile(path)
	if err != nil || data == nil {
		return false, err
	}
	_, changed, err := remove(data)
	if err != nil {
		return false, err
	}
	return changed, nil
}

// HookArtifactsInstalled is the bool-only form of HookArtifactsPresent, for the
// AreHooksInstalled contract, which has no error channel: a config that cannot
// be read is logged and answered as not installed. Agents call this so the
// "could not tell" case is handled the same way everywhere rather than swallowed
// differently in each package.
func HookArtifactsInstalled(ctx context.Context, agentName, path string, remove HookArtifactRemoval) bool {
	present, err := HookArtifactsPresent(path, remove)
	if err != nil {
		logging.Warn(ctx, "could not determine whether Entire hooks are installed",
			"agent", agentName, "path", path, "err", err)
		return false
	}
	return present
}

// RemoveHookArtifacts applies remove to the file at path, writing only when
// something Entire owns was actually removed.
//
// Leaving the file untouched otherwise is deliberate: removal runs on paths that
// sweep every agent, and rewriting a config Entire has no artifacts in would
// churn files belonging to agents the user never enabled.
func RemoveHookArtifacts(path string, remove HookArtifactRemoval) error {
	data, err := readHookConfigFile(path)
	if err != nil || data == nil {
		return err
	}
	out, changed, err := remove(data)
	if err != nil {
		return err
	}
	if !changed {
		return nil
	}
	if out == nil {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove %s: %w", path, err)
		}
		return nil
	}
	if err := os.WriteFile(path, out, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// RemoveDenyRule strips one rule from a Claude-shaped settings document's
// permissions.deny list, pruning the deny list and the permissions object when
// it empties them, and reports whether the rule was there. Agents that install a
// deny rule alongside their hooks (Claude Code, Factory Droid) share it, since
// the rule is one of the artifacts their removal owns.
//
// A permissions block that will not parse is an error, not a skip: the rule may
// well still be inside it, and answering "nothing of ours here" would leave it
// on disk while reporting a successful uninstall.
func RemoveDenyRule(rawSettings map[string]json.RawMessage, rule string) (bool, error) {
	permRaw, ok := rawSettings["permissions"]
	if !ok {
		return false, nil
	}
	var rawPermissions map[string]json.RawMessage
	if err := json.Unmarshal(permRaw, &rawPermissions); err != nil {
		return false, fmt.Errorf("failed to parse permissions: %w", err)
	}
	denyRaw, ok := rawPermissions["deny"]
	if !ok {
		return false, nil
	}
	var denyRules []string
	if err := json.Unmarshal(denyRaw, &denyRules); err != nil {
		return false, fmt.Errorf("failed to parse permissions.deny: %w", err)
	}
	remaining := slices.DeleteFunc(denyRules, func(existing string) bool { return existing == rule })
	if len(remaining) == len(denyRules) {
		return false, nil
	}

	if len(remaining) > 0 {
		denyJSON, err := json.Marshal(remaining)
		if err != nil {
			return false, fmt.Errorf("failed to marshal permissions.deny: %w", err)
		}
		rawPermissions["deny"] = denyJSON
	} else {
		delete(rawPermissions, "deny")
	}

	if len(rawPermissions) > 0 {
		permJSON, err := jsonutil.MarshalWithNoHTMLEscape(rawPermissions)
		if err != nil {
			return false, fmt.Errorf("failed to marshal permissions: %w", err)
		}
		rawSettings["permissions"] = permJSON
	} else {
		delete(rawSettings, "permissions")
	}
	return true, nil
}

// readHookConfigFile reads an agent config file, reporting an absent file as
// (nil, nil) so both entry points above treat "no file" as "no hooks" and every
// other read failure as an error.
func readHookConfigFile(path string) ([]byte, error) {
	data, err := os.ReadFile(path) //nolint:gosec // path is built by HookConfigPath from the repo root and a fixed name
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	return data, nil
}
