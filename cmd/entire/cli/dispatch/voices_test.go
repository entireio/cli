package dispatch

import "testing"

func TestResolveVoice_PresetMatch(t *testing.T) {
	t.Parallel()

	got := ResolveVoice(testVoicePresetMarvin)
	if got.Name != testVoicePresetMarvin {
		t.Fatalf("expected name=%s, got %q", testVoicePresetMarvin, got.Name)
	}
	if got.Text == "" {
		t.Fatal("expected non-empty preset text")
	}
}

func TestResolveVoice_CaseInsensitive(t *testing.T) {
	t.Parallel()

	got := ResolveVoice("MARVIN")
	if got.Name != testVoicePresetMarvin {
		t.Fatalf("expected normalized name=%s, got %q", testVoicePresetMarvin, got.Name)
	}
}

func TestResolveVoice_LiteralStringFallback(t *testing.T) {
	t.Parallel()

	got := ResolveVoice("sardonic AI named Gary")
	if got.Text != "sardonic AI named Gary" {
		t.Fatalf("expected passthrough, got %q", got.Text)
	}
	if got.Name != "" {
		t.Fatalf("expected empty name for literal, got %q", got.Name)
	}
}

func TestResolveVoice_FilePathIsTreatedAsLiteral(t *testing.T) {
	t.Parallel()

	got := ResolveVoice("/tmp/my-voice.md")
	if got.Text != "/tmp/my-voice.md" {
		t.Fatalf("expected literal passthrough, got %q", got.Text)
	}
}

func TestResolveVoice_EmptyDefaultsToNeutral(t *testing.T) {
	t.Parallel()

	got := ResolveVoice("")
	if got.Name != testVoicePresetNeutral {
		t.Fatalf("expected neutral default, got %+v", got)
	}
}
