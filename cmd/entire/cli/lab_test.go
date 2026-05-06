package cli

import (
	"bytes"
	"strings"
	"testing"
)

func TestLabCmd_PrintsLabCommandList(t *testing.T) {
	t.Parallel()

	root := NewRootCmd()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"lab"})

	if err := root.Execute(); err != nil {
		t.Fatalf("entire lab failed: %v", err)
	}
	got := out.String()
	for _, want := range []string{
		"Lab commands",
		"newer Entire workflows",
		"entire review",
		"entire review --help",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("entire lab output missing %q:\n%s", want, got)
		}
	}
}

func TestLabCmd_HelpShowsLabCommandList(t *testing.T) {
	t.Parallel()

	root := NewRootCmd()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"lab", "--help"})

	if err := root.Execute(); err != nil {
		t.Fatalf("entire lab --help failed: %v", err)
	}
	got := out.String()
	for _, want := range []string{"Lab commands", "entire review"} {
		if !strings.Contains(got, want) {
			t.Fatalf("entire lab --help output missing %q:\n%s", want, got)
		}
	}
}

func TestLabCmd_RejectsTopicWithoutRunningIt(t *testing.T) {
	t.Parallel()

	root := NewRootCmd()
	var out, errOut bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errOut)
	root.SetArgs([]string{"lab", "review"})

	err := root.Execute()
	if err == nil {
		t.Fatal("entire lab review should return an error")
	}
	if !strings.Contains(err.Error(), "unknown lab topic") {
		t.Fatalf("error should mention unknown lab topic, got: %v", err)
	}
	if !strings.Contains(errOut.String(), "entire review --help") {
		t.Fatalf("stderr should point to canonical review help, got:\n%s", errOut.String())
	}
	if strings.Contains(out.String(), "Run the review skills configured") {
		t.Fatalf("entire lab review should not run or show review help, got stdout:\n%s", out.String())
	}
}

func TestRootHelp_ShowsLabButHidesReview(t *testing.T) {
	t.Parallel()

	root := NewRootCmd()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"--help"})

	if err := root.Execute(); err != nil {
		t.Fatalf("entire --help failed: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "lab") || !strings.Contains(got, "Explore experimental Entire workflows") {
		t.Fatalf("root help should include lab command, got:\n%s", got)
	}
	if strings.Contains(got, "review") {
		t.Fatalf("root help should not include review while it is in lab, got:\n%s", got)
	}
}

func TestLabRegistryCommandsExistAtCanonicalPaths(t *testing.T) {
	t.Parallel()

	root := NewRootCmd()
	for _, info := range labCommands {
		cmd, _, err := root.Find([]string{info.Name})
		if err != nil {
			t.Fatalf("lab command %q should exist at canonical path: %v", info.Name, err)
		}
		if cmd == nil {
			t.Fatalf("lab command %q resolved to nil command", info.Name)
		}
	}
}
