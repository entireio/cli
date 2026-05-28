// Package clicobra holds shared helpers used by the entire-* CLI binaries
// (entire-core, entire-repo, entire-org, entire-project, entire-grant)
// for things cobra itself doesn't provide.
package clicobra

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"
)

// MustGetString reads a string flag that was registered on cmd. It panics
// if the flag isn't registered — that's a programmer error, not user
// input. Use this instead of cmd.Flag(name).Value.String(), which returns
// "" silently when the flag is missing.
func MustGetString(cmd *cobra.Command, name string) string {
	v, err := cmd.Flags().GetString(name)
	if err != nil {
		panic(fmt.Sprintf("flag %q not registered: %v", name, err))
	}
	return v
}

// PrintJSON pretty-prints data as indented JSON to stdout. When data
// isn't valid JSON (e.g. an empty 204 body), it falls through and prints
// the raw bytes so debugging stays cheap.
func PrintJSON(data []byte) error {
	var v any
	if err := json.Unmarshal(data, &v); err != nil {
		fmt.Println(string(data))
		return nil //nolint:nilerr // intentional: print raw data when not valid JSON
	}
	out, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("format json: %w", err)
	}
	fmt.Println(string(out))
	return nil
}
