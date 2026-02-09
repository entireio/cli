package cli

import (
	"slices"
	"strings"
	"testing"
)

func TestFindStanza_Present(t *testing.T) {
	content := `# some config
export FOO=bar

# BEGIN my-tool (v2)
source <(my-tool completion bash)
# END my-tool

export BAZ=qux
`
	version, body, found := FindStanza(content, "my-tool")
	if !found {
		t.Fatal("expected stanza to be found")
	}
	if version != 2 {
		t.Errorf("version = %d, want 2", version)
	}
	if body != "source <(my-tool completion bash)" {
		t.Errorf("body = %q, want %q", body, "source <(my-tool completion bash)")
	}
}

func TestFindStanza_Absent(t *testing.T) {
	content := `export FOO=bar` + "\n"
	version, body, found := FindStanza(content, "my-tool")
	if found {
		t.Fatal("expected stanza to not be found")
	}
	if version != -1 {
		t.Errorf("version = %d, want -1", version)
	}
	if body != "" {
		t.Errorf("body = %q, want empty", body)
	}
}

func TestFindStanza_DifferentName(t *testing.T) {
	content := `# BEGIN other-tool (v1)
something
# END other-tool
`
	_, _, found := FindStanza(content, "my-tool")
	if found {
		t.Fatal("should not find stanza with different name")
	}
}

func TestFindStanza_MultilineBody(t *testing.T) {
	content := `# BEGIN my-tool (v1)
line1
line2
line3
# END my-tool
`
	version, body, found := FindStanza(content, "my-tool")
	if !found {
		t.Fatal("expected stanza to be found")
	}
	if version != 1 {
		t.Errorf("version = %d, want 1", version)
	}
	if body != "line1\nline2\nline3" {
		t.Errorf("body = %q, want %q", body, "line1\nline2\nline3")
	}
}

func TestUpsertStanza_EmptyContent(t *testing.T) {
	result := UpsertStanza("", "my-tool", 1, "source <(my-tool completion bash)")
	want := "# BEGIN my-tool (v1)\nsource <(my-tool completion bash)\n# END my-tool\n"
	if result != want {
		t.Errorf("result = %q, want %q", result, want)
	}
}

func TestUpsertStanza_AppendToExistingContent(t *testing.T) {
	content := "export FOO=bar\n"
	result := UpsertStanza(content, "my-tool", 1, "source <(my-tool completion bash)")
	want := "export FOO=bar\n\n# BEGIN my-tool (v1)\nsource <(my-tool completion bash)\n# END my-tool\n"
	if result != want {
		t.Errorf("result = %q, want %q", result, want)
	}
}

func TestUpsertStanza_ReplaceExisting(t *testing.T) {
	content := `export FOO=bar

# BEGIN my-tool (v1)
old-command
# END my-tool

export BAZ=qux
`
	result := UpsertStanza(content, "my-tool", 2, "new-command")
	want := `export FOO=bar

# BEGIN my-tool (v2)
new-command
# END my-tool

export BAZ=qux
`
	if result != want {
		t.Errorf("result = %q, want %q", result, want)
	}
}

func TestUpsertStanza_PreservesSurroundingContent(t *testing.T) {
	content := `# header
export A=1

# BEGIN my-tool (v1)
old-line
# END my-tool

# footer
export B=2
`
	result := UpsertStanza(content, "my-tool", 2, "new-line")

	// Check surrounding content is preserved
	if !containsLine(result, "# header") {
		t.Error("header should be preserved")
	}
	if !containsLine(result, "export A=1") {
		t.Error("content before stanza should be preserved")
	}
	if !containsLine(result, "# footer") {
		t.Error("footer should be preserved")
	}
	if !containsLine(result, "export B=2") {
		t.Error("content after stanza should be preserved")
	}
}

func TestUpsertStanza_NoTrailingNewline(t *testing.T) {
	content := "export FOO=bar"
	result := UpsertStanza(content, "my-tool", 1, "cmd")
	want := "export FOO=bar\n\n# BEGIN my-tool (v1)\ncmd\n# END my-tool\n"
	if result != want {
		t.Errorf("result = %q, want %q", result, want)
	}
}

func TestUpsertStanza_MultipleStanzas(t *testing.T) {
	content := `# BEGIN tool-a (v1)
cmd-a
# END tool-a
`
	result := UpsertStanza(content, "tool-b", 1, "cmd-b")
	// tool-a should remain, tool-b appended
	_, _, foundA := FindStanza(result, "tool-a")
	if !foundA {
		t.Error("tool-a stanza should still exist")
	}
	_, _, foundB := FindStanza(result, "tool-b")
	if !foundB {
		t.Error("tool-b stanza should be appended")
	}
}

func TestRemoveStanza_Present(t *testing.T) {
	content := `export FOO=bar

# BEGIN my-tool (v1)
source <(my-tool completion bash)
# END my-tool

export BAZ=qux
`
	result := RemoveStanza(content, "my-tool")
	if containsLine(result, "# BEGIN my-tool") {
		t.Error("stanza BEGIN marker should be removed")
	}
	if containsLine(result, "# END my-tool") {
		t.Error("stanza END marker should be removed")
	}
	if containsLine(result, "my-tool completion") {
		t.Error("stanza body should be removed")
	}
	if !containsLine(result, "export FOO=bar") {
		t.Error("content before stanza should be preserved")
	}
	if !containsLine(result, "export BAZ=qux") {
		t.Error("content after stanza should be preserved")
	}
}

func TestRemoveStanza_Absent(t *testing.T) {
	content := "export FOO=bar\n"
	result := RemoveStanza(content, "my-tool")
	if result != content {
		t.Errorf("content should be unchanged, got %q", result)
	}
}

func TestRemoveStanza_AtBeginning(t *testing.T) {
	content := `# BEGIN my-tool (v1)
cmd
# END my-tool

export FOO=bar
`
	result := RemoveStanza(content, "my-tool")
	if containsLine(result, "# BEGIN my-tool") {
		t.Error("stanza should be removed")
	}
	if !containsLine(result, "export FOO=bar") {
		t.Error("remaining content should be preserved")
	}
}

func TestRemoveStanza_AtEnd(t *testing.T) {
	content := `export FOO=bar

# BEGIN my-tool (v1)
cmd
# END my-tool
`
	result := RemoveStanza(content, "my-tool")
	if containsLine(result, "# BEGIN my-tool") {
		t.Error("stanza should be removed")
	}
	if !containsLine(result, "export FOO=bar") {
		t.Error("remaining content should be preserved")
	}
}

func TestRemoveStanza_OnlyStanza(t *testing.T) {
	content := `# BEGIN my-tool (v1)
cmd
# END my-tool
`
	result := RemoveStanza(content, "my-tool")
	// Should result in empty or whitespace-only content
	if containsLine(result, "# BEGIN my-tool") {
		t.Error("stanza should be removed")
	}
}

func TestRemoveStanza_PreservesOtherStanzas(t *testing.T) {
	content := `# BEGIN tool-a (v1)
cmd-a
# END tool-a

# BEGIN tool-b (v1)
cmd-b
# END tool-b
`
	result := RemoveStanza(content, "tool-a")
	_, _, foundA := FindStanza(result, "tool-a")
	if foundA {
		t.Error("tool-a should be removed")
	}
	_, _, foundB := FindStanza(result, "tool-b")
	if !foundB {
		t.Error("tool-b should be preserved")
	}
}

func containsLine(content, line string) bool {
	return slices.Contains(strings.Split(content, "\n"), line)
}
