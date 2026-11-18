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
// the key/value and URI rules must catch it with the scanners off. Not
// parallel: scanner selection is process-global.
func TestString_ShellParameterDefaultsCaughtWithoutScanners(t *testing.T) {
	assertStringRedactionCasesWithoutScanners(t, []stringRedactionCase{
		{
			name:  "credential value with a real default on a positional parameter",
			input: "DB_PASSWORD=${1:-hunter2}",
			want:  "DB_PASSWORD=REDACTED",
		},
		{
			name:  "one-letter default on a positional parameter",
			input: "PGPASSWORD=${2:-x}",
			want:  "PGPASSWORD=REDACTED",
		},
		{
			name:  "colonless default on a two-digit positional parameter",
			input: "DB_PASSWORD=${10-hunter2}",
			want:  "DB_PASSWORD=REDACTED",
		},
		{
			name:  "alternate value on a positional parameter",
			input: "DB_PASSWORD=${1:+hunter2}",
			want:  "DB_PASSWORD=REDACTED",
		},
		{
			name:  "default on the special parameter @",
			input: "DB_PASSWORD=${@:-hunter2}",
			want:  "DB_PASSWORD=REDACTED",
		},
		{
			name:  "default on the special parameter #",
			input: "DB_PASSWORD=${#:-hunter2}",
			want:  "DB_PASSWORD=REDACTED",
		},
		{
			name:  "default on the special parameter -",
			input: "DB_PASSWORD=${--hunter2}",
			want:  "DB_PASSWORD=REDACTED",
		},
		{
			name:  "default behind an indirect reference",
			input: "DB_PASSWORD=${!ref:-hunter2}",
			want:  "DB_PASSWORD=REDACTED",
		},
		{
			name:  "default on an array element",
			input: "DB_PASSWORD=${pw[0]:-hunter2}",
			want:  "DB_PASSWORD=REDACTED",
		},
		{
			name:  "default on an array element whose subscript holds an element",
			input: "DB_PASSWORD=${pw[idx[0]]:-hunter2}",
			want:  "DB_PASSWORD=REDACTED",
		},
		{
			name:  "default on an array element whose subscript holds an expansion",
			input: "DB_PASSWORD=${pw[${idx[0]}]:-hunter2}",
			want:  "DB_PASSWORD=REDACTED",
		},
		{
			name:  "default on an array element with an arithmetic subscript",
			input: "DB_PASSWORD=${pw[$((i+1))]:-hunter2}",
			want:  "DB_PASSWORD=REDACTED",
		},
		{
			name:  "literal in a substitution on a positional parameter",
			input: "DB_PASSWORD=${1/x/hunter2}",
			want:  "DB_PASSWORD=REDACTED",
		},
		{
			name:  "literal in a substitution on a nested array element",
			input: "DB_PASSWORD=${pw[idx[0]]/x/hunter2}",
			want:  "DB_PASSWORD=REDACTED",
		},
		{
			name:  "quoted positional default",
			input: `export PGPASSWORD="${2:-s3cr3t}"`,
			want:  `export PGPASSWORD="REDACTED"`,
		},
		{
			name:  "positional default under another credential key",
			input: "API_KEY=${1:-hunter2}",
			want:  "API_KEY=REDACTED",
		},
		{
			name:  "credentialed URI with a positional default",
			input: "postgres://app:${1:-hunter2}@db/app",
			want:  "REDACTED",
		},
		{
			name:  "single-quoted default is a literal, not a reference",
			input: `DB_PASSWORD=${PW:-'$S3CRET'}`,
			want:  "DB_PASSWORD=REDACTED",
		},
		{
			name:  "single-quoted credential default is a literal",
			input: `API_KEY=${K:-'$S3CRET'}`,
			want:  "API_KEY=REDACTED",
		},
		{
			name:  "single-quoted default on a positional parameter",
			input: `DB_PASSWORD=${1:-'$S3CRET'}`,
			want:  "DB_PASSWORD=REDACTED",
		},
		{
			name:  "single-quoted default in a URI password",
			input: `postgres://app:${PW:-'$S3CRET'}@db.example.com/app`,
			want:  "REDACTED",
		},
		{
			name:  "single-quoted default in a URI after an empty string",
			input: `psql ""postgres://app:${PW:-'$S3CRET'}@db/app`,
			want:  `psql ""REDACTED`,
		},
		{
			name:  "single-quoted default in a URI after an empty string that a quote follows",
			input: `psql ""postgres://app:${PW:-'$S3CRET'}@db/app"`,
			want:  `psql ""REDACTED"`,
		},
		{
			name:  "single-quoted default in a URI after a closed string",
			input: `psql "x"postgres://app:${PW:-'$S3CRET'}@db/app`,
			want:  `psql "x"REDACTED`,
		},
		{
			name:  "double-quoted default whose single quotes wrap a literal",
			input: `DB_PASSWORD="${PW:-'hunter2'}"`,
			want:  `DB_PASSWORD="REDACTED"`,
		},
		{
			name:  "literal default inside a double-quoted string",
			input: `echo "DB_PASSWORD=${PW:-hunter2}"`,
			want:  `echo "DB_PASSWORD=REDACTED`,
		},
	})
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
// "${PW:-'$FALLBACK'}" expands $FALLBACK, as it does in a URI or a value that
// a double-quoted string holds; only the unquoted form is the literal text
// $FALLBACK.
func TestString_DoubleQuotedShellDefaultsPreserved(t *testing.T) {
	t.Parallel()
	var cases []stringRedactionCase
	for _, input := range []string{
		`DB_PASSWORD="${PW:-'$FALLBACK'}"`,
		`API_KEY="${KEY:-'$FALLBACK'}"`,
		`PGPASSWORD="${1:-'$FALLBACK'}" psql -h db`,
		`echo "DB_PASSWORD=${PW:-'$FALLBACK'}" done`,
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
		"${A}${B:-'x'}":      "${A}${B:-'x'}",
	} {
		if got := doubleQuotedText(text); got != want {
			t.Errorf("doubleQuotedText(%q) = %q, want %q", text, got, want)
		}
	}
}
