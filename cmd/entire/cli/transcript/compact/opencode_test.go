package compact

import (
	"testing"
)

func TestCompact_OpenCodeFixture(t *testing.T) {
	t.Parallel()
	assertFixtureTransform(t, agentOpts("opencode"), "testdata/opencode_full.jsonl", "testdata/opencode_expected.jsonl")
}

func TestCompact_OpenCodeTokenUsage(t *testing.T) {
	t.Parallel()

	input := []byte(`{
		"info":{"id":"ses-1","title":"test","version":"1.0"},
		"messages":[
			{
				"info":{"id":"msg-u1","role":"user","time":{"created":1700000000000}},
				"parts":[{"type":"text","text":"hello"}]
			},
			{
				"info":{"id":"msg-a1","role":"assistant","time":{"created":1700000001000,"completed":1700000002000},
					"tokens":{"total":500,"input":150,"output":90,"reasoning":0,"cache":{"read":100,"write":160}}},
				"parts":[{"type":"text","text":"Hi there!"}]
			}
		]
	}`)

	expected := []string{
		`{"v":1,"agent":"opencode","cli_version":"0.5.1","type":"user","ts":"2023-11-14T22:13:20Z","content":[{"text":"hello"}]}`,
		`{"v":1,"agent":"opencode","cli_version":"0.5.1","type":"assistant","ts":"2023-11-14T22:13:21Z","id":"msg-a1","input_tokens":150,"output_tokens":90,"content":[{"type":"text","text":"Hi there!"}]}`,
	}

	result, err := Compact(input, agentOpts("opencode"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	assertJSONLines(t, result, expected)
}

func TestCompact_OpenCodeStartLine(t *testing.T) {
	t.Parallel()

	input := []byte(`{
		"info":{"id":"ses-1","title":"test","version":"1.0"},
		"messages":[
			{
				"info":{"id":"msg-u1","role":"user","time":{"created":1700000000000}},
				"parts":[{"type":"text","text":"hello"}]
			},
			{
				"info":{"id":"msg-a1","role":"assistant","time":{"created":1700000001000}},
				"parts":[{"type":"text","text":"Hi there!"}]
			},
			{
				"info":{"id":"msg-u2","role":"user","time":{"created":1700000002000}},
				"parts":[{"type":"text","text":"bye"}]
			}
		]
	}`)

	t.Run("skip first message", func(t *testing.T) {
		t.Parallel()
		opts := MetadataFields{Agent: "opencode", CLIVersion: "0.5.1", StartLine: 1}
		expected := []string{
			`{"v":1,"agent":"opencode","cli_version":"0.5.1","type":"assistant","ts":"2023-11-14T22:13:21Z","id":"msg-a1","content":[{"type":"text","text":"Hi there!"}]}`,
			`{"v":1,"agent":"opencode","cli_version":"0.5.1","type":"user","ts":"2023-11-14T22:13:22Z","content":[{"text":"bye"}]}`,
		}
		result, err := Compact(input, opts)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		assertJSONLines(t, result, expected)
	})

	t.Run("skip all messages", func(t *testing.T) {
		t.Parallel()
		opts := MetadataFields{Agent: "opencode", CLIVersion: "0.5.1", StartLine: 100}
		result, err := Compact(input, opts)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		assertJSONLines(t, result, nil)
	})
}

func TestCompact_OpenCodeNoTokensOmitsFields(t *testing.T) {
	t.Parallel()

	input := []byte(`{
		"info":{"id":"ses-1","title":"test","version":"1.0"},
		"messages":[
			{
				"info":{"id":"msg-a1","role":"assistant","time":{"created":1700000001000}},
				"parts":[{"type":"text","text":"no tokens here"}]
			}
		]
	}`)

	expected := []string{
		`{"v":1,"agent":"opencode","cli_version":"0.5.1","type":"assistant","ts":"2023-11-14T22:13:21Z","id":"msg-a1","content":[{"type":"text","text":"no tokens here"}]}`,
	}

	result, err := Compact(input, agentOpts("opencode"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	assertJSONLines(t, result, expected)
}
