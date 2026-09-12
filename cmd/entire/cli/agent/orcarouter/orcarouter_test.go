package orcarouter

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAPIKeyAndBaseURLDefaults(t *testing.T) {
	// Note: no t.Parallel() — Setenv cannot be used after Parallel, and these
	// subtests mutate process environment.
	t.Run("empty key by default", func(t *testing.T) {
		t.Setenv(EnvAPIKey, "")
		if got := APIKey(); got != "" {
			t.Errorf("APIKey() = %q, want empty", got)
		}
	})

	t.Run("key returned when set", func(t *testing.T) {
		t.Setenv(EnvAPIKey, "sk-orca-test")
		if got := APIKey(); got != "sk-orca-test" {
			t.Errorf("APIKey() = %q, want sk-orca-test", got)
		}
	})

	t.Run("base URL defaults to production", func(t *testing.T) {
		t.Setenv(EnvAPIBaseURL, "")
		if got, want := APIBaseURL(), "https://api.orcarouter.ai/v1"; got != want {
			t.Errorf("APIBaseURL() = %q, want %q", got, want)
		}
	})

	t.Run("base URL override", func(t *testing.T) {
		t.Setenv(EnvAPIBaseURL, "https://example.com/v1/")
		if got, want := APIBaseURL(), "https://example.com/v1/"; got != want {
			t.Errorf("APIBaseURL() = %q, want %q", got, want)
		}
	})
}

func TestDetectPresenceAlwaysFalse(t *testing.T) {
	t.Setenv(EnvAPIKey, "sk-orca-test")
	ag := &OrcaRouterAgent{}
	present, err := ag.DetectPresence(context.Background())
	if err != nil {
		t.Fatalf("DetectPresence() error = %v", err)
	}
	if present {
		t.Error("DetectPresence() = true, want false (provider must not auto-detect as a coding agent)")
	}
}

func TestGenerateText(t *testing.T) {
	const prompt = "Summarize the session."
	const wantText = "The session added a login flow."

	var srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify the request shape: OpenAI chat-completions wire format. The
		// test sets the base URL to the bare server root, so the path is
		// /chat/completions (the gateway base already includes any version
		// prefix such as /v1).
		if r.URL.Path != "/chat/completions" {
			t.Errorf("request path = %q, want /chat/completions", r.URL.Path)
		}
		if auth := r.Header.Get("Authorization"); auth != "Bearer sk-orca-test" {
			t.Errorf("Authorization = %q, want Bearer sk-orca-test", auth)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("Content-Type = %q, want application/json", ct)
		}

		var body chatCompletionRequest
		if err := jsonUnmarshalRequestBody(r, &body); err != nil {
			t.Errorf("decode request body: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if body.Model != "orcarouter/auto" {
			t.Errorf("model = %q, want default orcarouter/auto", body.Model)
		}
		if len(body.Messages) != 1 || body.Messages[0].Role != "user" || body.Messages[0].Content != prompt {
			t.Errorf("messages = %+v, want single user message with prompt", body.Messages)
		}

		w.Header().Set("Content-Type", "application/json")
		writeJSONBody(w, `{"choices":[{"message":{"role":"assistant","content":"`+wantText+`"}}]}`)
	}))
	defer srv.Close()

	t.Setenv(EnvAPIKey, "sk-orca-test")
	t.Setenv(EnvAPIBaseURL, srv.URL)

	ag := &OrcaRouterAgent{}
	got, err := ag.GenerateText(context.Background(), prompt, "")
	if err != nil {
		t.Fatalf("GenerateText() error = %v", err)
	}
	if got != wantText {
		t.Errorf("GenerateText() = %q, want %q", got, wantText)
	}
}

func TestGenerateText_RequiresAPIKey(t *testing.T) {
	t.Setenv(EnvAPIKey, "")
	ag := &OrcaRouterAgent{}
	_, err := ag.GenerateText(context.Background(), "prompt", "")
	if err == nil {
		t.Fatal("GenerateText() error = nil, want missing-key error")
	}
	if !strings.Contains(err.Error(), EnvAPIKey) {
		t.Errorf("error %q does not mention %s", err.Error(), EnvAPIKey)
	}
}

func TestGenerateText_EmptyPrompt(t *testing.T) {
	t.Setenv(EnvAPIKey, "sk-orca-test")
	ag := &OrcaRouterAgent{}
	_, err := ag.GenerateText(context.Background(), "   ", "")
	if err == nil {
		t.Fatal("GenerateText() error = nil, want empty-prompt error")
	}
}

func TestGenerateText_PropagatesModelHint(t *testing.T) {
	var srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body chatCompletionRequest
		if err := jsonUnmarshalRequestBody(r, &body); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if body.Model != "anthropic/claude-haiku-4.5" {
			t.Errorf("model = %q, want anthropic/claude-haiku-4.5", body.Model)
		}
		w.Header().Set("Content-Type", "application/json")
		writeJSONBody(w, `{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`)
	}))
	defer srv.Close()

	t.Setenv(EnvAPIKey, "sk-orca-test")
	t.Setenv(EnvAPIBaseURL, srv.URL)

	ag := &OrcaRouterAgent{}
	if _, err := ag.GenerateText(context.Background(), "prompt", "anthropic/claude-haiku-4.5"); err != nil {
		t.Fatalf("GenerateText() error = %v", err)
	}
}

func TestGenerateText_GatewayError(t *testing.T) {
	var srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		writeJSONBody(w, `{"error":{"message":"Invalid API key","type":"authentication_error"}}`)
	}))
	defer srv.Close()

	t.Setenv(EnvAPIKey, "sk-orca-test")
	t.Setenv(EnvAPIBaseURL, srv.URL)

	ag := &OrcaRouterAgent{}
	_, err := ag.GenerateText(context.Background(), "prompt", "")
	if err == nil {
		t.Fatal("GenerateText() error = nil, want gateway error")
	}
	if !strings.Contains(err.Error(), "401") || !strings.Contains(err.Error(), "Invalid API key") {
		t.Errorf("error = %q, want HTTP 401 with Invalid API key", err.Error())
	}
}

func TestGenerateText_EmptyChoices(t *testing.T) {
	var srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		writeJSONBody(w, `{"choices":[]}`)
	}))
	defer srv.Close()

	t.Setenv(EnvAPIKey, "sk-orca-test")
	t.Setenv(EnvAPIBaseURL, srv.URL)

	ag := &OrcaRouterAgent{}
	_, err := ag.GenerateText(context.Background(), "prompt", "")
	if err == nil {
		t.Fatal("GenerateText() error = nil, want empty-response error")
	}
}

func TestGenerateText_UnavailableServer(t *testing.T) {
	// A server that accepts the connection and immediately closes it exercises
	// the request-failure path.
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {}))
	srvURL := srv.URL
	srv.Close()

	t.Setenv(EnvAPIKey, "sk-orca-test")
	t.Setenv(EnvAPIBaseURL, srvURL)

	ag := &OrcaRouterAgent{}
	_, err := ag.GenerateText(context.Background(), "prompt", "")
	if err == nil {
		t.Fatal("GenerateText() error = nil, want request failure")
	}
}

func TestGenerateText_ContextCancelled(t *testing.T) {
	var srv = httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer srv.Close()

	t.Setenv(EnvAPIKey, "sk-orca-test")
	t.Setenv(EnvAPIBaseURL, srv.URL)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	ag := &OrcaRouterAgent{}
	_, err := ag.GenerateText(ctx, "prompt", "")
	if err == nil {
		t.Fatal("GenerateText() error = nil, want cancelled error")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want context.Canceled", err)
	}
}

// jsonUnmarshalRequestBody decodes a JSON request body into v. It exists so the
// test helpers can assert on the wire format without duplicating the encoding.
func jsonUnmarshalRequestBody(r *http.Request, v any) error {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return err
	}
	return json.Unmarshal(body, v)
}

// writeJSONBody writes a raw JSON response body. It exists so the test handlers
// can emit responses without tripping errcheck's check-blank rule.
func writeJSONBody(w http.ResponseWriter, body string) {
	_, _ = io.WriteString(w, body) //nolint:errcheck // response write failure in a test server is irrelevant
}
