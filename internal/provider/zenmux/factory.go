package zenmux

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"altv1/internal/credential"
	"altv1/internal/provider"
	"altv1/internal/provider/openaicompat"
)

const (
	Name            = "zenmux"
	Route           = "openai"
	DefaultEndpoint = "https://zenmux.ai/api/v1"
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
				Name:                  "ZenMux",
				CredentialEnvironment: "ALT_ZENMUX_API_KEY",
				MultiModelCatalog:     true,
				Routes: []provider.GatewayRoute{
					{ID: Route, Label: "OpenAI-compatible"},
				},
			},
			Route:    Route,
			BaseURL:  DefaultEndpoint,
			Hostname: "zenmux.ai",
		},
	)}
}

type modelsResponse struct {
	Data []struct {
		ID               string   `json:"id"`
		DisplayName      string   `json:"display_name"`
		OutputModalities []string `json:"output_modalities"`
	} `json:"data"`
}

func (f *Factory) ListModels(ctx context.Context) ([]provider.CatalogModel, error) {
	var payload modelsResponse
	if err := f.GetJSON(ctx, DefaultEndpoint+"/models", &payload); err != nil {
		return nil, err
	}
	result := make([]provider.CatalogModel, 0, len(payload.Data))
	seen := make(map[string]bool, len(payload.Data))
	for _, item := range payload.Data {
		id := strings.TrimSpace(item.ID)
		if id == "" || seen[id] || !contains(item.OutputModalities, "text") {
			continue
		}
		seen[id] = true
		result = append(result, provider.CatalogModel{
			Gateway:     Name,
			Route:       Route,
			ID:          id,
			DisplayName: strings.TrimSpace(item.DisplayName),
			Capabilities: provider.Capabilities{
				StructuredOutput: provider.CapabilityUnknown,
				ToolCalling:      provider.CapabilityUnknown,
			},
		})
	}
	if len(result) == 0 {
		return nil, fmt.Errorf("ZenMux returned no text-output models")
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result, nil
}

func contains(values []string, expected string) bool {
	for _, value := range values {
		if strings.EqualFold(strings.TrimSpace(value), expected) {
			return true
		}
	}
	return false
}
