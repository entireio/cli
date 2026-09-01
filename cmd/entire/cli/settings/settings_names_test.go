package settings

import (
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/entiredir"
)

// The two spellings of each settings file are declared separately (a const block
// cannot call MustName), so nothing but this test stops them drifting apart. A
// mismatch would send reads and writes to a different file than the one every
// message, gitignore entry, and tracked-file check names.
func TestSettingsNamesMatchRepoRelativePaths(t *testing.T) {
	t.Parallel()

	if got := entiredir.MustName(EntireSettingsFile); got != SettingsName {
		t.Errorf("SettingsName = %q, but %q resolves to %q", SettingsName, EntireSettingsFile, got)
	}
	if got := entiredir.MustName(EntireSettingsLocalFile); got != SettingsLocalName {
		t.Errorf("SettingsLocalName = %q, but %q resolves to %q", SettingsLocalName, EntireSettingsLocalFile, got)
	}
}
