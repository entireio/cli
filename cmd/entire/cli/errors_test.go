package cli

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/internal/entireclient/tokenstore"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRenderUserFacingError_HeadlessKeyringHint(t *testing.T) {
	cases := []struct {
		name    string
		errText string
	}{
		{
			name:    "org.freedesktop.secrets missing service (#3058)",
			errText: "resolve repo: refresh login token: load core token: read access token: The name org.freedesktop.secrets was not provided by any .service files",
		},
		{
			name:    "dbus autolaunch without X11 DISPLAY",
			errText: "read access token: Cannot autolaunch D-Bus without X11 $DISPLAY",
		},
		{
			name:    "no such secret service",
			errText: "token store get: no such secret service at /org/freedesktop/secrets",
		},
		{
			name:    "macOS keychain inaccessible",
			errText: "the macOS Keychain is not accessible from this process",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("ENTIRE_TOKEN_STORE", "")
			restore := tokenstore.UseFailingBackendForTesting(filepath.Join(t.TempDir(), "tokens.json"), func(string, string) bool { return true })
			t.Cleanup(restore)

			err := errors.New(tc.errText)
			rendered := RenderUserFacingError(err)
			require.NotNil(t, rendered)
			got := rendered.Error()

			assert.Contains(t, got, tc.errText)
			assert.Contains(t, got, "ENTIRE_TOKEN_STORE=file")
			assert.Contains(t, got, "ENTIRE_TOKEN_STORE_PATH")
			assert.Contains(t, got, "If this machine has no usable OS keyring")
		})
	}
}

func TestRenderUserFacingError_KeyringUnavailableSentinel(t *testing.T) {
	t.Setenv("ENTIRE_TOKEN_STORE", "")
	restore := tokenstore.UseFailingBackendForTesting(filepath.Join(t.TempDir(), "tokens.json"), func(string, string) bool { return true })
	t.Cleanup(restore)

	err := fmt.Errorf("read token: %w", tokenstore.ErrKeyringUnavailable)
	rendered := RenderUserFacingError(err)
	require.NotNil(t, rendered)
	assert.Contains(t, rendered.Error(), "ENTIRE_TOKEN_STORE=file")
}

func TestRenderUserFacingError_FileBackendSelected_NoHint(t *testing.T) {
	t.Setenv("ENTIRE_TOKEN_STORE", "file")
	restore := tokenstore.UseFileBackendForTesting(filepath.Join(t.TempDir(), "tokens.json"))
	t.Cleanup(restore)

	err := errors.New("read access token: The name org.freedesktop.secrets was not provided by any .service files")
	rendered := RenderUserFacingError(err)
	require.NotNil(t, rendered)
	assert.NotContains(t, rendered.Error(), "If this machine has no usable OS keyring")
	assert.NotContains(t, rendered.Error(), "ENTIRE_TOKEN_STORE=file")
}

func TestRenderUserFacingError_BadConfig_NoHint(t *testing.T) {
	t.Setenv("ENTIRE_TOKEN_STORE", "")

	err := fmt.Errorf("load config: %w", tokenstore.ErrBadConfig)
	rendered := RenderUserFacingError(err)
	require.NotNil(t, rendered)
	assert.NotContains(t, rendered.Error(), "If this machine has no usable OS keyring")
	assert.NotContains(t, rendered.Error(), "ENTIRE_TOKEN_STORE=file")
}

func TestRenderUserFacingError_UnrelatedError_NoHint(t *testing.T) {
	t.Setenv("ENTIRE_TOKEN_STORE", "")

	err := errors.New("dial tcp 127.0.0.1:8080: connect: connection refused")
	rendered := RenderUserFacingError(err)
	require.NotNil(t, rendered)
	assert.Equal(t, "dial tcp 127.0.0.1:8080: connect: connection refused", rendered.Error())
}

func TestRenderUserFacingError_NoDuplicateHint(t *testing.T) {
	t.Setenv("ENTIRE_TOKEN_STORE", "")
	restore := tokenstore.UseFailingBackendForTesting(filepath.Join(t.TempDir(), "tokens.json"), func(string, string) bool { return true })
	t.Cleanup(restore)

	err := errors.New("The name org.freedesktop.secrets was not provided by any .service files")
	first := RenderUserFacingError(err)
	second := RenderUserFacingError(first)

	assert.Equal(t, first.Error(), second.Error(), "hint should not be duplicated")
	assert.Equal(t, 1, strings.Count(second.Error(), "ENTIRE_TOKEN_STORE=file"))
}
