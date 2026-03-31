package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/api"
)

const (
	maxResponseBytes = 1 << 20
	clientID         = "entire-cli"
)

type Client struct {
	httpClient *http.Client
	baseURL    string
}

type DeviceAuthStart struct {
	DeviceCode              string `json:"device_code"`
	UserCode                string `json:"user_code"`
	VerificationURI         string `json:"verification_uri"`
	VerificationURIComplete string `json:"verification_uri_complete"`
	ExpiresIn               int    `json:"expires_in"`
	Interval                int    `json:"interval"`
}

type DeviceAuthPoll struct {
	AccessToken string `json:"access_token,omitempty"`
	TokenType   string `json:"token_type,omitempty"`
	ExpiresIn   int    `json:"expires_in,omitempty"`
	Scope       string `json:"scope,omitempty"`
	Error       string `json:"error,omitempty"`
}

type errorResponse struct {
	Error string `json:"error"`
}

func NewClient(httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = &http.Client{}
	}

	return &Client{
		httpClient: httpClient,
		baseURL:    api.BaseURL(),
	}
}

func (c *Client) BaseURL() string {
	return c.baseURL
}

func (c *Client) StartDeviceAuth(ctx context.Context) (*DeviceAuthStart, error) {
	body := url.Values{}
	body.Set("client_id", clientID)
	body.Set("scope", "cli")

	resp, err := c.postForm(ctx, "/oauth/device/code", body)
	if err != nil {
		return nil, fmt.Errorf("start device auth: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, readAPIError(resp, "start device auth")
	}

	var result DeviceAuthStart
	if err := decodeJSONStrict(resp.Body, &result); err != nil {
		return nil, fmt.Errorf("decode device auth start response: %w", err)
	}

	return &result, nil
}

func (c *Client) PollDeviceAuth(ctx context.Context, deviceCode string) (*DeviceAuthPoll, error) {
	body := url.Values{}
	body.Set("grant_type", "urn:ietf:params:oauth:grant-type:device_code")
	body.Set("client_id", clientID)
	body.Set("device_code", deviceCode)

	resp, err := c.postForm(ctx, "/oauth/token", body)
	if err != nil {
		return nil, fmt.Errorf("poll device auth: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		apiErr, err := readAPIErrorResponse(resp)
		if err != nil {
			return nil, fmt.Errorf("poll device auth: %w", err)
		}
		return &DeviceAuthPoll{Error: apiErr.Error}, nil
	}

	var result DeviceAuthPoll
	if err := decodeJSON(resp.Body, &result); err != nil {
		return nil, fmt.Errorf("decode device auth poll response: %w", err)
	}

	return &result, nil
}

// postForm sends a POST request with form-encoded body to an API-relative path.
func (c *Client) postForm(ctx context.Context, path string, body url.Values) (*http.Response, error) {
	endpoint, err := api.ResolveURLFromBase(c.baseURL, path)
	if err != nil {
		return nil, fmt.Errorf("resolve URL %s: %w", path, err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(body.Encode()))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", clientID)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request %s: %w", path, err)
	}

	return resp, nil
}

func readAPIErrorResponse(resp *http.Response) (*errorResponse, error) {
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}

	var apiErr errorResponse
	if err := json.Unmarshal(body, &apiErr); err == nil && strings.TrimSpace(apiErr.Error) != "" {
		return &apiErr, nil
	}

	text := strings.TrimSpace(string(body))
	if text != "" {
		return nil, fmt.Errorf("status %d: %s", resp.StatusCode, text)
	}

	return nil, fmt.Errorf("status %d", resp.StatusCode)
}

func readAPIError(resp *http.Response, action string) error {
	apiErr, err := readAPIErrorResponse(resp)
	if err == nil {
		return fmt.Errorf("%s: %s", action, apiErr.Error)
	}
	return fmt.Errorf("%s: %w", action, err)
}

func decodeJSON(r io.Reader, dest any) error {
	return decodeJSONWithOptions(r, dest, false)
}

func decodeJSONStrict(r io.Reader, dest any) error {
	return decodeJSONWithOptions(r, dest, true)
}

func decodeJSONWithOptions(r io.Reader, dest any, strict bool) error {
	body, err := io.ReadAll(io.LimitReader(r, maxResponseBytes))
	if err != nil {
		return fmt.Errorf("read JSON response: %w", err)
	}

	dec := json.NewDecoder(bytes.NewReader(body))
	if strict {
		dec.DisallowUnknownFields()
	}
	if err := dec.Decode(dest); err != nil {
		return fmt.Errorf("decode JSON response: %w", err)
	}

	return nil
}
