package codex

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/logging"
)

// HookTrustInspection is a structural view of Codex's local approval records.
// It never computes or copies trusted hashes.
type HookTrustInspection struct {
	Declared []string
	Gaps     []string
	Known    bool
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

// codexConfigPath returns the user-level config.toml, or "" when the Codex
// home cannot be resolved, which the caller reads as "trust unknown".
func codexConfigPath() string {
	codexHome, err := resolveCodexHome()
	if err != nil {
		logging.Debug(context.Background(), "codex home unresolved; hook trust is unknown",
			slog.String("error", err.Error()))
		return ""
	}
	return filepath.Join(codexHome, "config.toml")
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

// codexTrustStateHeaderRegex matches `[hooks.state.<key>]` headers in the
// user's Codex config.toml, where <key> is a TOML quoted key. Codex's writer
// (toml_edit) emits a double-quoted basic string on Unix and switches to a
// single-quoted literal string when the key contains a backslash, which every
// Windows path does:
//
//	[hooks.state."/repo/.codex/hooks.json:stop:0:0"]
//	[hooks.state.'C:\repo\.codex\hooks.json:stop:0:0']
//
// Group 1 captures a basic-string body (still escaped), group 2 a literal
// body. Bare keys are not accepted; looser parsing would invite false matches
// in user-edited configs.
var codexTrustStateHeaderRegex = regexp.MustCompile(`(?m)^\[hooks\.state\.(?:"((?:[^"\\\n]|\\.)*)"|'([^'\n]*)')\]`)

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
	text := string(data)
	keys := make(map[string]struct{})
	for _, m := range codexTrustStateHeaderRegex.FindAllStringSubmatchIndex(text, -1) {
		if m[2] < 0 {
			// Literal string: no escapes, the body is the key.
			keys[text[m[4]:m[5]]] = struct{}{}
			continue
		}
		key, ok := unescapeTOMLBasicString(text[m[2]:m[3]])
		if !ok {
			continue
		}
		keys[key] = struct{}{}
	}
	return keys, true
}

// unescapeTOMLBasicString decodes the body of a TOML basic string. TOML's
// escapes (\b \t \n \f \r \" \\ \uXXXX \UXXXXXXXX) are a subset of Go's
// double-quoted string syntax, so once every escape is checked against TOML's
// allowlist, strconv.Unquote decodes the key. Go-only escapes such as \x5c or
// \a are rejected: Codex would refuse that config, so it must not count as trust.
func unescapeTOMLBasicString(body string) (string, bool) {
	for i := 0; i < len(body); i++ {
		if body[i] != '\\' {
			continue
		}
		i++
		if i >= len(body) || !strings.ContainsRune(`btnfr"\uU`, rune(body[i])) {
			return "", false
		}
	}
	s, err := strconv.Unquote(`"` + body + `"`)
	return s, err == nil
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
