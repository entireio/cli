package auth

import (
	"testing"
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
