package orcarouter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
)

// defaultModel is used when the caller passes an empty model hint. It is an
// OrcaRouter model that reliably supports plain text generation.
const defaultModel = "orcarouter/auto"

// httpClient bounds the total time for a single chat-completions call. Summary
// generation feeds a condensed transcript to the model, so a long-running
// generation is expected; 5 minutes matches the explain layer's default
// patience for provider calls.
var httpClient = &http.Client{ //nolint:gochecknoglobals // package-level client is shared and read-only
	Timeout: 5 * time.Minute,
}

// chatCompletionRequest mirrors the OpenAI chat-completions wire format that
// OrcaRouter exposes at /chat/completions.
type chatCompletionRequest struct {
	Model     string        `json:"model"`
	Messages  []chatMessage `json:"messages"`
	MaxTokens int           `json:"max_tokens,omitempty"`
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// chatCompletionResponse is the subset of the OpenAI chat-completions response
// that the text generator reads.
type chatCompletionResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error"`
}

// GenerateText sends a prompt to OrcaRouter's OpenAI-compatible chat
// completions endpoint and returns the raw text response. It implements the
// agent.TextGenerator interface.
//
// The API key is read from the environment on each call (ORCAROUTER_API_KEY),
// so the provider works wherever the key is set — CI, a hook environment, or a
// login shell — without persisting the secret anywhere.
func (o *OrcaRouterAgent) GenerateText(ctx context.Context, prompt string, model string) (string, error) {
	if strings.TrimSpace(prompt) == "" {
		return "", errors.New("orcarouter: empty prompt")
	}

	apiKey := APIKey()
	if apiKey == "" {
		return "", errors.New("orcarouter: " + EnvAPIKey + " is not set; set it to use OrcaRouter for summary generation")
	}

	if model == "" {
		model = defaultModel
	}

	req := chatCompletionRequest{
		Model:    model,
		Messages: []chatMessage{{Role: "user", Content: prompt}},
	}
	body, err := json.Marshal(req)
	if err != nil {
		return "", fmt.Errorf("orcarouter: encode request: %w", err)
	}

	endpoint := strings.TrimRight(APIBaseURL(), "/") + "/chat/completions"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("orcarouter: build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+apiKey)

	resp, err := httpClient.Do(httpReq)
	if err != nil {
		return "", fmt.Errorf("orcarouter: request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 10<<20)) // 10 MiB cap
	if err != nil {
		return "", fmt.Errorf("orcarouter: read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("orcarouter: gateway returned HTTP %d: %s", resp.StatusCode, summarizeResponseError(respBody))
	}

	var parsed chatCompletionResponse
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return "", fmt.Errorf("orcarouter: parse response: %w", err)
	}
	if parsed.Error != nil {
		return "", fmt.Errorf("orcarouter: gateway error: %s", parsed.Error.Message)
	}
	if len(parsed.Choices) == 0 || strings.TrimSpace(parsed.Choices[0].Message.Content) == "" {
		return "", errors.New("orcarouter: gateway returned an empty response")
	}

	return strings.TrimSpace(parsed.Choices[0].Message.Content), nil
}

// summarizeResponseError extracts a human-readable message from a non-200
// response body. OrcaRouter returns OpenAI-style {"error": {"message": ...}}
// envelopes on most failures; when the body does not parse, the raw text is
// truncated and returned so the user still sees the server's answer.
func summarizeResponseError(body []byte) string {
	if len(body) == 0 {
		return "empty error response"
	}
	var envelope struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &envelope); err == nil && envelope.Error.Message != "" {
		return envelope.Error.Message
	}
	text := strings.TrimSpace(string(body))
	if len(text) > 200 {
		text = text[:200] + "..."
	}
	return text
}

var _ agent.TextGenerator = (*OrcaRouterAgent)(nil)
