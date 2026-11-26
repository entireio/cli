package transcript

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

// A named format must convert exactly as detection does for that format, so
// an importer naming it gets the same output as the built-in agent.
func TestConvert_FormatMatchesDetection(t *testing.T) {
	t.Parallel()

	fixtures := map[string]string{
		FormatClaudeCode: "testdata/claude_full.jsonl",
		FormatCodex:      "testdata/codex_full.jsonl",
		FormatCopilot:    "testdata/copilot_full.jsonl",
		FormatDroid:      "testdata/droid_full.jsonl",
		FormatGemini:     "testdata/gemini_full.jsonl",
		FormatOpenCode:   "testdata/opencode_full.jsonl",
		FormatPi:         "testdata/pi_full.jsonl",
	}
	for format, path := range fixtures {
		t.Run(format, func(t *testing.T) {
			t.Parallel()
			native, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			for _, startLine := range []int{0, 3} {
				opts := agentOpts(format)
				opts.StartLine = startLine
				detected, err := Convert(native, opts)
				if err != nil {
					t.Fatalf("detected: %v", err)
				}
				opts.Format = format
				declared, err := Convert(native, opts)
				if err != nil {
					t.Fatalf("declared: %v", err)
				}
				if !bytes.Equal(declared, detected) {
					t.Errorf("StartLine %d: declared format output differs from detected\ndeclared:\n%s\ndetected:\n%s", startLine, declared, detected)
				}
			}
		})
	}
}

// Detection picks Claude Code's per-call accounting only when Agent is
// "claude-code"; a declared format picks it for any agent name.
func TestConvert_FormatClaudeCodeForAnotherAgent(t *testing.T) {
	t.Parallel()

	native, err := os.ReadFile("testdata/claude_full.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	want, err := Convert(native, agentOpts(agentClaudeCode))
	if err != nil {
		t.Fatal(err)
	}
	opts := agentOpts("my-agent")
	opts.Format = FormatClaudeCode
	got, err := Convert(native, opts)
	if err != nil {
		t.Fatal(err)
	}
	want = []byte(strings.ReplaceAll(string(want), `"agent":"claude-code"`, `"agent":"my-agent"`))
	if !bytes.Equal(got, want) {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

// A named single-object format converts an empty transcript, one not yet
// written, to nothing as detection does, instead of failing to parse it.
func TestConvert_FormatEmptyContent(t *testing.T) {
	t.Parallel()

	for _, format := range []string{FormatOpenCode, FormatGemini} {
		for _, content := range []string{"", "\n  \n"} {
			opts := agentOpts("x")
			want, err := Convert([]byte(content), opts)
			if err != nil {
				t.Fatalf("detected %q: %v", content, err)
			}
			opts.Format = format
			got, err := Convert([]byte(content), opts)
			if err != nil {
				t.Fatalf("%s on %q: %v", format, content, err)
			}
			if !bytes.Equal(got, want) {
				t.Errorf("%s on %q = %q, want %q", format, content, got, want)
			}
		}
	}
}

func TestConvert_UnknownFormat(t *testing.T) {
	t.Parallel()

	opts := agentOpts("my-agent")
	opts.Format = "not-a-format"
	if _, err := Convert([]byte(`{"type":"user"}`+"\n"), opts); err == nil {
		t.Fatal("Convert with an unknown Format: want error")
	}
	if KnownFormat("not-a-format") || KnownFormat("") {
		t.Error("KnownFormat accepted an unknown or empty format")
	}
}

func TestConvert_FormatPiWithLeadingTitleSlot(t *testing.T) {
	t.Parallel()

	pi, err := os.ReadFile("testdata/pi_full.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	withTitle := append([]byte(`{"type":"title","v":1,"title":"t"}`+"\n"), pi...)

	want, err := Convert(pi, agentOpts("omp"))
	if err != nil {
		t.Fatal(err)
	}
	detected, err := Convert(withTitle, agentOpts("omp"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(detected, want) {
		t.Fatal("detection converted the title-slot session as Pi; this test no longer shows the need to declare")
	}
	opts := agentOpts("omp")
	opts.Format = FormatPi
	got, err := Convert(withTitle, opts)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("declared Pi:\ngot:\n%s\nwant:\n%s", got, want)
	}
}
