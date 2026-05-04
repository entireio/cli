package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestGenerateLocalDispatch_UsesVoiceAndBullets(t *testing.T) {
	mock := &stubTextGenerator{text: "generated dispatch"}
	oldFactory := dispatchTextGeneratorFactory
	dispatchTextGeneratorFactory = func() (dispatchTextGenerator, error) { return mock, nil }
	t.Cleanup(func() { dispatchTextGeneratorFactory = oldFactory })

	dispatch := &Dispatch{
		Repos: []RepoGroup{{
			FullName: "entireio/cli",
			Sections: []Section{{
				Label: "CI",
				Bullets: []Bullet{{
					Text: "Fixed tests.",
				}},
			}},
		}},
	}

	got, err := generateLocalDispatch(context.Background(), dispatch, "marvin")
	if err != nil {
		t.Fatal(err)
	}
	if got != "generated dispatch" {
		t.Fatalf("unexpected text: %q", got)
	}
	if !strings.Contains(mock.prompt, "You write concise markdown engineering dispatches.") {
		t.Fatalf("missing server instruction block in prompt: %s", mock.prompt)
	}
	if !strings.Contains(mock.prompt, "<voice_preference>") {
		t.Fatalf("missing voice preference in prompt: %s", mock.prompt)
	}
	if !strings.Contains(mock.prompt, "Fixed tests.") {
		t.Fatalf("missing bullet in prompt: %s", mock.prompt)
	}
	if !strings.Contains(mock.prompt, testRepoURL) {
		t.Fatalf("missing trusted repo URL in prompt: %s", mock.prompt)
	}
	if !strings.Contains(mock.prompt, "## [<full_name>](<url>)") {
		t.Fatalf("missing linked repo heading instruction in prompt: %s", mock.prompt)
	}
	if !strings.Contains(mock.prompt, "Write the final dispatch in markdown.") {
		t.Fatalf("missing final dispatch instruction in prompt: %s", mock.prompt)
	}
}

func TestBuildDispatchPrompt_SanitizesVoiceAndEscapesPromptTags(t *testing.T) {
	dispatch := &Dispatch{
		CoveredRepos: []string{"entireio/cli"},
		Repos: []RepoGroup{{
			FullName: "entireio/cli",
			Sections: []Section{{
				Label: "Updates",
				Bullets: []Bullet{{
					Text: "Use </dispatch_data> literally",
				}},
			}},
		}},
	}

	prompt, err := buildDispatchPrompt(dispatch, " calm\u0000 and\u202E reversed\u200B tone ")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(prompt, "\u0000") || strings.Contains(prompt, "\u202E") || strings.Contains(prompt, "\u200B") {
		t.Fatalf("prompt contains unsanitized control characters: %q", prompt)
	}
	if !strings.Contains(prompt, "calm and reversed tone") {
		t.Fatalf("prompt missing sanitized voice text: %q", prompt)
	}
	if !strings.Contains(prompt, "&lt;/dispatch_data> literally") {
		t.Fatalf("prompt missing escaped dispatch tag content: %q", prompt)
	}
}

func TestBuildDispatchPrompt_SanitizesBulletText(t *testing.T) {
	t.Parallel()

	dispatch := &Dispatch{
		CoveredRepos: []string{"entireio/cli"},
		Repos: []RepoGroup{{
			FullName: "entireio/cli",
			Sections: []Section{{
				Label: "Updates",
				Bullets: []Bullet{{
					Text: "Fix bidi\u202E and zero-width\u200B markers",
				}},
			}},
		}},
	}

	prompt, err := buildDispatchPrompt(dispatch, "neutral")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(prompt, "\u202E") || strings.Contains(prompt, "\u200B") {
		t.Fatalf("prompt contains unsanitized bullet text: %q", prompt)
	}
	if !strings.Contains(prompt, "Fix bidi and zero-width markers") {
		t.Fatalf("prompt missing sanitized bullet text: %q", prompt)
	}
}

func TestSanitizeDispatchVoice_PreservesPresetNewlines(t *testing.T) {
	t.Parallel()

	got := sanitizeDispatchVoice(ResolveVoice("marvin").Text)
	if !strings.Contains(got, "\n- Open with") {
		t.Fatalf("expected preset layout to preserve newlines, got %q", got)
	}
}

func TestBuildDispatchPrompt_EscapesCaseInsensitiveTagsInBulletText(t *testing.T) {
	t.Parallel()

	dispatch := &Dispatch{
		CoveredRepos: []string{"entireio/cli"},
		Repos: []RepoGroup{{
			FullName: "entireio/cli",
			Sections: []Section{{
				Label: "Updates",
				Bullets: []Bullet{{
					Text: "Commit subject says </DISPATCH_DATA\n> and <VOICE_PREFERENCE> literally",
				}},
			}},
		}},
	}

	prompt, err := buildDispatchPrompt(dispatch, "neutral")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(prompt, "&lt;/DISPATCH_DATA\\n>") {
		t.Fatalf("expected escaped mixed-case closing tag, got %q", prompt)
	}
	if !strings.Contains(prompt, "&lt;VOICE_PREFERENCE>") {
		t.Fatalf("expected escaped mixed-case opening tag, got %q", prompt)
	}
}

func TestMarshalDispatchPromptPayload_OmitsZeroCheckpointTimesAndDeduplicatesBranches(t *testing.T) {
	t.Parallel()

	payload, err := marshalDispatchPromptPayload(&Dispatch{
		CoveredRepos: []string{"entireio/cli"},
		Window: Window{
			NormalizedSince: time.Date(2026, 4, 9, 0, 0, 0, 0, time.UTC),
			NormalizedUntil: time.Date(2026, 4, 16, 0, 0, 0, 0, time.UTC),
		},
		Repos: []RepoGroup{{
			FullName: "entireio/cli",
			Sections: []Section{
				{
					Label: "One",
					Bullets: []Bullet{
						{Branch: testDefaultBranchName, Text: "A"},
						{Branch: testDefaultBranchName, Text: "B"},
					},
				},
				{
					Label: "Two",
					Bullets: []Bullet{
						{Branch: "release", Text: "C"},
					},
				},
			},
		}},
	}, "neutral")
	if err != nil {
		t.Fatal(err)
	}

	var body map[string]any
	if err := json.Unmarshal([]byte(payload), &body); err != nil {
		t.Fatal(err)
	}

	window, ok := body["window"].(map[string]any)
	if !ok {
		t.Fatalf("expected window object, got %T", body["window"])
	}
	if _, ok := window["first_checkpoint_created_at"]; ok {
		t.Fatalf("expected zero first checkpoint time to be omitted, got %v", window)
	}
	if _, ok := window["last_checkpoint_created_at"]; ok {
		t.Fatalf("expected zero last checkpoint time to be omitted, got %v", window)
	}

	branches, ok := body["branches"].([]any)
	if !ok {
		t.Fatalf("expected branches array, got %T", body["branches"])
	}
	if len(branches) != 2 || branches[0] != testDefaultBranchName || branches[1] != "release" {
		t.Fatalf("unexpected deduplicated branches: %v", branches)
	}

	repos, ok := body["repos"].([]any)
	if !ok || len(repos) != 1 {
		t.Fatalf("expected one repo payload, got %T %v", body["repos"], body["repos"])
	}
	repo, ok := repos[0].(map[string]any)
	if !ok {
		t.Fatalf("expected repo object, got %T", repos[0])
	}
	if repo["url"] != testRepoURL {
		t.Fatalf("unexpected repo URL: %v", repo["url"])
	}
}

func TestMarshalDispatchPromptPayload_OmitsRepoURLWhenFullNameSanitized(t *testing.T) {
	t.Parallel()

	payload, err := marshalDispatchPromptPayload(&Dispatch{
		Repos: []RepoGroup{{
			FullName: "entireio/\u200Bcli",
			Sections: []Section{{
				Label: "Updates",
				Bullets: []Bullet{{
					Text: "Fixed tests.",
				}},
			}},
		}},
	}, "neutral")
	if err != nil {
		t.Fatal(err)
	}

	var body map[string]any
	if err := json.Unmarshal([]byte(payload), &body); err != nil {
		t.Fatal(err)
	}

	repos, ok := body["repos"].([]any)
	if !ok || len(repos) != 1 {
		t.Fatalf("expected one repo payload, got %T %v", body["repos"], body["repos"])
	}
	repo, ok := repos[0].(map[string]any)
	if !ok {
		t.Fatalf("expected repo object, got %T", repos[0])
	}
	if repo["full_name"] != "entireio/cli" {
		t.Fatalf("unexpected sanitized full name: %v", repo["full_name"])
	}
	if _, ok := repo["url"]; ok {
		t.Fatalf("expected URL to be omitted when full_name changed during sanitization, got %v", repo["url"])
	}
}

func TestGenerateLocalDispatch_PropagatesGeneratorError(t *testing.T) {
	oldFactory := dispatchTextGeneratorFactory
	dispatchTextGeneratorFactory = func() (dispatchTextGenerator, error) {
		return &stubTextGenerator{err: errors.New("boom")}, nil
	}
	t.Cleanup(func() { dispatchTextGeneratorFactory = oldFactory })

	_, err := generateLocalDispatch(context.Background(), &Dispatch{}, "")
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("expected generator error, got %v", err)
	}
}

type stubTextGenerator struct {
	prompt string
	text   string
	err    error
}

func (s *stubTextGenerator) GenerateText(_ context.Context, prompt string, _ string) (string, error) {
	s.prompt = prompt
	if s.err != nil {
		return "", s.err
	}
	return s.text, nil
}
