package redact

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var benchmarkOpenSSHPrivateKey = makeFakeOpenSSHPrivateKey(`b3BlbnNzaC1rZXktdjEAAAAABG5vbmUAAAAEbm9uZQAAAAAAAAABAAAAMwAAAAtzc2gtZW
QyNTUxOQAAACB7ZlJ8tkWCKdRJRGF1BngP3bkNbz8bMF6Yl5xLJp9m1QAAAJj2M3UO9jN1
DgAAAAtzc2gtZWQyNTUxOQAAACB7ZlJ8tkWCKdRJRGF1BngP3bkNbz8bMF6Yl5xLJp9m1QA
AAEAGZmFrZS1rZXktZm9yLXJlZGFjdGlvbi1iZW5jaG1hcmstb25seQECAwQF`)

// BenchmarkRedactJSONLBytes gives us a stable redaction performance baseline.
//
// To compare against a base ref:
//
//	BENCH_PKG=./redact BENCH_PATTERN='BenchmarkRedactJSONLBytes' BASE_REF=main mise run bench:compare
func BenchmarkRedactJSONLBytes(b *testing.B) {
	cases := []struct {
		name string
		data []byte
	}{
		{
			name: "Fixture/ClaudeFull2",
			data: readBenchmarkFixture(b, "../cmd/entire/cli/transcript/compact/testdata/claude_full2.jsonl"),
		},
		{
			name: "Synthetic/CheckpointLog",
			data: generateBenchmarkJSONL(b, 2500),
		},
	}

	for _, tc := range cases {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(int64(len(tc.data)))
			for b.Loop() {
				redacted, err := JSONLBytes(tc.data)
				if err != nil {
					b.Fatalf("redact JSONL: %v", err)
				}
				if redacted.Len() == 0 {
					b.Fatal("redacted output was empty")
				}
			}
		})
	}
}

func readBenchmarkFixture(b *testing.B, path string) []byte {
	b.Helper()
	data, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		b.Fatalf("read benchmark fixture %s: %v", path, err)
	}
	return data
}

func generateBenchmarkJSONL(b *testing.B, lines int) []byte {
	b.Helper()

	var out strings.Builder
	for i := range lines {
		content := benchmarkLineContent(i)
		entry := map[string]any{
			"type":       "text",
			"session_id": fmt.Sprintf("bench-session-%06d", i),
			"message": map[string]any{
				"role":    roleForBenchmarkLine(i),
				"content": content,
			},
			"metadata": map[string]any{
				"cwd":       "/tmp/entire-redact-benchmark/repo",
				"tool_id":   fmt.Sprintf("toolu_%06d", i),
				"file_path": fmt.Sprintf("src/generated/file_%04d.go", i%200),
			},
		}
		encoded, err := json.Marshal(entry)
		if err != nil {
			b.Fatalf("marshal benchmark line: %v", err)
		}
		out.Write(encoded)
		out.WriteByte('\n')
	}
	return []byte(out.String())
}

func benchmarkLineContent(i int) string {
	switch {
	case i%997 == 0:
		return "Configure DATABASE_URL=postgres://app:pwd123@db.example.com:5432/app and keep going."
	case i%991 == 0:
		return "SSH key material follows:\n" + benchmarkOpenSSHPrivateKey
	case i%137 == 0:
		return "Use api_key=" + highEntropySecret + " when testing redaction throughput."
	default:
		return fmt.Sprintf(
			"Line %04d: agent inspected files, summarized diffs, and wrote a long but ordinary transcript chunk %s.",
			i,
			strings.Repeat("with repeated neutral prose ", 12),
		)
	}
}

func roleForBenchmarkLine(i int) string {
	if i%3 == 0 {
		return "user"
	}
	return "assistant"
}
