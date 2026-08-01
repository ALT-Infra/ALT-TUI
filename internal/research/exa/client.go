package exa

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

const BaseURL = "https://api.exa.ai"

type CredentialResolver func() (string, error)

type Client struct {
	ResolveCredential CredentialResolver
	HTTPClient        *http.Client
	BaseURL           string
}

type SearchRequest struct {
	Query              string         `json:"query"`
	Type               string         `json:"type,omitempty"`
	NumResults         int            `json:"numResults,omitempty"`
	IncludeDomains     []string       `json:"includeDomains,omitempty"`
	ExcludeDomains     []string       `json:"excludeDomains,omitempty"`
	StartPublishedDate string         `json:"startPublishedDate,omitempty"`
	EndPublishedDate   string         `json:"endPublishedDate,omitempty"`
	Category           string         `json:"category,omitempty"`
	Contents           map[string]any `json:"contents"`
}

type ContentsRequest struct {
	URLs     []string       `json:"urls"`
	Contents map[string]any `json:"-"`
}

func (c Client) Search(ctx context.Context, request SearchRequest) (map[string]any, error) {
	request.Query = strings.TrimSpace(request.Query)
	if request.Query == "" {
		return nil, fmt.Errorf("query is required")
	}
	return c.request(ctx, "/search", request)
}

func (c Client) Contents(ctx context.Context, request ContentsRequest) (map[string]any, error) {
	if len(request.URLs) == 0 {
		return nil, fmt.Errorf("at least one URL is required")
	}
	payload := make(map[string]any, len(request.Contents)+1)
	payload["urls"] = request.URLs
	for key, value := range request.Contents {
		payload[key] = value
	}
	return c.request(ctx, "/contents", payload)
}

func (c Client) request(ctx context.Context, path string, payload any) (map[string]any, error) {
	if c.ResolveCredential == nil {
		return nil, fmt.Errorf("Exa is not configured; run `alt auth set exa`")
	}
	credential, err := c.ResolveCredential()
	if err != nil {
		return nil, fmt.Errorf("resolve Exa credential: %w", err)
	}
	credential = strings.TrimSpace(credential)
	if credential == "" {
		return nil, fmt.Errorf("Exa is not configured; run `alt auth set exa`")
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("encode Exa request: %w", err)
	}
	baseURL := strings.TrimRight(c.BaseURL, "/")
	if baseURL == "" {
		baseURL = BaseURL
	}
	request, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		baseURL+path,
		bytes.NewReader(body),
	)
	if err != nil {
		return nil, fmt.Errorf("create Exa request: %w", err)
	}
	request.Header.Set("content-type", "application/json")
	request.Header.Set("x-api-key", credential)
	request.Header.Set("user-agent", "ALT-chat")
	client := c.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("call Exa: %w", err)
	}
	defer response.Body.Close()
	var decoded map[string]any
	if err := json.NewDecoder(response.Body).Decode(&decoded); err != nil {
		return nil, fmt.Errorf("decode Exa response (HTTP %d): %w", response.StatusCode, err)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		if message := responseMessage(decoded); message != "" {
			return nil, fmt.Errorf("Exa returned HTTP %d: %s", response.StatusCode, message)
		}
		return nil, fmt.Errorf("Exa returned HTTP %d", response.StatusCode)
	}
	return decoded, nil
}

func responseMessage(value map[string]any) string {
	for _, key := range []string{"error", "message", "detail"} {
		switch item := value[key].(type) {
		case string:
			return strings.TrimSpace(item)
		case map[string]any:
			if message, ok := item["message"].(string); ok {
				return strings.TrimSpace(message)
			}
		}
	}
	return ""
}
