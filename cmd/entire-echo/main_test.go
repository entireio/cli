package main

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
)

type fakeResult struct {
	out, errOut string
	err         error
}
type fakeRunner struct {
	results map[string]fakeResult
	calls   [][]string
}

func (f *fakeRunner) Run(_ context.Context, name string, args []string, _ string) ([]byte, []byte, error) {
	f.calls = append(f.calls, append([]string{name}, args...))
	r, ok := f.results[strings.Join(append([]string{name}, args...), " ")]
	if !ok {
		return nil, nil, errors.New("unexpected command")
	}
	return []byte(r.out), []byte(r.errOut), r.err
}
func fixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestParseCheckpointMetadataFixture(t *testing.T) {
	t.Parallel()
	meta, err := parseCheckpointMetadata([]byte(fixture(t, "checkpoint.json")))
	if err != nil {
		t.Fatal(err)
	}
	if meta.CheckpointID != "01TEST" {
		t.Fatalf("checkpoint = %q", meta.CheckpointID)
	}
	index, id := latestSession(meta)
	if index != 1 || id != "session-2" {
		t.Fatalf("latest = %d %s", index, id)
	}
}
func TestParseCheckpointMetadataMalformed(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{"{", `{ "checkpoint_id":"" }`, `{ "checkpoint_id":"x", "sessions":[{"index":0},{"index":0,"session_id":"two"}]}`} {
		if _, err := parseCheckpointMetadata([]byte(raw)); err == nil {
			t.Fatalf("wanted error for %s", raw)
		}
	}
}
func TestBuildUnavailableTranscriptAndGraph(t *testing.T) {
	t.Parallel()
	f := &fakeRunner{results: map[string]fakeResult{
		"entire checkpoint explain target --json":                                                          {out: fixture(t, "checkpoint.json")},
		"git rev-parse --show-toplevel":                                                                    {out: "/repo\n"},
		"entire checkpoint explain target --transcript --session-index 1":                                  {err: errors.New("missing"), errOut: "no transcript"},
		"git log --all --format=%H --fixed-strings --grep=Entire-Checkpoint: 01TEST":                       {out: "abc\n"},
		"git diff --find-renames abc^ abc --":                                                              {out: fixture(t, "change.diff")},
		"entire graph impact --repo /repo --symbol Added --file new.go --depth 1 --limit 10 --format json": {err: errors.New("unavailable"), errOut: "graph down"},
	}}
	bundle, err := build(context.Background(), f, "/repo", "target")
	if err != nil {
		t.Fatal(err)
	}
	if len(bundle.MissingOrUncertain) == 0 {
		t.Fatal("missing transcript claim")
	}
	found := false
	for _, w := range bundle.Warnings {
		found = found || strings.Contains(w.Text, "Graph evidence unavailable")
	}
	if !found {
		t.Fatal("missing graph unavailable warning")
	}
	if err := validateBundle(bundle); err != nil {
		t.Fatal(err)
	}
}
func TestBuildPartialGraphAndDeterministicOutput(t *testing.T) {
	t.Parallel()
	results := map[string]fakeResult{
		"entire checkpoint explain target --json": {out: fixture(t, "checkpoint.json")}, "git rev-parse --show-toplevel": {out: "/repo\n"}, "entire checkpoint explain target --transcript --session-index 1": {out: fixture(t, "transcript.jsonl")}, "git log --all --format=%H --fixed-strings --grep=Entire-Checkpoint: 01TEST": {out: "abc\n"}, "git diff --find-renames abc^ abc --": {out: fixture(t, "change.diff")}, "entire graph impact --repo /repo --symbol Added --file new.go --depth 1 --limit 10 --format json": {out: `{"warning":"partial result","callers":[]}`},
	}
	one, err := build(context.Background(), &fakeRunner{results: results}, "/repo", "target")
	if err != nil {
		t.Fatal(err)
	}
	two, err := build(context.Background(), &fakeRunner{results: results}, "/repo", "target")
	if err != nil {
		t.Fatal(err)
	}
	if renderText(one) != renderText(two) {
		t.Fatal("renderer is not deterministic")
	}
	if len(one.PotentiallyAffected) != 1 || one.PotentiallyAffected[0].Confidence != "question" {
		t.Fatalf("partial graph = %#v", one.PotentiallyAffected)
	}
	if err := validateBundle(one); err != nil {
		t.Fatal(err)
	}
}
func TestValidateBundleRejectsUnlinkedClaim(t *testing.T) {
	t.Parallel()
	err := validateBundle(ReviewBundle{Overview: Claim{Text: "fact"}, Evidence: []Evidence{{ID: "E001"}}})
	if err == nil {
		t.Fatal("expected unlinked claim error")
	}
}
