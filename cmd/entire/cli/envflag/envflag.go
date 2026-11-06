// Package envflag reads on/off environment variables.
package envflag

import (
	"os"
	"strings"
)

// Truthy reports whether value means on.
// Empty, "0", and "false" (any case) mean off.
func Truthy(value string) bool {
	return value != "" && value != "0" && !strings.EqualFold(value, "false")
}

// Enabled reports whether variable name is on.
func Enabled(name string) bool {
	return Truthy(os.Getenv(name))
}
