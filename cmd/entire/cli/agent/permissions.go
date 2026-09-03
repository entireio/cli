package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"slices"

	"github.com/entireio/cli/cmd/entire/cli/jsonutil"
)

// MetadataDenyRule is the Read deny rule Entire used to install into agent
// permission configs to keep an agent from reading `.entire/metadata`.
//
// It is no longer installed, and `entire enable` and `entire doctor` remove it.
// The constant survives because removing it is now the migration, so the exact
// string still has to be recognisable — and because it is what identifies the
// rule as ours: a rule byte-identical to this one was written by Entire, so
// deleting it takes nothing the user chose.
//
// Why it went, in order of weight:
//
//   - A `deny` rule is a hard block, not a hint. Rules resolve deny → ask →
//     allow, first match wins, and a deny rule cannot carry allowlist
//     exceptions, so there is no way to soften it. Claude Code applies Read
//     deny rules to file-reading Bash commands too, so a recursive grep from
//     the repo root — or a command that merely names the path — is refused and
//     has to be approved by hand. That defeats unattended/auto permission
//     modes for a whole class of ordinary commands.
//   - It guarded a staging buffer, not a store. `.entire/metadata/<session>`
//     holds a `full.jsonl` that is rewritten from the agent's own transcript on
//     every Stop and deleted once the session is condensed. The durable copy
//     lives in the checkpoint tree, which no permission rule covers: the same
//     transcript is readable with `git show entire/checkpoints/v1:...`.
//   - It was mostly redundant. `.entire/.gitignore` already ignores
//     `metadata/`, and Claude Code's Grep skips gitignored files, so the
//     accidental-bulk-read case that motivated the rule was already covered.
//     Glob does not respect gitignore, but Glob returns names, not content.
//
// Deliberately NOT replaced with a PreToolUse hook on Read: that fires a
// subprocess for every file the agent reads, on a hook path that is already the
// dominant cost of a turn.
const MetadataDenyRule = "Read(./.entire/metadata/**)"

// HasMetadataDenyRule reports whether rawPermissions still carries the retired
// metadata deny rule. Used by the diagnostics that tell a user why they are
// being asked to approve ordinary commands; it does not mutate anything.
func HasMetadataDenyRule(rawPermissions map[string]json.RawMessage) bool {
	rules, ok := denyRules(rawPermissions)
	if !ok {
		return false
	}
	return slices.Contains(rules, MetadataDenyRule)
}

// RemoveMetadataDenyRule drops the retired metadata deny rule from
// rawPermissions, mutating it in place, and reports whether anything changed.
//
// Only the exact MetadataDenyRule string is removed, so every other deny rule —
// including one a user wrote — is preserved. A `deny` array left empty is
// deleted rather than written back as [].
//
// Emptying the surrounding `permissions` object is the CALLER's to clean up:
// this function is handed only that object and cannot delete its own key. Every
// caller does — the two InstallHooks write paths, both UninstallHooks, and
// RepairRetiredMetadataDenyRule — so a config Entire fully owned ends up with no
// permissions block rather than an empty one.
//
// A `deny` value that will not parse as a string array is left untouched: it is
// not ours, and rewriting what we cannot read is worse than leaving it.
func RemoveMetadataDenyRule(rawPermissions map[string]json.RawMessage) (bool, error) {
	rules, ok := denyRules(rawPermissions)
	if !ok {
		return false, nil
	}
	kept := make([]string, 0, len(rules))
	for _, rule := range rules {
		if rule != MetadataDenyRule {
			kept = append(kept, rule)
		}
	}
	if len(kept) == len(rules) {
		return false, nil
	}
	if len(kept) == 0 {
		delete(rawPermissions, "deny")
		return true, nil
	}
	denyJSON, err := json.Marshal(kept)
	if err != nil {
		return false, fmt.Errorf("failed to marshal permissions.deny: %w", err)
	}
	rawPermissions["deny"] = denyJSON
	return true, nil
}

// denyRules parses rawPermissions["deny"] as a string array. ok is false when
// the key is absent or holds something else, which both mean "nothing here for
// us to act on".
func denyRules(rawPermissions map[string]json.RawMessage) (rules []string, ok bool) {
	denyRaw, present := rawPermissions["deny"]
	if !present {
		return nil, false
	}
	if err := json.Unmarshal(denyRaw, &rules); err != nil {
		return nil, false
	}
	return rules, true
}

// PermissionConfigOwner is implemented by an agent that keeps Entire-managed
// entries in a JSON config with a top-level "permissions" object. It exists so
// the shared diagnostics and the retired-rule migration can inspect and repair
// that block without knowing where each agent keeps its config.
//
// Implemented by Claude Code (.claude/settings.json) and Factory AI Droid
// (.factory/settings.json) — the two agents Entire ever wrote MetadataDenyRule
// into.
type PermissionConfigOwner interface {
	Agent

	// PermissionConfig returns the agent's permission-bearing config file for
	// the current worktree. It does not create anything.
	PermissionConfig(ctx context.Context) (*HookConfigFile, error)
}

// AsPermissionConfigOwner returns ag as a PermissionConfigOwner when it keeps a
// permissions block Entire may have written into.
func AsPermissionConfigOwner(ag Agent) (PermissionConfigOwner, bool) {
	owner, ok := ag.(PermissionConfigOwner)
	return owner, ok
}

// HasRetiredMetadataDenyRule reports whether ag's config still carries the
// retired deny rule. It is read-only and answers false for every reason that is
// not a positive sighting — agent doesn't own a permissions config, file absent,
// unreadable, or unparseable — because its only callers are diagnostics, and
// telling a user their config is broken on the strength of a failed read is
// worse than staying quiet.
func HasRetiredMetadataDenyRule(ctx context.Context, ag Agent) bool {
	owner, ok := AsPermissionConfigOwner(ag)
	if !ok {
		return false
	}
	cfg, err := owner.PermissionConfig(ctx)
	if err != nil {
		return false
	}
	// No Exists() precheck: Read reports a missing file classifiably and
	// readPermissionsBlock preserves that, so a separate stat would only walk
	// the config's parent directory a second time on the SessionStart path.
	_, rawPermissions, err := readPermissionsBlock(cfg)
	if err != nil {
		return false
	}
	return HasMetadataDenyRule(rawPermissions)
}

// RepairRetiredMetadataDenyRule removes the retired deny rule from ag's config
// and reports whether the file was changed.
//
// Unlike the detector above, this one returns its errors, and `entire doctor`
// calls it directly rather than behind the detector — reading and parsing the
// same file twice bought nothing, since `changed` already reports whether the
// rule was there.
//
// An absent config is not an error: the agent was simply never set up here.
func RepairRetiredMetadataDenyRule(ctx context.Context, ag Agent) (bool, error) {
	owner, ok := AsPermissionConfigOwner(ag)
	if !ok {
		return false, nil
	}
	cfg, err := owner.PermissionConfig(ctx)
	if err != nil {
		return false, err //nolint:wrapcheck // the agent's accessor already names its file
	}
	rawSettings, rawPermissions, err := readPermissionsBlock(cfg)
	if errors.Is(err, fs.ErrNotExist) {
		// The agent was never set up here; nothing to repair and nothing wrong.
		return false, nil
	}
	if err != nil {
		return false, err
	}
	changed, err := RemoveMetadataDenyRule(rawPermissions)
	if err != nil || !changed {
		return false, err
	}

	// An emptied permissions block is deleted rather than written back as {}, so
	// a config Entire fully owned ends up as it was before Entire touched it.
	if len(rawPermissions) == 0 {
		delete(rawSettings, "permissions")
	} else {
		permJSON, marshalErr := jsonutil.MarshalWithNoHTMLEscape(rawPermissions)
		if marshalErr != nil {
			return false, fmt.Errorf("failed to marshal permissions for %s: %w", cfg.Path(), marshalErr)
		}
		rawSettings["permissions"] = permJSON
	}

	output, err := jsonutil.MarshalIndentWithNewline(rawSettings, "", "  ")
	if err != nil {
		return false, fmt.Errorf("failed to marshal %s: %w", cfg.Path(), err)
	}
	if err := cfg.Write(output, 0o600); err != nil {
		return false, fmt.Errorf("failed to write %s: %w", cfg.Path(), err)
	}
	return true, nil
}

// readPermissionsBlock reads cfg and returns its top-level fields plus the
// "permissions" object. rawPermissions is always non-nil so callers can index
// it without a nil check.
func readPermissionsBlock(cfg *HookConfigFile) (rawSettings, rawPermissions map[string]json.RawMessage, err error) {
	data, err := cfg.Read()
	if err != nil {
		return nil, nil, fmt.Errorf("failed to read %s: %w", cfg.Path(), err)
	}
	if err := json.Unmarshal(data, &rawSettings); err != nil {
		return nil, nil, fmt.Errorf("failed to parse %s: %w", cfg.Path(), err)
	}
	if rawSettings == nil {
		rawSettings = make(map[string]json.RawMessage)
	}
	if permRaw, ok := rawSettings["permissions"]; ok {
		if err := json.Unmarshal(permRaw, &rawPermissions); err != nil {
			return nil, nil, fmt.Errorf("failed to parse permissions in %s: %w", cfg.Path(), err)
		}
	}
	if rawPermissions == nil {
		rawPermissions = make(map[string]json.RawMessage)
	}
	return rawSettings, rawPermissions, nil
}
