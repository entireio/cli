package codex

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// HookTrustGaps returns the snake_case event labels declared in the hooks.json
// Codex discovers that don't have a matching approval entry in the
// user's Codex config.toml. Matching parses the full
// `<hooks.json>:<event>:<group>:<handler>` key, accepts any valid indexes, and
// compares canonicalized hook paths.
//
// This is the structural form of the trust check: we don't recompute
// Codex's hook hash, we only look at key presence. That misses the
// "command changed but key is still there" case (status = Modified),
// but Codex's own startup warning catches those — our purpose here is
// to surface fresh additions like "you trusted three hooks last month
// but a new PostToolUse arrived" inside our SessionStart welcome.
//
// Returns nil when:
//   - .codex/hooks.json doesn't exist (entire isn't installed in this repo)
//   - The authoritative hook location can't be resolved
//   - The checkout has no local .codex project layer
//   - The user's config.toml can't be read
//   - Every declared event already has a state entry
func HookTrustGaps(ctx context.Context) []string {
	return InspectHookTrust(ctx).Gaps
}

// HookTrustInspection is a structural view of Codex's local approval records.
// It never computes or copies trusted hashes.
type HookTrustInspection struct {
	Declared []string
	Gaps     []string
	Known    bool
}

// InspectHookTrust reports declared events and whether the user's Codex config
// contains approval records for them. Known is false when config.toml cannot be
// read, so callers do not mistake an unavailable trust check for active hooks.
func InspectHookTrust(ctx context.Context) HookTrustInspection {
	discovery := ResolveHookDiscovery(ctx)
	if discovery.State != HookDiscoveryResolved || !discovery.ProjectLayerExists() {
		return HookTrustInspection{}
	}
	inspection := inspectDiscoveredHookConfig(ctx, discovery.DiscoveredHooks)
	if inspection.State != HookFileEntire {
		return HookTrustInspection{}
	}
	return inspectHookTrustForDeclared(discovery.DiscoveredHooks.Path(), inspection.Declared)
}

func inspectHookTrust(hooksJSONPath string) HookTrustInspection {
	declared, ok := declaredCodexEvents(hooksJSONPath)
	if !ok || len(declared) == 0 {
		return HookTrustInspection{}
	}
	return inspectHookTrustForDeclared(hooksJSONPath, declared)
}

func inspectHookTrustForDeclared(hooksJSONPath string, declared []string) HookTrustInspection {
	if len(declared) == 0 {
		return HookTrustInspection{}
	}
	inspection := HookTrustInspection{Declared: declared}

	configPath := codexConfigPath()
	if configPath == "" {
		return inspection
	}
	trusted, ok := readCodexTrustedKeys(configPath)
	if !ok {
		return inspection
	}
	inspection.Known = true

	for _, ev := range declared {
		if !codexHasTrustedEvent(trusted, hooksJSONPath, ev) {
			inspection.Gaps = append(inspection.Gaps, ev)
		}
	}
	return inspection
}

func codexConfigPath() string {
	if h := os.Getenv("CODEX_HOME"); h != "" {
		return filepath.Join(h, "config.toml")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".codex", "config.toml")
}

// declaredCodexEvents reads hooks.json and returns the snake_case labels
// of every event that has at least one handler declared. The bool reports
// whether the read+parse succeeded — false on missing/malformed file so
// callers can stay silent rather than mid-flow noise.
func declaredCodexEvents(hooksJSONPath string) ([]string, bool) {
	document, err := readHooksDocument(hooksJSONPath)
	if err != nil || !document.exists {
		return nil, false
	}
	events, err := declaredCodexEventsFromDocument(document)
	return events, err == nil
}

func declaredCodexEventsFromDocument(document *hooksDocument) ([]string, error) {
	var events []string
	add := func(event, label string) error {
		var groups []MatcherGroup
		if err := parseHookType(document.rawHooks, event, &groups); err != nil {
			return err
		}
		for _, g := range groups {
			if len(g.Hooks) > 0 {
				events = append(events, label)
				break
			}
		}
		return nil
	}
	for _, event := range HookEventSpecs() {
		if err := add(event.Event, event.Label); err != nil {
			return nil, err
		}
	}
	return events, nil
}

// MissingEntireHooks reports the managed Codex events that are not present in
// a repository-local hooks file. It is kept as a compatibility helper for
// callers that only need the drift list; diagnostics use HookConfigInspection.
func MissingEntireHooks(repoRoot string) []string {
	document, err := readHooksDocument(filepath.Join(repoRoot, ".codex", HooksFileName))
	if err != nil || !document.exists {
		return nil
	}
	var missing []string
	for _, hook := range managedHooks {
		var groups []MatcherGroup
		if err := parseHookType(document.rawHooks, hook.event, &groups); err != nil {
			return nil
		}
		if !hasEntireHook(groups) {
			missing = append(missing, hook.label)
		}
	}
	return missing
}

// codexTrustStateHeaderRegex matches `[hooks.state."<key>"]` headers in
// the user's Codex config.toml. Quote-only — Codex's own writer emits
// quoted keys (codex-rs/tui/src/app/background_requests.rs:874), and
// looser parsing would invite false matches in user-edited configs.
var codexTrustStateHeaderRegex = regexp.MustCompile(`(?m)^\[hooks\.state\."([^"]+)"\]`)

func readCodexTrustedKeys(configPath string) (map[string]struct{}, bool) {
	file, err := os.Open(configPath) //nolint:gosec // path resolved from CODEX_HOME or HOME
	if err != nil {
		return nil, false
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxHooksFileBytes+1))
	if err != nil || len(data) > maxHooksFileBytes {
		return nil, false
	}
	keys := make(map[string]struct{})
	for _, m := range codexTrustStateHeaderRegex.FindAllStringSubmatch(string(data), -1) {
		keys[m[1]] = struct{}{}
	}
	return keys, true
}

func codexHasTrustedEvent(keys map[string]struct{}, hooksPath, event string) bool {
	canonicalHooksPath, err := canonicalPath(hooksPath)
	if err != nil {
		return false
	}
	for key := range keys {
		trustedHooksPath, trustedEvent, ok := parseCodexTrustKey(key)
		if !ok || trustedEvent != event {
			continue
		}
		// Codex preserves nested symlinks in trust keys, while Git resolves
		// worktree roots to their physical paths.
		canonicalTrustedPath, err := canonicalPath(trustedHooksPath)
		if err == nil && canonicalTrustedPath == canonicalHooksPath {
			return true
		}
	}
	return false
}

func parseCodexTrustKey(key string) (hooksPath, event string, ok bool) {
	handlerSeparator := strings.LastIndexByte(key, ':')
	if handlerSeparator < 0 {
		return "", "", false
	}
	groupSeparator := strings.LastIndexByte(key[:handlerSeparator], ':')
	if groupSeparator < 0 {
		return "", "", false
	}
	eventSeparator := strings.LastIndexByte(key[:groupSeparator], ':')
	if eventSeparator < 0 {
		return "", "", false
	}
	if _, err := strconv.Atoi(key[groupSeparator+1 : handlerSeparator]); err != nil {
		return "", "", false
	}
	if _, err := strconv.Atoi(key[handlerSeparator+1:]); err != nil {
		return "", "", false
	}
	return key[:eventSeparator], key[eventSeparator+1 : groupSeparator], true
}
