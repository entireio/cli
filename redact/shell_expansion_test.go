package redact

import "testing"

// assertStringRedactionCasesWithoutScanners checks each case with only the
// key/value rules on, then restores the default scanners. The scanner
// configuration is process-wide, so the cases run one at a time.
func assertStringRedactionCasesWithoutScanners(t *testing.T, tests []stringRedactionCase) {
	t.Helper()
	t.Cleanup(func() {
		if err := ConfigureScanners(ScannersConfig{Betterleaks: true}); err != nil {
			t.Fatalf("restore default scanners: %v", err)
		}
	})
	if err := ConfigureScanners(ScannersConfig{}); err != nil {
		t.Fatalf("ConfigureScanners(none): %v", err)
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := String(tt.input); got != tt.want {
				t.Errorf("String(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

// A default on a positional or special parameter, an indirect reference or an
// array element is a value like the default in ${DB_PASSWORD:-hunter2}, and
// the key/value and URI rules must catch it with the scanners off. A single
// quoted default outside double quotes is literal text, nested ones included.
// Not parallel: scanner selection is process-global.
func TestString_ShellParameterDefaultsCaughtWithoutScanners(t *testing.T) {
	var cases []stringRedactionCase
	for _, value := range []string{
		"${1:-hunter2}", "${2:-x}", "${10-hunter2}", "${1:+hunter2}",
		"${@:-hunter2}", "${#:-hunter2}", "${--hunter2}", "${!ref:-hunter2}",
		"${pw[0]:-hunter2}", "${pw[idx[0]]:-hunter2}", "${pw[${idx[0]}]:-hunter2}",
		"${pw[$((i+1))]:-hunter2}", "${1/x/hunter2}", "${pw[idx[0]]/x/hunter2}",
		"${PW:-'$S3CRET'}", "${1:-'$S3CRET'}", "${A:-${B:-'$S3CRET'}}",
	} {
		cases = append(cases, stringRedactionCase{
			name:  value,
			input: "DB_PASSWORD=" + value,
			want:  "DB_PASSWORD=REDACTED",
		})
	}
	for input, want := range map[string]string{
		`export PGPASSWORD="${2:-s3cr3t}"`:                   `export PGPASSWORD="REDACTED"`,
		"API_KEY=${1:-hunter2}":                              "API_KEY=REDACTED",
		`API_KEY=${K:-'$S3CRET'}`:                            "API_KEY=REDACTED",
		"postgres://app:${1:-hunter2}@db/app":                "REDACTED",
		`postgres://app:${PW:-'$S3CRET'}@db.example.com/app`: "REDACTED",
		`DB_PASSWORD="${PW:-'hunter2'}"`:                     `DB_PASSWORD="REDACTED"`,
		`echo "DB_PASSWORD=${PW:-hunter2}"`:                  `echo "DB_PASSWORD=REDACTED`,
		// Quotes that close a string, or are escaped, leave the default
		// outside double quotes.
		`psql ""postgres://app:${PW:-'$S3CRET'}@db/app`:            `psql ""REDACTED`,
		`psql ""postgres://app:${PW:-'$S3CRET'}@db/app"`:           `psql ""REDACTED"`,
		`psql "x"postgres://app:${PW:-'$S3CRET'}@db/app`:           `psql "x"REDACTED`,
		`psql "a"postgres://app:${PW:-'$S3CRET'}@db/app "b"`:       `psql "a"REDACTED "b"`,
		`echo "a"DB_PASSWORD=${PW:-'$S3CRET'} b"`:                  `echo "a"DB_PASSWORD=REDACTED b"`,
		`echo "a DB_PASSWORD="${PW:-'$S3CRET'}"`:                   `echo "a DB_PASSWORD="REDACTED"`,
		`psql \"postgres://app:${PW:-'$S3CRET'}@db/app\"`:          `psql \"REDACTED"`,
		`printf '%s\n' \"postgres://app:${PW:-'$S3CRET'}@db/app\"`: `printf '%s\n' \"REDACTED"`,
		`echo \"DB_PASSWORD=${PW:-'$S3CRET'} more\"`:               `echo \"DB_PASSWORD=REDACTED more\"`,
		// A single quote earlier on the line can change which quotes pair, so
		// the value is read as unquoted: this redacts more than a shell needs.
		`echo 'it' "DB_PASSWORD=${PW:-'$FALLBACK'}"`: `echo 'it' "DB_PASSWORD=REDACTED`,
	} {
		cases = append(cases, stringRedactionCase{name: input, input: input, want: want})
	}
	assertStringRedactionCasesWithoutScanners(t, cases)
}

// Shell expansions on positional and special parameters, indirect references
// and array elements are references like ${DB_PASSWORD}: only a default or
// other text inside them is a secret.
func TestString_ShellParameterReferencesPreserved(t *testing.T) {
	t.Parallel()
	var cases []stringRedactionCase
	for _, value := range []string{
		"${1}", "${10}", "${1:-}", "${1-}", "${1:-$DEFAULT_PW}", "${2:-${FALLBACK_PASSWORD}}",
		"${@}", "${*}", "${#}", "${?}", "${-}", "${$}", "${!}", "${0}",
		"${#1}", "${##}", "${#@}", "${#pw[@]}",
		"${!ref}", "${!pre*}", "${!pw[@]}", "${pw[0]}", "${pw[@]}",
		"${pw[idx[0]]}", "${pw[${idx[0]}]}", "${#pw[idx[0]]}", "${pw[idx[0]]:-$DEFAULT_PW}",
		"${1:-''}", "${PW:-'changeme'}", "${PW:-'<password>'}",
	} {
		cases = append(cases, stringRedactionCase{
			name:  value,
			input: "DB_PASSWORD=" + value,
			want:  "DB_PASSWORD=" + value,
		})
	}
	assertStringRedactionCases(t, cases)
}

// Inside double quotes a single quote is a plain character, so the default in
// "${PW:-'$FALLBACK'}" expands $FALLBACK, as it does nested, in a URI, or in a
// value that a double-quoted string holds with more text after it; only the
// unquoted form is the literal text $FALLBACK.
func TestString_DoubleQuotedShellDefaultsPreserved(t *testing.T) {
	t.Parallel()
	var cases []stringRedactionCase
	for _, input := range []string{
		`DB_PASSWORD="${PW:-'$FALLBACK'}"`,
		`API_KEY="${KEY:-'$FALLBACK'}"`,
		`PGPASSWORD="${1:-'$FALLBACK'}" psql -h db`,
		`echo "DB_PASSWORD=${PW:-'$FALLBACK'}" done`,
		`echo "DB_PASSWORD=${PW:-'$FALLBACK'} more"`,
		`echo "x" "DB_PASSWORD=${PW:-'$FALLBACK'}"`,
		`echo "API_KEY=${KEY:-'$FALLBACK'} x"`,
		`DB_PASSWORD="${PW:-${FALLBACK:-'$DEFAULT_PASSWORD'}}"`,
		`DB_PASSWORD="${A:-${B:-'$FALLBACK'}}"`,
		`psql "postgres://app:${PW:-${F:-'$X'}}@db/app"`,
		`echo "API_KEY=${KEY:-'$FALLBACK'}"`,
		`psql "postgres://app:${PW:-'$FALLBACK'}@db/app"`,
		`echo "DB_PASSWORD=${PW}"`,
		`echo "DB_PASSWORD=${PW:-}"`,
		`echo "DB_PASSWORD=${1:-$FALLBACK}"`,
	} {
		cases = append(cases, stringRedactionCase{name: input, input: input, want: input})
	}
	assertStringRedactionCases(t, cases)
}

func TestIsPlaceholderShellExpansion_ParameterForms(t *testing.T) {
	t.Parallel()
	for value, want := range map[string]bool{
		"${1}":                   true,
		"${#1}":                  true,
		"${##}":                  true,
		"${!ref}":                true,
		"${pw[idx[0]]}":          true,
		"${pw[${idx[0]}]}":       true,
		"${1:-hunter2}":          false,
		"${#:-hunter2}":          false,
		"${pw[idx[0]]:-hunter2}": false,
		"${PW:-'$S3CRET'}":       false,
		"${PW:-'changeme'}":      true,
		"${PW:-$FALLBACK}":       true,
		// A subscript nested two levels deep is not read as a name, as before.
		"${pw[a[b[0]]]:-hunter2}": true,
	} {
		if got := isPlaceholderShellExpansion(value); got != want {
			t.Errorf("isPlaceholderShellExpansion(%q) = %v, want %v", value, got, want)
		}
	}
}

func TestDoubleQuotedText(t *testing.T) {
	t.Parallel()
	for text, want := range map[string]string{
		"${PW:-'$FALLBACK'}": "${PW:-$FALLBACK}",
		"${1:-'$FALLBACK'}":  "${1:-$FALLBACK}",
		"${PW:-$FALLBACK}":   "${PW:-$FALLBACK}",
		"${PW:-'a'b}":        "${PW:-'a'b}",
		"'$FALLBACK'":        "'$FALLBACK'",
		"${A}${B:-'x'}":      "${A}${B:-x}",
		"${PW:-${F:-'$X'}}":  "${PW:-${F:-$X}}",
	} {
		if got := doubleQuotedText(text); got != want {
			t.Errorf("doubleQuotedText(%q) = %q, want %q", text, got, want)
		}
	}
}
