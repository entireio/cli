package cli

import (
	"slices"
	"testing"
)

// A superset of entire-core's TestIsReservedHostEmail so parity
// drift with regional.IsReservedHostEmail fails here, not as a server 400.
func TestReservedHostEmail(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   string
		want bool
	}{
		{"coledriver@coles-macbook-pro.local", true},
		{"lizziesiegle@lizzies-macbook-pro.local", true},
		{"peytonmontei@mac.localdomain", true},
		{"coledriver@Coles-MacBook-Pro.local", true}, // predicate lowercases
		{"dev@localhost", true},
		{"dev@box.localhost", true},
		{"me@host.internal", true},
		{"me@host.lan", true},
		{"me@host.home", true},
		{"me@host.home.arpa", true},
		{"me@host.test", true},
		{"me@host.example", true},
		{"me@host.invalid", true},
		{"me@host.local.", true},   // one trailing dot tolerated
		{"me@host.local..", false}, // only one
		{"me@local", true},         // bare suffix is intentional
		{"me@.local", false},
		{"   ", false},
		{"bob@company.com", false},
		{"jdoe@lt-4471.corp.acme.com", false},
		{"me@fritz.box", false},
		{"me@attlocal.net", false},
		{"12345+octo@users.noreply.github.com", false},
		{"me@notlocal", false},
		{"me@host.notlocal", false},
		{"me@somehost", false}, // dotless host is NOT reserved
		{"", false},
		{"nolocalpart@", false},
		{"@host.local", false},
		{"two@at@host.local", false},
		{"me@xn--bcher-kva.local", false},
		{"me@hóst.local", false},
	}
	for _, c := range cases {
		if got := reservedHostEmail(c.in); got != c.want {
			t.Errorf("reservedHostEmail(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

// The authorship gate: only addresses whose local part is the current OS user
// are candidates. Hostname is deliberately not matched (renamed / new laptop).
func TestFilterCandidateAuthors(t *testing.T) {
	t.Parallel()
	authors := []string{
		"coledriver@Coles-MacBook-Pro.local",
		"coledriver@New-Laptop.local",            // same person, new machine — kept
		"lizziesiegle@Lizzies-MacBook-Pro.local", // colleague — dropped
		"coledriver@company.com",                 // not reserved-host — dropped
		"COLEDRIVER@Old-Mac.local.",              // case + trailing dot — kept, normalized
		"coledriver@Coles-MacBook-Pro.local",     // duplicate — deduped
		"coledriver@host.local..",                // two trailing dots — dropped, same as the predicate
	}
	got := filterCandidateAuthors(authors, "coledriver")
	want := []string{"coledriver@coles-macbook-pro.local", "coledriver@new-laptop.local", "coledriver@old-mac.local"}
	if !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	if got := filterCandidateAuthors(authors, ""); got != nil {
		t.Fatalf("empty username must yield no candidates, got %v", got)
	}
	got = filterCandidateAuthors(authors, "ColeDriver")
	if !slices.Equal(got, want) {
		t.Fatalf("ColeDriver (unnormalized) got %v, want %v", got, want)
	}
}

func TestNormalizeOSUsername(t *testing.T) {
	t.Parallel()
	cases := []struct{ in, want string }{
		{"coledriver", "coledriver"},
		{"ColeDriver", "coledriver"},
		{`CORP\jdoe`, "jdoe"}, // Windows DOMAIN\user
		{"  coledriver ", "coledriver"},
		{"", ""},
	}
	for _, c := range cases {
		if got := normalizeOSUsername(c.in); got != c.want {
			t.Errorf("normalizeOSUsername(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// git shortlog -se lines are "<tab-padded count>\t<Name> <email>".
func TestAuthorsFromShortlog(t *testing.T) {
	t.Parallel()
	in := "    12\tCole Driver <coledriver@Coles-MacBook-Pro.local>\n     1\tLizzie <lizziesiegle@lizzies.local>\n\n"
	got := authorsFromShortlog([]byte(in))
	want := []string{"coledriver@Coles-MacBook-Pro.local", "lizziesiegle@lizzies.local"}
	if !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}
