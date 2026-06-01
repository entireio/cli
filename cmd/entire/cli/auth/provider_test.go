package auth

import (
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/api"
)

func TestResolveProvider_DefaultsToV1(t *testing.T) {
	t.Parallel()

	cases := []string{"", "v1", "  ", "unknown", "v3"}
	for _, in := range cases {
		got := resolveProvider(in)
		if got.DeviceCodePath != "/oauth/device/code" {
			t.Errorf("resolveProvider(%q).DeviceCodePath = %q, want v1's", in, got.DeviceCodePath)
		}
		if got.STSPath != "" {
			t.Errorf("resolveProvider(%q).STSPath = %q, want empty for v1", in, got.STSPath)
		}
	}
}

func TestResolveProvider_V2(t *testing.T) {
	t.Parallel()

	got := resolveProvider("v2")
	if got.DeviceCodePath != "/device_authorization" {
		t.Errorf("v2 DeviceCodePath = %q", got.DeviceCodePath)
	}
	if got.TokenPath != "/oauth/token" {
		t.Errorf("v2 TokenPath = %q", got.TokenPath)
	}
	// Token poll and RFC 8693 exchange share the same OIDC endpoint —
	// grant_type differentiates them on the wire.
	if got.STSPath != "/oauth/token" {
		t.Errorf("v2 STSPath = %q", got.STSPath)
	}
	if got.AuthTokensPath != "/api/v1/auth/tokens" {
		t.Errorf("v2 AuthTokensPath = %q", got.AuthTokensPath)
	}
}

// effectiveProviderVersion tests cannot be t.Parallel (they use t.Setenv).

func TestEffectiveProviderVersion_ExplicitEnvWins(t *testing.T) {
	// Split-host configured: would auto-pick v2 if env were unset.
	t.Setenv(api.BaseURLEnvVar, "https://api.example.com")
	t.Setenv(api.AuthBaseURLEnvVar, "https://auth.example.com")

	t.Setenv(ProviderVersionEnvVar, "v1")
	if got := effectiveProviderVersion(); got != "v1" {
		t.Errorf("explicit v1 override = %q, want v1", got)
	}

	t.Setenv(ProviderVersionEnvVar, "v2")
	if got := effectiveProviderVersion(); got != "v2" {
		t.Errorf("explicit v2 override = %q, want v2", got)
	}

	// Unrecognised values pass through verbatim rather than triggering
	// auto-detect — a typo must not silently auto-upgrade. resolveProvider
	// downstream defaults unknowns to v1.
	t.Setenv(ProviderVersionEnvVar, "v3")
	if got := effectiveProviderVersion(); got != "v3" {
		t.Errorf("unrecognised value = %q, want passthrough %q", got, "v3")
	}
}

func TestEffectiveProviderVersion_SplitHostPicksV2(t *testing.T) {
	t.Setenv(ProviderVersionEnvVar, "")
	t.Setenv(api.BaseURLEnvVar, "https://api.example.com")
	t.Setenv(api.AuthBaseURLEnvVar, "https://auth.example.com")

	if got := effectiveProviderVersion(); got != "v2" {
		t.Errorf("split-host auto-detect = %q, want v2", got)
	}
}

// Redundant ENTIRE_AUTH_BASE_URL (same origin as data API, just
// cosmetically different) must not register as split — relies on
// AuthBaseURL/BaseURL canonicalisation.
func TestEffectiveProviderVersion_SameHostPicksV1(t *testing.T) {
	t.Setenv(ProviderVersionEnvVar, "")
	t.Setenv(api.BaseURLEnvVar, "https://api.example.com")
	t.Setenv(api.AuthBaseURLEnvVar, "https://api.example.com/")

	if got := effectiveProviderVersion(); got != "v1" {
		t.Errorf("same-host auto-detect = %q, want v1 (no split-host signal)", got)
	}
}

// Default production path — split-host (entire.io + us.auth.entire.io)
// resolves to v2 without any env vars set. Most users hit this.
func TestEffectiveProviderVersion_UnsetDefaultsToV2(t *testing.T) {
	t.Setenv(ProviderVersionEnvVar, "")
	t.Setenv(api.BaseURLEnvVar, "")
	t.Setenv(api.AuthBaseURLEnvVar, "")

	if got := effectiveProviderVersion(); got != "v2" {
		t.Errorf("unset env = %q, want v2 (split-host default)", got)
	}
}

func TestSetProviderForTest_Overrides(t *testing.T) {
	t.Parallel()

	custom := Provider{
		ClientID:       "test-cli",
		DeviceCodePath: "/custom/device",
		TokenPath:      "/custom/token",
		STSPath:        "/custom/sts",
		AuthTokensPath: "/custom/tokens",
	}
	SetProviderForTest(t, custom)

	got := CurrentProvider()
	if got.DeviceCodePath != "/custom/device" {
		t.Errorf("DeviceCodePath = %q, want override", got.DeviceCodePath)
	}
	if got.STSPath != "/custom/sts" {
		t.Errorf("STSPath = %q, want override", got.STSPath)
	}
}
