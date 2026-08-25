package redact

import (
	"errors"
	"log/slog"
	"regexp"
	"regexp/syntax"
	"sync"
)

// CustomRulesConfig configures inline custom_redactions and parsed rule packs.
type CustomRulesConfig struct {
	// Inline maps a label (used only in logs/diagnostics) to a Go RE2 regex
	// string. Failed compilations are logged to Logger and dropped.
	Inline map[string]string

	// Packs are pre-parsed rule packs (see LoadPacks). Per-rule regex
	// compilation failures are logged and dropped; sample mismatches are
	// logged but do not drop the rule.
	Packs []*Pack

	// Logger receives this package's diagnostics (rule compile errors,
	// sample mismatches). The CLI injects the logger its entry point
	// initialized so warnings reach .entire/logs/ instead of the
	// process-default stderr logger, which hook contexts swallow.
	// nil means slog.Default(), preserving standalone behavior for
	// library consumers and tests.
	Logger *slog.Logger
}

// compiledCustomRule is a compiled regex retained across calls.
// label is unused for replacement (custom rules always emit the bare REDACTED
// token to match other secret layers), but is preserved for diagnostics.
type compiledCustomRule struct {
	label string
	regex *regexp.Regexp
}

type customRulesState struct {
	rules []compiledCustomRule
}

var (
	customConfig   *customRulesState
	customConfigMu sync.RWMutex
)

// componentAttr tags every warning emitted by this package so log aggregators
// can filter redaction failures with the same key the CLI uses elsewhere
// (`logging.WithComponent(ctx, "redaction")`).
var componentAttr = slog.String("component", "redaction")

// loggerOrDefault resolves an injected diagnostics logger, falling back to
// the process-default slog logger when the caller didn't provide one. No
// package-level sink exists on purpose: the logger arrives by injection
// (config field or parameter) from whichever entry point initialized it.
func loggerOrDefault(l *slog.Logger) *slog.Logger {
	if l != nil {
		return l
	}
	return slog.Default()
}

// compileErrorAttr describes a regex compile failure without the pattern
// text: regexp error strings quote the full pattern verbatim, and these
// warnings are persisted to .entire/logs/ and packaged by doctor bundle. A
// user who pastes a literal secret as a custom_redactions value and gets the
// regex wrong must not have it stored there — the rule that would have
// redacted it is exactly the one that failed to compile. The rule label/id
// attrs beside this identify which pattern to fix; the user owns its text.
func compileErrorAttr(err error) slog.Attr {
	var serr *syntax.Error
	if errors.As(err, &serr) {
		return slog.String("error", string(serr.Code))
	}
	return slog.String("error", "regex compile failed")
}

// ActiveCustomRules reports how many user-defined rules (inline + pack)
// actually compiled and are live in the detection pipeline — as opposed to
// how many were configured, which silently diverges when a regex fails to
// compile.
func ActiveCustomRules() int {
	cfg := getCustomRulesConfig()
	if cfg == nil {
		return 0
	}
	return len(cfg.rules)
}

// ConfigureCustomRules compiles user-defined redaction rules and stores the
// result for use by redact.String(). Sample-validation runs here too, so
// failures surface the next time any process initializes redaction.
//
// Call once at process startup after loading settings. Thread-safe.
func ConfigureCustomRules(cfg CustomRulesConfig) {
	state := &customRulesState{}
	logger := loggerOrDefault(cfg.Logger)

	for label, pattern := range cfg.Inline {
		compiled, ok := compileCustomRule(
			logger,
			label,
			pattern,
			"skipping invalid custom_redactions pattern",
			slog.String("label", label),
		)
		if ok {
			state.rules = append(state.rules, compiled)
		}
	}

	for _, pack := range cfg.Packs {
		for _, rule := range pack.Rules {
			compiled, ok := compileCustomRule(
				logger,
				pack.Name+"."+rule.ID,
				rule.Regex,
				"skipping invalid pack rule",
				slog.String("pack", pack.sourcePath),
				slog.String("rule", rule.ID),
			)
			if ok {
				state.rules = append(state.rules, compiled)
				runRuleSamples(logger, pack, rule, compiled.regex)
			}
		}
	}

	customConfigMu.Lock()
	defer customConfigMu.Unlock()
	customConfig = state
}

func compileCustomRule(logger *slog.Logger, label, pattern, warning string, attrs ...any) (compiledCustomRule, bool) {
	compiled, err := regexp.Compile(pattern)
	if err != nil {
		all := make([]any, 0, len(attrs)+2)
		all = append(all, componentAttr)
		all = append(all, attrs...)
		all = append(all, compileErrorAttr(err))
		logger.Warn(warning, all...)
		return compiledCustomRule{}, false
	}
	return compiledCustomRule{label: label, regex: compiled}, true
}

// runRuleSamples checks each sample against the compiled regex and logs a
// warning per mismatch. Failures never drop the rule — sample validation
// is informational, not gating.
func runRuleSamples(logger *slog.Logger, pack *Pack, rule Rule, compiled *regexp.Regexp) {
	for i, s := range rule.Samples {
		got := compiled.MatchString(s.Input)
		if got != s.Redacted {
			logger.Warn("redactor pack sample mismatch",
				componentAttr,
				slog.String("pack", pack.sourcePath),
				slog.String("rule", rule.ID),
				slog.Int("sample_index", i),
				slog.Int("sample_length", len(s.Input)),
				slog.Bool("expected", s.Redacted),
				slog.Bool("got", got))
		}
	}
}

// getCustomRulesConfig returns the currently-configured custom rules.
// Returns nil if ConfigureCustomRules has never been called.
func getCustomRulesConfig() *customRulesState {
	customConfigMu.RLock()
	defer customConfigMu.RUnlock()
	return customConfig
}

// detectCustomRules returns tagged regions for every match of every
// configured custom rule. Returns nil if no rules are configured.
//
// All regions use an empty label so they are replaced with the bare
// "REDACTED" token used by the built-in secret layers, not the
// "[REDACTED_<LABEL>]" token used by PII.
func detectCustomRules(cfg *customRulesState, s string) []taggedRegion {
	if cfg == nil || len(cfg.rules) == 0 || s == "" {
		return nil
	}
	var regions []taggedRegion
	for _, rule := range cfg.rules {
		for _, loc := range rule.regex.FindAllStringIndex(s, -1) {
			regions = append(regions, taggedRegion{region: region{loc[0], loc[1]}})
		}
	}
	return regions
}
