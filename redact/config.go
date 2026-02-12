package redact

import (
	"fmt"
	"os"
	"sync"

	gitleaksconfig "github.com/zricethezav/gitleaks/v8/config"
)

var (
	globalRuleSet  *RuleSet
	globalConfig   Config
	globalConfigMu sync.RWMutex
	initOnce       sync.Once
)

// Config controls the behavior of the redaction engine.
type Config struct {
	// PatternDetectionEnabled enables gitleaks pattern matching. Default: true.
	PatternDetectionEnabled bool

	// EntropyThreshold is the Shannon entropy threshold for entropy-based detection. Default: 4.5.
	EntropyThreshold float64

	// CustomRulesPath points to a custom gitleaks.toml file. Empty = use embedded default.
	CustomRulesPath string

	// LogMatchCallback is an optional callback invoked for each detected secret.
	// Arguments: detection method ("pattern", "entropy", "pattern+entropy"), rule ID, masked preview.
	LogMatchCallback func(method, ruleID, preview string)
}

// defaultConfig returns the default configuration.
func defaultConfig() Config {
	return Config{
		PatternDetectionEnabled: true,
		EntropyThreshold:        entropyThreshold,
	}
}

// ensureInit loads default rules on first use.
func ensureInit() {
	initOnce.Do(func() {
		globalConfig = defaultConfig()
		rs, err := ParseGitleaksConfig([]byte(gitleaksconfig.DefaultConfig))
		if err != nil {
			// Fall back to entropy-only detection if parsing fails.
			globalRuleSet = nil
			return
		}
		globalRuleSet = rs
	})
}

// Init initializes the redaction engine with the given configuration.
// Call this during CLI startup to apply settings overrides.
func Init(cfg Config) error {
	ensureInit()

	globalConfigMu.Lock()
	defer globalConfigMu.Unlock()

	if cfg.EntropyThreshold > 0 {
		globalConfig.EntropyThreshold = cfg.EntropyThreshold
	}
	globalConfig.PatternDetectionEnabled = cfg.PatternDetectionEnabled
	globalConfig.LogMatchCallback = cfg.LogMatchCallback

	if cfg.CustomRulesPath != "" {
		data, err := os.ReadFile(cfg.CustomRulesPath) //nolint:gosec // path from user config
		if err != nil {
			return fmt.Errorf("reading custom rules file: %w", err)
		}
		rs, err := ParseGitleaksConfig(data)
		if err != nil {
			return fmt.Errorf("parsing custom rules: %w", err)
		}
		globalRuleSet = rs
	}

	return nil
}

// getConfig returns the current configuration (thread-safe).
func getConfig() Config {
	ensureInit()
	globalConfigMu.RLock()
	defer globalConfigMu.RUnlock()
	return globalConfig
}

// getRuleSet returns the current rule set (thread-safe).
func getRuleSet() *RuleSet {
	ensureInit()
	globalConfigMu.RLock()
	defer globalConfigMu.RUnlock()
	return globalRuleSet
}

// maskedPreview returns a masked version of a secret for logging.
// Shows first 4 and last 4 characters, masking the middle.
func maskedPreview(s string) string {
	if len(s) <= 12 {
		return "****"
	}
	return s[:4] + "..." + s[len(s)-4:]
}
