package plugins

import (
	"strings"
	"testing"
)

func TestParseManifest_Valid(t *testing.T) {
	t.Parallel()
	data := []byte(`{
		"name": "checkpoint-notify",
		"version": "1.0.0",
		"description": "notifies on checkpoints",
		"hooks": ["checkpoint_saved", "post_commit"],
		"commands": [{"name": "notify", "short": "send a notification"}],
		"capabilities": ["http"]
	}`)
	m, err := ParseManifest(data)
	if err != nil {
		t.Fatalf("ParseManifest() error = %v", err)
	}
	if m.Name != "checkpoint-notify" {
		t.Errorf("Name = %q, want checkpoint-notify", m.Name)
	}
	if m.EntryFile() != DefaultEntry {
		t.Errorf("EntryFile() = %q, want %q", m.EntryFile(), DefaultEntry)
	}
	if len(m.Hooks) != 2 || len(m.Commands) != 1 {
		t.Errorf("unexpected hooks/commands: %+v", m)
	}
}

func TestParseManifest_EntryOverride(t *testing.T) {
	t.Parallel()
	m, err := ParseManifest([]byte(`{"name":"p","entry":"init.lua"}`))
	if err != nil {
		t.Fatalf("ParseManifest() error = %v", err)
	}
	if m.EntryFile() != "init.lua" {
		t.Errorf("EntryFile() = %q, want init.lua", m.EntryFile())
	}
}

func TestParseManifest_RejectsUnknownField(t *testing.T) {
	t.Parallel()
	_, err := ParseManifest([]byte(`{"name":"p","bogus":true}`))
	if err == nil {
		t.Fatal("expected error for unknown field")
	}
}

func TestParseManifest_RejectsUnknownHook(t *testing.T) {
	t.Parallel()
	_, err := ParseManifest([]byte(`{"name":"p","hooks":["not_a_hook"]}`))
	if err == nil || !strings.Contains(err.Error(), "unknown hook") {
		t.Fatalf("expected unknown hook error, got %v", err)
	}
}

func TestParseManifest_RejectsUnknownCapability(t *testing.T) {
	t.Parallel()
	_, err := ParseManifest([]byte(`{"name":"p","capabilities":["gpu"]}`))
	if err == nil || !strings.Contains(err.Error(), "unknown capability") {
		t.Fatalf("expected unknown capability error, got %v", err)
	}
}

func TestParseManifest_RejectsBadName(t *testing.T) {
	t.Parallel()
	cases := []string{
		`{"name":""}`,
		`{"name":"-flag"}`,
		`{"name":"agent-foo"}`,
		`{"name":"a/b"}`,
		`{"name":".."}`,
	}
	for _, c := range cases {
		if _, err := ParseManifest([]byte(c)); err == nil {
			t.Errorf("expected error for manifest %s", c)
		}
	}
}

func TestParseManifest_RejectsEntryWithPath(t *testing.T) {
	t.Parallel()
	_, err := ParseManifest([]byte(`{"name":"p","entry":"sub/main.lua"}`))
	if err == nil || !strings.Contains(err.Error(), "bare file name") {
		t.Fatalf("expected entry path rejection, got %v", err)
	}
}

func TestParseManifest_RejectsDuplicateCommand(t *testing.T) {
	t.Parallel()
	_, err := ParseManifest([]byte(`{"name":"p","commands":[{"name":"x"},{"name":"x"}]}`))
	if err == nil || !strings.Contains(err.Error(), "duplicate command") {
		t.Fatalf("expected duplicate command error, got %v", err)
	}
}
