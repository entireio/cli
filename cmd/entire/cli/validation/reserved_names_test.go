package validation

import (
	"regexp"
	"strings"
	"testing"
)

// Every ID validator must refuse the DOS device names, on every platform: an
// ID travels inside checkpoint metadata and is restored on whatever machine
// pulls it, and on Windows "CON" (in any case, with any extension, with
// trailing dots or spaces) is the console device, not a file.
func TestValidators_RejectWindowsReservedDeviceNames(t *testing.T) {
	t.Parallel()
	validators := map[string]func(string) error{
		"ValidateSessionID":      ValidateSessionID,
		"ValidateToolUseID":      ValidateToolUseID,
		"ValidateAgentID":        ValidateAgentID,
		"ValidateAgentSessionID": ValidateAgentSessionID,
	}
	reserved := []string{
		"CON", "con", "Nul", "PRN", "AUX",
		"COM0", "COM1", "com9", "LPT0", "lpt9",
		"CON.txt", "nul.json", "COM1.", "aux ", "prn .log",
	}
	fine := []string{"CONSOLE", "console-1", "COM", "LPT", "com10", "nul_x", "auxiliary", "prn2", "lpt-1"}
	// Three of the validators already restrict IDs to [A-Za-z0-9_-]; only
	// check "must pass" on names those regexes admit.
	regexSafe := regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

	for name, validate := range validators {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			for _, id := range reserved {
				if err := validate(id); err == nil {
					t.Errorf("%s(%q) = nil, want a rejection", name, id)
				}
			}
			for _, id := range fine {
				if !regexSafe.MatchString(id) {
					continue
				}
				if err := validate(id); err != nil {
					t.Errorf("%s(%q) = %v, want accepted (not a reserved device name)", name, id, err)
				}
			}
		})
	}

	err := ValidateSessionID("CON")
	if err == nil || !strings.Contains(err.Error(), "reserved device name") {
		t.Fatalf("ValidateSessionID(CON) = %v, want the reserved-device-name reason", err)
	}
}
