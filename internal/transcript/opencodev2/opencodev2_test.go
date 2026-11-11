package opencodev2

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestIsExport(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name string
		data string
		want bool
	}{
		{"1.x", `{"info":{"id":"ses_1"},"messages":[{"info":{"id":"m1","role":"user"},"parts":[]}]}`, false},
		{"2.x", `{"info":{"id":"ses_2"},"messages":[{"id":"m1","type":"user","text":"hi"}]}`, true},
		{"2.x, messages before info", `{"messages":[{"id":"m1","type":"user","text":"hi"}],"info":{"id":"ses_2"}}`, true},
		{"no messages", `{"info":{"id":"ses_1"},"messages":[]}`, false},
		{"no messages key", `{"info":{"id":"ses_1"}}`, false},
		{"a 1.x message carrying type", `{"messages":[{"info":{"id":"m1"},"type":"x","parts":[]}]}`, false},
		{"not an object", `[1,2]`, false},
		{"invalid", `{"messages":[{"id":`, false},
	} {
		if got := IsExport([]byte(c.data)); got != c.want {
			t.Errorf("%s: IsExport = %v, want %v", c.name, got, c.want)
		}
	}
}

// bigV1 is a 1.x export with many messages, as stored transcripts are.
func bigV1(n int) []byte {
	var b strings.Builder
	b.WriteString(`{"info":{"id":"ses_1"},"messages":[`)
	part := strings.Repeat("x", 4096)
	for i := range n {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(`{"info":{"id":"m","role":"assistant"},"parts":[{"type":"text","text":"` + part + `"}]}`)
	}
	b.WriteString(`]}`)
	return []byte(b.String())
}

// BenchmarkIsExport shows the check on a 1.x export reads one message, not
// the file: compare with BenchmarkIsExportFullDecode, the earlier check.
func BenchmarkIsExport(b *testing.B) {
	data := bigV1(3000)
	b.SetBytes(int64(len(data)))
	for b.Loop() {
		if IsExport(data) {
			b.Fatal("1.x read as 2.x")
		}
	}
}

func BenchmarkIsExportFullDecode(b *testing.B) {
	data := bigV1(3000)
	b.SetBytes(int64(len(data)))
	for b.Loop() {
		var doc struct {
			Messages []map[string]json.RawMessage `json:"messages"`
		}
		if json.Unmarshal(data, &doc) != nil {
			b.Fatal("decode")
		}
	}
}
