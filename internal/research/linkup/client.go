package linkup

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

const BaseURL = "https://api.linkup.so/v1"

const maxErrorBody = 16 * 1024

type CredentialResolver func() (string, error)

type Client struct {
	ResolveCredential CredentialResolver
	HTTPClient        *http.Client
	BaseURL           string
}

type SearchRequest struct {
	Query                  string         `json:"q"`
	Depth                  string         `json:"depth"`
	OutputType             string         `json:"outputType"`
	IncludeImages          bool           `json:"includeImages,omitempty"`
	IncludeDomains         []string       `json:"includeDomains,omitempty"`
	ExcludeDomains         []string       `json:"excludeDomains,omitempty"`
	FromDate               string         `json:"fromDate,omitempty"`
	ToDate                 string         `json:"toDate,omitempty"`
	MaxResults             int            `json:"maxResults,omitempty"`
	IncludeInlineCitations bool           `json:"includeInlineCitations,omitempty"`
	IncludeSources         bool           `json:"includeSources,omitempty"`
	StructuredOutputSchema map[string]any `json:"-"`
}

type FetchRequest struct {
	URL               string `json:"url"`
	RenderJS          bool   `json:"renderJs,omitempty"`
	IncludeRawContent bool   `json:"includeRawContent,omitempty"`
	ExtractImages     bool   `json:"extractImages,omitempty"`
}

func (c Client) Search(ctx context.Context, request SearchRequest) (map[string]any, error) {
	request.Query = strings.TrimSpace(request.Query)
	if request.Query == "" {
		return nil, fmt.Errorf("query is required")
	}
	payload := map[string]any{
		"q": request.Query, "depth": request.Depth, "outputType": request.OutputType,
	}
	if request.IncludeImages {
		payload["includeImages"] = true
	}
	if len(request.IncludeDomains) > 0 {
		payload["includeDomains"] = request.IncludeDomains
	}
	if len(request.ExcludeDomains) > 0 {
		payload["excludeDomains"] = request.ExcludeDomains
	}
	if request.FromDate != "" {
		payload["fromDate"] = request.FromDate
	}
	if request.ToDate != "" {
		payload["toDate"] = request.ToDate
	}
	if request.MaxResults > 0 {
		payload["maxResults"] = request.MaxResults
	}
	if request.IncludeInlineCitations {
		payload["includeInlineCitations"] = true
	}
	if request.IncludeSources {
		payload["includeSources"] = true
	}
	if len(request.StructuredOutputSchema) > 0 {
		encoded, err := json.Marshal(request.StructuredOutputSchema)
		if err != nil {
			return nil, fmt.Errorf("encode Linkup structured output schema: %w", err)
		}
		payload["structuredOutputSchema"] = string(encoded)
	}
	return c.request(ctx, "/search", payload)
}

func (c Client) Fetch(ctx context.Context, request FetchRequest) (map[string]any, error) {
	request.URL = strings.TrimSpace(request.URL)
	if request.URL == "" {
		return nil, fmt.Errorf("URL is required")
	}
	return c.request(ctx, "/fetch", request)
}

func (c Client) request(ctx context.Context, path string, payload any) (map[string]any, error) {
	if c.ResolveCredential == nil {
		return nil, fmt.Errorf("Linkup is not configured; run `alt auth set linkup`")
	}
	credential, err := c.ResolveCredential()
	if err != nil {
		return nil, fmt.Errorf("resolve Linkup credential: %w", err)
	}
	credential = strings.TrimSpace(credential)
	if credential == "" {
		return nil, fmt.Errorf("Linkup is not configured; run `alt auth set linkup`")
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("encode Linkup request: %w", err)
	}
	baseURL := strings.TrimRight(c.BaseURL, "/")
	if baseURL == "" {
		baseURL = BaseURL
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+path, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create Linkup request: %w", err)
	}
	request.Header.Set("authorization", "Bearer "+credential)
	request.Header.Set("content-type", "application/json")
	request.Header.Set("user-agent", "ALT-TUI")
	client := c.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("call Linkup: %w", err)
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, 64*1024*1024))
	if err != nil {
		return nil, fmt.Errorf("read Linkup response (HTTP %d): %w", response.StatusCode, err)
	}
	var decoded map[string]any
	decodeErr := json.Unmarshal(raw, &decoded)
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		if decodeErr == nil {
			if message := responseMessage(decoded); message != "" {
				return nil, fmt.Errorf("Linkup returned HTTP %d: %s", response.StatusCode, message)
			}
		}
		return nil, fmt.Errorf("Linkup returned HTTP %d: %s", response.StatusCode, bodySnippet(raw))
	}
	if decodeErr != nil {
		return nil, fmt.Errorf("decode Linkup response (HTTP %d): %w", response.StatusCode, decodeErr)
	}
	return decoded, nil
}

func responseMessage(value map[string]any) string {
	if item, ok := value["message"].(string); ok {
		return strings.TrimSpace(item)
	}
	if problem, ok := value["error"].(map[string]any); ok {
		if item, ok := problem["message"].(string); ok {
			return strings.TrimSpace(item)
		}
		if item, ok := problem["code"].(string); ok {
			return strings.TrimSpace(item)
		}
	}
	if item, ok := value["error"].(string); ok {
		return strings.TrimSpace(item)
	}
	return ""
}

func bodySnippet(raw []byte) string {
	if len(raw) > maxErrorBody {
		raw = raw[:maxErrorBody]
	}
	value := strings.Join(strings.Fields(string(raw)), " ")
	if value == "" {
		return "empty response"
	}
	return value
}
