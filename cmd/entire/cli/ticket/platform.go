package ticket

import (
	"fmt"
	"strings"
)

// Platform identifies a supported ticket platform.
type Platform string

const (
	// PlatformLinear is Linear (linear.app).
	PlatformLinear Platform = "linear"
)

// SupportedPlatforms lists the platforms `entire ticket setup` can configure,
// in display order. Only Linear is supported today.
var SupportedPlatforms = []Platform{PlatformLinear}

// DisplayName returns the human-facing name for the platform.
func (p Platform) DisplayName() string {
	switch p {
	case PlatformLinear:
		return "Linear"
	default:
		return string(p)
	}
}

// ParsePlatform validates s against the supported platforms.
func ParsePlatform(s string) (Platform, error) {
	for _, p := range SupportedPlatforms {
		if string(p) == s {
			return p, nil
		}
	}
	return "", fmt.Errorf("unsupported ticket platform %q (supported: %s)", s, supportedList())
}

// supportedList renders the supported platform identifiers for error messages.
func supportedList() string {
	names := make([]string, len(SupportedPlatforms))
	for i, p := range SupportedPlatforms {
		names[i] = string(p)
	}
	return strings.Join(names, ", ")
}
