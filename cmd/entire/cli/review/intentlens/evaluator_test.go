package intentlens

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

type fakeRoundTripper func(*http.Request) (*http.Response, error)

func (f fakeRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestGeminiHTTPTransportUsesStructuredBoundedRequest(t *testing.T) {
	t.Parallel()
	for _, oversized := range []bool{false, true} {
		called := false
		transport := httpGeminiTransport{client: &http.Client{Transport: fakeRoundTripper(func(r *http.Request) (*http.Response, error) {
			called = true
			if r.Method != http.MethodPost || r.URL.Host != "generativelanguage.googleapis.com" || r.URL.RawQuery != "" {
				t.Fatal("unexpected provider request destination")
			}
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Fatal(err)
			}
			var request struct {
				GenerationConfig struct {
					MIME   string          `json:"responseMimeType"`
					Schema json.RawMessage `json:"responseJsonSchema"`
				} `json:"generationConfig"`
			}
			if err := json.Unmarshal(body, &request); err != nil {
				t.Fatal(err)
			}
			if request.GenerationConfig.MIME != "application/json" || !json.Valid(request.GenerationConfig.Schema) {
				t.Fatal("missing structured output contract")
			}
			response := string(verifiedResponse())
			if oversized {
				response = strings.Repeat("x", (1<<20)+1)
			}
			return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(response))}, nil
		})}}
		_, err := transport.Generate(context.Background(), "", CheckpointAuditPrompt(verifiedInput(t)), Schema())
		if !called || (err != nil) != oversized {
			t.Fatalf("called=%v oversized=%v error=%v", called, oversized, err)
		}
	}
}

type fakeGeminiTransport struct {
	response []byte
	err      error
	called   bool
	prompt   string
}

func (f *fakeGeminiTransport) Generate(_ context.Context, _ string, prompt string, _ json.RawMessage) ([]byte, error) {
	f.called = true
	f.prompt = prompt
	return f.response, f.err
}
func verifiedInput(t *testing.T) EvaluatorInput {
	t.Helper()
	input, err := NewEvaluatorInput([]RequirementSignals{{ImplementationVerified: true, ConnectionVerified: true, BehaviorPassed: true}}, true)
	if err != nil {
		t.Fatal(err)
	}
	return input
}
func verifiedResponse() []byte {
	b, _ := json.Marshal(Audit{Summary: "Verified.", Requirements: []Requirement{{
		ID: "R1", Requirement: "Verify behavior for R1", Status: StatusImplemented, Confidence: 0.9,
		Evidence: []Evidence{{Type: EvidenceCode, Explanation: "Local implementation and connection verified."}, {Type: EvidenceTest, Explanation: "Local behavior verification passed.", Result: "passed"}}, Recommendation: "",
	}}})
	return b
}
func TestGeminiEvaluatorValidatesFakeTransportResponse(t *testing.T) {
	t.Setenv("GEMINI_API_KEY", "set")
	transport := &fakeGeminiTransport{response: verifiedResponse()}
	result, err := NewGeminiEvaluator(transport).Evaluate(context.Background(), verifiedInput(t))
	if err != nil {
		t.Fatal(err)
	}
	if !transport.called {
		t.Fatal("expected fake transport request")
	}
	if _, err := ParseAuditJSON(result); err != nil {
		t.Fatal(err)
	}
}
func TestGeminiEvaluatorRejectsUnsupportedAndMalformedResults(t *testing.T) {
	t.Setenv("GEMINI_API_KEY", "set")
	for _, response := range [][]byte{[]byte("not JSON"), DemoAuditJSON(), []byte(strings.Replace(string(verifiedResponse()), "IMPLEMENTED", "INVALID", 1))} {
		_, err := NewGeminiEvaluator(&fakeGeminiTransport{response: response}).Evaluate(context.Background(), verifiedInput(t))
		if err == nil {
			t.Fatal("accepted malformed or mismatched result")
		}
	}
}
func TestGeminiEvaluatorIncompleteAndUnverifiedContextIsConservative(t *testing.T) {
	t.Parallel()
	for _, complete := range []bool{false, true} {
		input, err := NewEvaluatorInput([]RequirementSignals{{}}, complete)
		if err != nil {
			t.Fatal(err)
		}
		transport := &fakeGeminiTransport{response: verifiedResponse()}
		result, err := NewGeminiEvaluator(transport).Evaluate(context.Background(), input)
		if err != nil {
			t.Fatal(err)
		}
		if transport.called {
			t.Fatal("unverified context reached provider")
		}
		audit, err := ParseAuditJSON(result)
		if err != nil {
			t.Fatal(err)
		}
		if audit.Requirements[0].Status != StatusUncertain || audit.Requirements[0].Confidence > 0.25 {
			t.Fatal("unsafe verdict")
		}
	}
}
func TestGeminiEvaluatorInputStructurallyExcludesText(t *testing.T) {
	t.Parallel()
	var check func(reflect.Type)
	check = func(typ reflect.Type) {
		switch typ.Kind() {
		case reflect.Bool:
		case reflect.Struct:
			for i := 0; i < typ.NumField(); i++ {
				check(typ.Field(i).Type)
			}
		case reflect.Slice:
			check(typ.Elem())
		default:
			t.Fatalf("evaluator input admits raw data through %v", typ)
		}
	}
	check(reflect.TypeFor[EvaluatorInput]())
}
func TestGeminiEvaluatorUniqueSentinelCannotPopulateInput(t *testing.T) {
	t.Setenv("GEMINI_API_KEY", "set")
	input := verifiedInput(t)
	// Unknown JSON cannot populate private fields, including valid-looking text.
	sentinel := "UNIQUE_ARBITRARY_CONTENT_29c584"
	payload, _ := json.Marshal(map[string]any{"signals": sentinel, "complete": sentinel, "requirements": sentinel, "path": sentinel, "prompt": sentinel})
	if err := json.Unmarshal(payload, &input); err != nil {
		t.Fatal(err)
	}
	transport := &fakeGeminiTransport{response: verifiedResponse()}
	if _, err := NewGeminiEvaluator(transport).Evaluate(context.Background(), input); err != nil {
		t.Fatal(err)
	}
	if !transport.called {
		t.Fatal("sentinel test did not exercise the request")
	}
	if strings.Contains(transport.prompt, sentinel) {
		t.Fatal("raw content reached request")
	}
}
func TestGeminiEvaluatorMissingKeyAndProviderErrors(t *testing.T) {
	t.Setenv("GEMINI_API_KEY", "")
	transport := &fakeGeminiTransport{}
	if _, err := NewGeminiEvaluator(transport).Evaluate(context.Background(), verifiedInput(t)); err == nil {
		t.Fatal("expected missing credential error")
	}
	if transport.called {
		t.Fatal("called without credentials")
	}
	t.Setenv("GEMINI_API_KEY", "set")
	transport.err = errors.New("UNIQUE_ERROR_PAYLOAD_384")
	_, err := NewGeminiEvaluator(transport).Evaluate(context.Background(), verifiedInput(t))
	if err == nil || strings.Contains(err.Error(), "UNIQUE_ERROR_PAYLOAD_384") {
		t.Fatal("provider error payload exposed")
	}
}
func TestEvaluatorInputCopiesSignalsAndRejectsEmpty(t *testing.T) {
	t.Parallel()
	if _, err := NewEvaluatorInput(nil, true); err == nil {
		t.Fatal("accepted no requirements")
	}
	signals := []RequirementSignals{{}}
	input, _ := NewEvaluatorInput(signals, true)
	signals[0] = RequirementSignals{ImplementationVerified: true, ConnectionVerified: true, BehaviorPassed: true}
	if input.status(0) != StatusUncertain {
		t.Fatal("caller mutated sealed snapshot")
	}
}
