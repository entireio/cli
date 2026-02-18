package summarize

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
)

const openAIDefaultModel = "gpt-5-mini-mini"
const openAIAPIEndpoint = "https://api.openai.com/v1/chat/completions"

// OpenAIGenerator generates summaries using the OpenAI API.
type OpenAIGenerator struct {
	// APIKey is the OpenAI API key.
	// Falls back to OPENAI_API_KEY environment variable if empty.
	APIKey string

	// Model is the OpenAI model to use.
	// Defaults to openAIDefaultModel if empty.
	Model string

	// HTTPClient is used for API requests. Defaults to http.DefaultClient if nil.
	HTTPClient *http.Client

	// endpoint overrides the API URL (used in tests).
	endpoint string
}

type openAIRequest struct {
	Model    string          `json:"model"`
	Messages []openAIMessage `json:"messages"`
}

type openAIMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type openAIResponse struct {
	Choices []openAIChoice `json:"choices"`
	Error   *openAIError   `json:"error,omitempty"`
}

type openAIChoice struct {
	Message openAIMessage `json:"message"`
}

type openAIError struct {
	Message string `json:"message"`
	Type    string `json:"type"`
}

// Generate creates a summary by calling the OpenAI chat completions API.
func (g *OpenAIGenerator) Generate(ctx context.Context, input Input) (*checkpoint.Summary, error) {
	apiKey := g.APIKey
	if apiKey == "" {
		apiKey = os.Getenv("OPENAI_API_KEY")
	}
	if apiKey == "" {
		return nil, errors.New("OpenAI API key not configured: set summarize.api_key in settings or OPENAI_API_KEY environment variable")
	}

	model := g.Model
	if model == "" {
		model = openAIDefaultModel
	}

	transcriptText := FormatCondensedTranscript(input)
	prompt := buildSummarizationPrompt(transcriptText)

	reqBody := openAIRequest{
		Model:    model,
		Messages: []openAIMessage{{Role: "user", Content: prompt}},
	}
	reqBytes, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal OpenAI request: %w", err)
	}

	endpoint := g.endpoint
	if endpoint == "" {
		endpoint = openAIAPIEndpoint
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(reqBytes))
	if err != nil {
		return nil, fmt.Errorf("failed to create OpenAI request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)

	client := g.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to call OpenAI API: %w", err)
	}
	defer resp.Body.Close()

	var apiResp openAIResponse
	if err := json.NewDecoder(resp.Body).Decode(&apiResp); err != nil {
		return nil, fmt.Errorf("failed to parse OpenAI response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		errMsg := "unknown error"
		if apiResp.Error != nil {
			errMsg = apiResp.Error.Message
		}
		return nil, fmt.Errorf("OpenAI API error (status %d): %s", resp.StatusCode, errMsg)
	}

	if len(apiResp.Choices) == 0 {
		return nil, errors.New("OpenAI API returned no choices")
	}

	resultJSON := extractJSONFromMarkdown(apiResp.Choices[0].Message.Content)
	var summary checkpoint.Summary
	if err := json.Unmarshal([]byte(resultJSON), &summary); err != nil {
		return nil, fmt.Errorf("failed to parse summary JSON from OpenAI: %w (response: %s)", err, resultJSON)
	}

	return &summary, nil
}
