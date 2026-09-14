package userdirs

import (
	"errors"
	"testing"
)

func TestResolveConfig_HomeFailureReturnsError(t *testing.T) {
	t.Parallel()

	_, err := resolveConfig("", "", false, func() (string, error) {
		return "", errors.New("home unavailable")
	})
	if err == nil {
		t.Fatal("an unresolvable home must be an error, never a CWD-relative config directory")
	}
}

func TestResolveCache_HomeFailureReturnsError(t *testing.T) {
	t.Parallel()

	_, err := resolveCache("", "", false, func() (string, error) {
		return "", errors.New("home unavailable")
	})
	if err == nil {
		t.Fatal("an unresolvable home must be an error, never a CWD-relative cache directory")
	}
}
