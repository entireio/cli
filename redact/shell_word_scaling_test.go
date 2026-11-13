package redact

import (
	"math/rand/v2"
	"slices"
	"strings"
	"testing"
	"time"
)

// joinedAssignmentUnits are shell and key/value fragments that, repeated with
// nothing between them, make one long run of joined words, values that start
// inside other values, and keys that a value runs into.
var joinedAssignmentUnits = []string{
	`DB_PASSWORD=a" "`, `API_KEY=a" "`, `DB_PASSWORD=a" "=`, `DB_PASSWORD=a" DB_PASSWORD=b"|`,
	`DB_PASSWORD=a' DB_PASSWORD=b'|`, `db_password=foo,client_secret = `, `DB_PASSWORD=a" API_KEY="b `,
	`DB_PASSWORD=a' API_KEY="b' `, `DB_PASSWORD=a" FOO="b `, `DB_PASSWORD=a" FOO='b API_KEY="c' `,
	`DB_PASSWORD=a\" API_KEY=b\"|c `, `API_KEY=a" API_KEY="b API_KEY=c"API_KEY = `,
	`DB_PASSWORD="a\"b";API_KEY=a"b;c"x `, "GITHUB_CLIENT_SECRET=\"a;b\n", `DB_PASSWORD="a"b= `,
	`API_KEY=a=" API_KEY=aaa\"|bbb" `, `FOO="x"API_KEY=a `, `DB_PASSWORD="x DB_PASSWORD=a\";b" `, `FOO="x"b=`,
	`DB_PASSWORD=a"b"\|`, `API_KEY=${A}"${B}",`, `printf a"|x secrets put K; printf "b c"|x secrets put K; `,
	`DB_PASSWORD="a"|`, `API_KEY='a'|`, `DB_PASSWORD=`, `API_KEY="!a=b"|`, `API_KEY="x""a=b"|`,
	`DB_PASSWORD="!a=b"|`, `API_KEY="x"FOO=|`, `API_KEY="x"_FOO=|`, `DB_PASSWORD='DB_PASSWORD=+`,
	`API_KEY="API_KEY=+`,
}

// joinedAssignments repeats unit n times. A unit that leaves the last value
// open gets a secret after it, which that value takes.
func joinedAssignments(unit string, n int) string {
	in := strings.Repeat(unit, n)
	if unit == "DB_PASSWORD=" || strings.HasSuffix(unit, "=+") {
		in += "hunter2,"
	}
	return in
}

// leftAsCaptured holds the units whose last value stays visible, as it does when
// each value is read only as captured: a value that starts inside a word and
// runs past it is read only as captured.
var leftAsCaptured = []string{
	`API_KEY=a" API_KEY="b API_KEY=c"API_KEY = `,
	`API_KEY="API_KEY=+`,
}

// fastestRun returns the shortest of three timed calls of f, which leaves out
// most of the noise a busy machine adds.
func fastestRun(f func()) time.Duration {
	best := time.Duration(1<<63 - 1)
	for range 3 {
		start := time.Now()
		f()
		best = min(best, time.Since(start))
	}
	return max(best, time.Microsecond)
}

// Each key/value pass reads a shell word once, so redacting a run of joined
// assignments takes time linear in its length. The check compares two sizes
// rather than a wall-clock budget, so a slow or busy machine doesn't fail it:
// on four times the text a linear reading takes about four times as long and
// a quadratic one about sixteen. Not parallel: it times String, and scanner
// selection is process-global.
func TestString_JoinedShellWordsScaleLinearly(t *testing.T) {
	if testing.Short() {
		t.Skip("times String on long inputs")
	}
	t.Cleanup(func() {
		if err := ConfigureScanners(ScannersConfig{Betterleaks: true}); err != nil {
			t.Fatalf("restore default scanners: %v", err)
		}
	})
	if err := ConfigureScanners(ScannersConfig{}); err != nil {
		t.Fatalf("ConfigureScanners(none): %v", err)
	}
	const n = 500
	for _, unit := range joinedAssignmentUnits {
		small, large := joinedAssignments(unit, n), joinedAssignments(unit, 4*n)
		out := String(large)
		// A pause can still land on one size, so a ratio over the bound is
		// measured again before it counts; a quadratic reading stays over it.
		var ratio float64
		var smallTime, largeTime time.Duration
		for range 3 {
			smallTime = fastestRun(func() { String(small) })
			largeTime = fastestRun(func() { String(large) })
			if ratio = float64(largeTime) / float64(smallTime); ratio <= 6 {
				break
			}
		}
		if ratio > 6 {
			t.Errorf("String on %d joined %q took %v, %.1f times its %v on %d", 4*n, unit, largeTime, ratio, smallTime, n)
		}
		if slices.Contains(leftAsCaptured, unit) {
			continue
		}
		if strings.Contains(out, `a" `) || strings.Contains(out, `a' `) || strings.Contains(out, "hunter2") {
			t.Errorf("joined values of %q left visible: %.80q", unit, out)
		}
	}
}

// keyValueRegions returns what the key/value passes redact in s, reading
// each value also as its whole shell word when wholeWords is set.
func keyValueRegions(s string, wholeWords bool) []taggedRegion {
	regions := credentialValueRegions(s, wholeWords)
	return append(regions, detectCredentialKeyCaptures(s, shellStdinSecretPattern, true, nil, wholeWords)...)
}

// assertWordReadingCoversCaptured fails when the key/value passes leave a
// byte of s visible that they redact when each value is read only as the
// patterns' FindAllStringSubmatchIndex captured it.
func assertWordReadingCoversCaptured(t *testing.T, s string) {
	t.Helper()
	covered := make([]bool, len(s))
	for _, r := range keyValueRegions(s, true) {
		for i := r.start; i < r.end; i++ {
			covered[i] = true
		}
	}
	for _, r := range keyValueRegions(s, false) {
		for i := r.start; i < r.end; i++ {
			if !covered[i] {
				t.Fatalf("in %q, %q is redacted as captured but not with whole words", s, s[r.start:r.end])
			}
		}
	}
}

// keyValueFragments build the inputs of the differential test below: keys,
// quotes, separators, escapes, shell expansions and secrets.
var keyValueFragments = []string{
	"DB_PASSWORD=", "API_KEY=", "api_key=", "_api_key=", "PASSWORD=", "DB_USER=", "FOO=", "a=", "b=", "=", " = ",
	`"`, "'", `""`, " ", "\n", ";", ",", "&", "|", `\`, `\"`, "$", "${", "}", "${PW:-", "$X",
	"hunter2", "secretpart", "x", "D", "E= ", ":", "+", "(", ")", "printf ", "|x secrets put K",
}

// Reading each value also as its whole shell word only adds to what the
// key/value passes redact: every byte redacted as captured stays redacted.
// The inputs join random fragments from a fixed seed, so a failure repeats.
func TestCredentialValueRegions_WordReadingCoversCaptured(t *testing.T) {
	t.Parallel()
	random := rand.New(rand.NewPCG(3078, 1))
	for range 20000 {
		var b strings.Builder
		for range 1 + random.IntN(14) {
			b.WriteString(keyValueFragments[random.IntN(len(keyValueFragments))])
		}
		assertWordReadingCoversCaptured(t, b.String())
	}
}

// FuzzCredentialValueRegions_WordReadingCoversCaptured explores the same
// property; its seeds are inputs that leaked under an earlier design, found
// by a differential fuzz or in review.
func FuzzCredentialValueRegions_WordReadingCoversCaptured(f *testing.F) {
	for _, seed := range []string{
		"DB_PASSWORD=D=\nDB_PASSWORD=c= R= E= API_KEY=hunter2",
		`API_KEY=""B_R= E= API_KEY=hunter2`,
		`api_key=""i_e =""_api_key=hunter2`,
		`API_KEY=API_KEY= "API_KEY=":hunter2`,
		`DB_PASSWORD=${'hunter2 '}`,
		`API_KEY=a=;API_KEY=b= DB_USER= DB_NAME= API_KEY=hunter2`,
		`API_KEY=''DB_USER= ""NOTE=API_KEY=hunter2xyz`,
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, s string) {
		assertWordReadingCoversCaptured(t, s)
	})
}
