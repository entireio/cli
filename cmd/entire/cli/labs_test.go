package cli

import (
	"bytes"
	"strings"
	"testing"
)

func TestLabsCmd_PrintsExperimentalCommandList(t *testing.T) {
	t.Parallel()

	root := NewRootCmd()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"labs"})

	if err := root.Execute(); err != nil {
		t.Fatalf("entire labs failed: %v", err)
	}
	got := out.String()
	for _, want := range []string{
		"Labs",
		"newer Entire workflows",
		"Available experimental commands",
		"entire review",
		"entire review --help",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("entire labs output missing %q:\n%s", want, got)
		}
	}
}

func TestLabsCmd_HelpShowsExperimentalCommandList(t *testing.T) {
	t.Parallel()

	root := NewRootCmd()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"labs", "--help"})

	if err := root.Execute(); err != nil {
		t.Fatalf("entire labs --help failed: %v", err)
	}
	got := out.String()
	for _, want := range []string{"Labs", "entire review"} {
		if !strings.Contains(got, want) {
			t.Fatalf("entire labs --help output missing %q:\n%s", want, got)
		}
	}
}

func TestLabsCmd_RejectsTopicWithoutRunningIt(t *testing.T) {
	t.Parallel()

	root := NewRootCmd()
	var out, errOut bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errOut)
	root.SetArgs([]string{"labs", "review"})

	err := root.Execute()
	if err == nil {
		t.Fatal("entire labs review should return an error")
	}
	if !strings.Contains(err.Error(), "unknown labs topic") {
		t.Fatalf("error should mention unknown labs topic, got: %v", err)
	}
	if !strings.Contains(errOut.String(), "entire review --help") {
		t.Fatalf("stderr should point to canonical review help, got:\n%s", errOut.String())
	}
	if strings.Contains(out.String(), "Run the review skills configured") {
		t.Fatalf("entire labs review should not run or show review help, got stdout:\n%s", out.String())
	}
}

func TestRootHelp_ShowsLabsButHidesReview(t *testing.T) {
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
	if !strings.Contains(got, "labs") || !strings.Contains(got, "Explore experimental Entire workflows") {
		t.Fatalf("root help should include labs command, got:\n%s", got)
	}
	if strings.Contains(got, "review") {
		t.Fatalf("root help should not include review while it is listed in labs, got:\n%s", got)
	}
}

func TestLabsRegistryCommandsExistAtCanonicalPaths(t *testing.T) {
	t.Parallel()

	root := NewRootCmd()
	for _, info := range experimentalCommands {
		cmd, _, err := root.Find([]string{info.Name})
		if err != nil {
			t.Fatalf("labs command %q should exist at canonical path: %v", info.Name, err)
		}
		if cmd == nil {
			t.Fatalf("labs command %q resolved to nil command", info.Name)
		}
	}
}
