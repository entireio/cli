package pi

import "testing"

func TestParsePiModelList(t *testing.T) {
	raw := "provider      model                       context  max-out  thinking  images\n" +
		"anthropic     claude-opus-4-0             200K     32K      yes       yes   \n" +
		"openai        gpt-5                       400K     128K     yes       no    \n" +
		"\n" +
		"google        gemini-2.5-pro              1M       64K      yes       yes   \n"

	got := parsePiModelList(raw)
	if len(got) != 3 {
		t.Fatalf("parsed %d models, want 3: %#v", len(got), got)
	}
	want := []struct{ id, note string }{
		{"anthropic/claude-opus-4-0", "200K ctx"},
		{"openai/gpt-5", "400K ctx"},
		{"google/gemini-2.5-pro", "1M ctx"},
	}
	for i, w := range want {
		if got[i].ID != w.id {
			t.Errorf("model[%d].ID = %q, want %q", i, got[i].ID, w.id)
		}
		if got[i].Note != w.note {
			t.Errorf("model[%d].Note = %q, want %q", i, got[i].Note, w.note)
		}
	}
}

func TestParsePiModelList_HeaderAndBlanksSkipped(t *testing.T) {
	if got := parsePiModelList("provider model\n\n   \n"); len(got) != 0 {
		t.Fatalf("expected no models, got %#v", got)
	}
}
