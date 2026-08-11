package cli

import "testing"

func TestHashSearchQuery_NormalizesAndTruncates(t *testing.T) {
	t.Parallel()
	a := hashSearchQuery("  Foo   BAR ")
	b := hashSearchQuery("foo bar")
	if a != b {
		t.Errorf("normalized hashes differ: %q vs %q", a, b)
	}
	if len(a) != 16 {
		t.Errorf("hash length = %d, want 16", len(a))
	}
	if a == hashSearchQuery("foo baz") {
		t.Error("different queries must hash differently")
	}
}

func TestNewSearchID_IsULIDShaped(t *testing.T) {
	t.Parallel()
	id := newSearchID()
	if len(id) != 26 {
		t.Fatalf("search id %q length = %d, want 26", id, len(id))
	}
	if id == newSearchID() {
		t.Error("consecutive search ids must differ")
	}
}
