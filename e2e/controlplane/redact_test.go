package controlplane

import "testing"

func TestNewRedactor(t *testing.T) {
	t.Parallel()
	redact := newRedactor("Test.User", "hunter2", "JBSWY3DP", "ABCD-EFGH", "https://entire.io/cli/auth?code=ABCD-EFGH")
	got := redact("Signed in as test.user (TEST.USER, test.user@example.com) with hunter2, " +
		"secret JBSWY3DP, code ABCD-EFGH at https://entire.io/cli/auth?code=ABCD-EFGH; testXuser stays")
	want := "Signed in as [username] ([username], [email]) with [password], " +
		"secret [totp secret], code [device code] at [approval url]; testXuser stays"
	if got != want {
		t.Fatalf("redact:\n got %q\nwant %q", got, want)
	}
}
