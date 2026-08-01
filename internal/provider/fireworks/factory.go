package fireworks

import (
	"context"
	"fmt"
	"net/url"
	"sort"
	"strings"

	"altv1/internal/credential"
	"altv1/internal/provider"
	"altv1/internal/provider/openaicompat"
)

const (
	Name              = "fireworks"
	Route             = "serverless"
	InferenceEndpoint = "https://api.fireworks.ai/inference/v1"
	CatalogEndpoint   = "https://api.fireworks.ai/v1/accounts/fireworks/models"
)

type Factory struct {
	*openaicompat.Factory
}

func NewFactory(credentials credential.Store) *Factory {
	return &Factory{Factory: openaicompat.NewFactory(
		credentials,
		openaicompat.Config{
			Descriptor: provider.GatewayDescriptor{
				ID:                    Name,
				Name:                  "Fireworks AI",
				CredentialEnvironment: "ALT_FIREWORKS_API_KEY",
				MultiModelCatalog:     true,
				Routes: []provider.GatewayRoute{
					{ID: Route, Label: "Serverless"},
				},
			},
			Route:    Route,
			BaseURL:  InferenceEndpoint,
			Hostname: "api.fireworks.ai",
		},
	)}
}

type modelsResponse struct {
	Models []struct {
		Name               string `json:"name"`
		DisplayName        string `json:"displayName"`
		ConversationConfig any    `json:"conversationConfig"`
		SupportsServerless bool   `json:"supportsServerless"`
		SupportsTools      *bool  `json:"supportsTools"`
	} `json:"models"`
	NextPageToken string `json:"nextPageToken"`
}

func (f *Factory) ListModels(ctx context.Context) ([]provider.CatalogModel, error) {
	next := ""
	seenTokens := map[string]bool{}
	seenModels := map[string]bool{}
	var result []provider.CatalogModel
	for {
		rawURL := CatalogEndpoint
		if next != "" {
			parsed, err := url.Parse(rawURL)
			if err != nil {
				return nil, err
			}
			query := parsed.Query()
			query.Set("pageToken", next)
			parsed.RawQuery = query.Encode()
			rawURL = parsed.String()
		}
		var payload modelsResponse
		if err := f.GetJSON(ctx, rawURL, &payload); err != nil {
			return nil, err
		}
		for _, item := range payload.Models {
			id := strings.TrimSpace(item.Name)
			if id == "" || seenModels[id] ||
				!item.SupportsServerless || item.ConversationConfig == nil {
				continue
			}
			seenModels[id] = true
			result = append(result, provider.CatalogModel{
				Gateway:     Name,
				Route:       Route,
				ID:          id,
				DisplayName: strings.TrimSpace(item.DisplayName),
				Capabilities: provider.Capabilities{
					StructuredOutput: provider.CapabilityUnknown,
					ToolCalling:      capability(item.SupportsTools),
				},
			})
		}
		next = strings.TrimSpace(payload.NextPageToken)
		if next == "" {
			break
		}
		if seenTokens[next] {
			return nil, fmt.Errorf("Fireworks repeated catalog page token")
		}
		seenTokens[next] = true
	}
	if len(result) == 0 {
		return nil, fmt.Errorf("Fireworks returned no serverless chat models")
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result, nil
}

func capability(value *bool) provider.CapabilityState {
	if value == nil {
		return provider.CapabilityUnknown
	}
	if *value {
		return provider.CapabilitySupported
	}
	return provider.CapabilityUnsupported
}
