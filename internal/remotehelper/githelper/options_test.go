package githelper

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

func TestOptions_SetAndSendPackArgs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		commands  [][2]string
		wantReply []string
		wantArgs  []string
	}{
		{
			name:      "no options accepted",
			commands:  [][2]string{{"object-format", "sha1"}},
			wantReply: []string{"ok"},
			wantArgs:  nil,
		},
		{
			name:      "dry-run on",
			commands:  [][2]string{{"dry-run", "true"}},
			wantReply: []string{"ok"},
			wantArgs:  []string{"--dry-run"},
		},
		{
			name:      "atomic + force-if-includes",
			commands:  [][2]string{{"atomic", "true"}, {"force-if-includes", "true"}},
			wantReply: []string{"ok", "ok"},
			wantArgs:  []string{"--atomic", "--force-if-includes"},
		},
		{
			name:      "push-cert true → signed=true",
			commands:  [][2]string{{"push-cert", "true"}},
			wantReply: []string{"ok"},
			wantArgs:  []string{"--signed=true"},
		},
		{
			name:      "push-cert if-asked",
			commands:  [][2]string{{"push-cert", "if-asked"}},
			wantReply: []string{"ok"},
			wantArgs:  []string{"--signed=if-asked"},
		},
		{
			name:      "pushcert, git's option name",
			commands:  [][2]string{{"pushcert", "true"}},
			wantReply: []string{"ok"},
			wantArgs:  []string{"--signed=true"},
		},
		{
			name:      "push-option and cas accumulate",
			commands:  [][2]string{{"push-option", "ci.skip"}, {"push-option", "ref-prefix=refs/heads/"}, {"cas", "refs/heads/main:deadbeef"}},
			wantReply: []string{"ok", "ok", "ok"},
			wantArgs:  []string{"--push-option=ci.skip", "--push-option=ref-prefix=refs/heads/", "--force-with-lease=refs/heads/main:deadbeef"},
		},
		{
			name:      "cas octal UTF-8 is unquoted",
			commands:  [][2]string{{"cas", `"refs/heads/caf\303\251:deadbeef"`}},
			wantReply: []string{"ok"},
			wantArgs:  []string{"--force-with-lease=refs/heads/café:deadbeef"},
		},
		{
			name:      "cas escaped quote is unquoted",
			commands:  [][2]string{{"cas", `"refs/heads/feat/\"quoted\":deadbeef"`}},
			wantReply: []string{"ok"},
			wantArgs:  []string{`--force-with-lease=refs/heads/feat/"quoted":deadbeef`},
		},
		{
			name:      "push-option escaped backslash is unquoted",
			commands:  [][2]string{{"push-option", `"back\\slash"`}},
			wantReply: []string{"ok"},
			wantArgs:  []string{`--push-option=back\slash`},
		},
		{
			name:      "push-option control escapes are unquoted",
			commands:  [][2]string{{"push-option", `"a\tb\nc"`}},
			wantReply: []string{"ok"},
			wantArgs:  []string{"--push-option=a\tb\nc"},
		},
		{
			name:      "unquoted value with backslash is verbatim",
			commands:  [][2]string{{"push-option", `a\303"b`}},
			wantReply: []string{"ok"},
			wantArgs:  []string{`--push-option=a\303"b`},
		},
		{
			name:      "malformed cas is rejected and not forwarded",
			commands:  [][2]string{{"cas", `"refs/heads/main:deadbeef`}},
			wantReply: []string{"error invalid value"},
			wantArgs:  nil,
		},
		{
			name:      "malformed push-option is rejected and not forwarded",
			commands:  [][2]string{{"push-option", `"bad\q"`}, {"push-option", "ok"}},
			wantReply: []string{"error invalid value", "ok"},
			wantArgs:  []string{"--push-option=ok"},
		},
		{
			name:      "verbosity is accepted and not forwarded",
			commands:  [][2]string{{"verbosity", "0"}, {"verbosity", "2"}},
			wantReply: []string{"ok", "ok"},
			wantArgs:  nil,
		},
		{
			name:      "unknown option → unsupported",
			commands:  [][2]string{{"weird-option", "value"}},
			wantReply: []string{"unsupported"},
			wantArgs:  nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			opts := &Options{}
			for i, cmd := range tt.commands {
				if reply := opts.Set(cmd[0], cmd[1]); reply != tt.wantReply[i] {
					t.Errorf("Set(%q, %q) = %q, want %q", cmd[0], cmd[1], reply, tt.wantReply[i])
				}
			}
			if got := opts.SendPackArgs(); !slices.Equal(got, tt.wantArgs) {
				t.Errorf("SendPackArgs = %v, want %v", got, tt.wantArgs)
			}
		})
	}
}

func TestUnquoteOptionValue(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		in     string
		want   string
		wantOK bool
	}{
		{name: "unquoted verbatim", in: `refs/heads/main:abc`, want: `refs/heads/main:abc`, wantOK: true},
		{name: "empty unquoted", in: ``, want: ``, wantOK: true},
		{name: "empty quoted", in: `""`, want: ``, wantOK: true},
		{name: "octal UTF-8", in: `"\346\227\245\346\234\254"`, want: "日本", wantOK: true},
		{name: "escaped quote", in: `"a\"b"`, want: `a"b`, wantOK: true},
		{name: "escaped backslash", in: `"a\\b"`, want: `a\b`, wantOK: true},
		{name: "all control escapes", in: `"\a\b\f\n\r\t\v"`, want: "\a\b\f\n\r\t\v", wantOK: true},
		{name: "raw UTF-8 inside quotes", in: `"café"`, want: "café", wantOK: true},
		{name: "trailing bytes ignored like remote-curl", in: `"a"b`, want: `a`, wantOK: true},
		{name: "missing closing quote", in: `"abc`, wantOK: false},
		{name: "dangling backslash", in: `"abc\`, wantOK: false},
		{name: "unknown escape", in: `"\q"`, wantOK: false},
		{name: "octal first digit over 3", in: `"\400"`, wantOK: false},
		{name: "short octal", in: `"\30"`, wantOK: false},
		{name: "non-octal digit", in: `"\308"`, wantOK: false},
		{name: "embedded NUL", in: "\"a\x00b\"", wantOK: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, ok := unquoteOptionValue(tt.in)
			if ok != tt.wantOK {
				t.Fatalf("unquoteOptionValue(%q) ok = %v, want %v", tt.in, ok, tt.wantOK)
			}
			if ok && got != tt.want {
				t.Errorf("unquoteOptionValue(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// Only send-pack signs a push, so a signed push keeps to the send-pack path.
// A push signed "if-asked" and force-if-includes stay on the client-chunked
// path: bbpush checks whether the remote asks for a signature, and git checks
// force-if-includes before it runs the helper.
func TestOptions_BBPushOptions(t *testing.T) {
	t.Parallel()

	o := &Options{}
	for _, kv := range [][2]string{{"dry-run", "true"}, {"atomic", "true"}, {"push-option", "ci.skip"}, {"cas", "refs/heads/main:" + strings.Repeat("a", 40)}, {"force-if-includes", "true"}} {
		o.Set(kv[0], kv[1])
	}
	got, err := o.bbPushOptions()
	if err != nil {
		t.Fatal(err)
	}
	if !got.DryRun || !got.Atomic || !slices.Equal(got.PushOptions, []string{"ci.skip"}) || len(got.CAS) != 1 || got.PushCertIfAsked {
		t.Fatalf("bbPushOptions = %+v", got)
	}

	// transport-helper sends verbosity 0 for git push -q, 1 by default and
	// 2 or more for -v.
	for value, quiet := range map[string]bool{"0": true, "1": false, "2": false} {
		o := &Options{}
		o.Set("verbosity", value)
		got, err := o.bbPushOptions()
		if err != nil || got.Quiet != quiet {
			t.Errorf("verbosity %s: options %+v, err %v; want Quiet %v", value, got, err, quiet)
		}
	}

	for _, name := range []string{"pushcert", "push-cert"} {
		o := &Options{}
		o.Set(name, "true")
		if _, err := o.bbPushOptions(); !errors.Is(err, errBBUnsupportedOption) {
			t.Errorf("%s=true: err = %v, want errBBUnsupportedOption", name, err)
		}
		o = &Options{}
		o.Set(name, "if-asked")
		got, err := o.bbPushOptions()
		if err != nil || !got.PushCertIfAsked {
			t.Errorf("%s=if-asked: options %+v, err %v; want PushCertIfAsked", name, got, err)
		}
	}
}
