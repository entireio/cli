package redact

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// These tests mutate package-global redaction config, so they cannot run in
// parallel with each other or with anything else reading that config.

func TestConfigFingerprint_StableAcrossCalls(t *testing.T) {
	first := ConfigFingerprint()
	second := ConfigFingerprint()
	require.Equal(t, first, second)
	require.NotEmpty(t, first)
}

func TestConfigFingerprint_ChangesWithCustomRules(t *testing.T) {
	before := ConfigFingerprint()
	t.Cleanup(func() { ConfigureCustomRules(CustomRulesConfig{}) })

	ConfigureCustomRules(CustomRulesConfig{Inline: map[string]string{"emp": `EMP-\d{6}`}})
	after := ConfigFingerprint()
	require.NotEqual(t, before, after, "adding a custom rule must change the fingerprint")

	// Same config re-applied must reproduce the same hash (map order must not leak).
	ConfigureCustomRules(CustomRulesConfig{Inline: map[string]string{"emp": `EMP-\d{6}`}})
	require.Equal(t, after, ConfigFingerprint())

	ConfigureCustomRules(CustomRulesConfig{Inline: map[string]string{"emp": `EMP-\d{7}`}})
	require.NotEqual(t, after, ConfigFingerprint(), "changing a pattern must change the fingerprint")
}

func TestConfigFingerprint_MultipleRulesOrderIndependent(t *testing.T) {
	t.Cleanup(func() { ConfigureCustomRules(CustomRulesConfig{}) })

	ConfigureCustomRules(CustomRulesConfig{Inline: map[string]string{
		"a": `AAA-\d+`, "b": `BBB-\d+`, "c": `CCC-\d+`,
	}})
	first := ConfigFingerprint()

	// Re-apply the same set several times; Go randomises map iteration order, so
	// an unsorted hash would drift across these.
	for range 8 {
		ConfigureCustomRules(CustomRulesConfig{Inline: map[string]string{
			"c": `CCC-\d+`, "a": `AAA-\d+`, "b": `BBB-\d+`,
		}})
		require.Equal(t, first, ConfigFingerprint())
	}
}

func TestConfigFingerprint_ChangesWithPIIConfig(t *testing.T) {
	t.Cleanup(func() { ConfigurePII(PIIConfig{}) })

	ConfigurePII(PIIConfig{Enabled: false})
	disabled := ConfigFingerprint()

	ConfigurePII(PIIConfig{Enabled: true, Categories: map[PIICategory]bool{PIIEmail: true}})
	enabled := ConfigFingerprint()
	require.NotEqual(t, disabled, enabled, "enabling PII must change the fingerprint")

	ConfigurePII(PIIConfig{Enabled: true, Categories: map[PIICategory]bool{PIIEmail: false}})
	require.NotEqual(t, enabled, ConfigFingerprint(), "toggling a category must change the fingerprint")
}
