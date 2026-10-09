package external

import "testing"

func TestRecordCallerEnvVarsKeepsOnlyPlainNames(t *testing.T) {
	t.Parallel()

	recordCallerEnvVars([]string{"KILO_RUN_ID", "lower_case", "BAD-NAME", "X", "PATH=/evil"}, "Kilo")
	got := CallerEnvVars()
	if got["KILO_RUN_ID"] != "Kilo" {
		t.Errorf("KILO_RUN_ID not recorded: %v", got)
	}
	for _, bad := range []string{"lower_case", "BAD-NAME", "X", "PATH=/evil"} {
		if _, ok := got[bad]; ok {
			t.Errorf("invalid name %q recorded", bad)
		}
	}
}
