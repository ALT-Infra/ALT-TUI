package together

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
	Name            = "together"
	Route           = "serverless"
	DefaultEndpoint = "https://api.together.xyz/v1"
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
				Name:                  "Together AI",
				CredentialEnvironment: "ALT_TOGETHER_API_KEY",
				MultiModelCatalog:     true,
				Routes: []provider.GatewayRoute{
					{ID: Route, Label: "Serverless"},
				},
			},
			Route:    Route,
			BaseURL:  DefaultEndpoint,
			Hostname: "api.together.xyz",
		},
	)}
}

type catalogModel struct {
	ID          string `json:"id"`
	Type        string `json:"type"`
	DisplayName string `json:"display_name"`
}

func (f *Factory) ListModels(ctx context.Context) ([]provider.CatalogModel, error) {
	var payload []catalogModel
	if err := f.GetJSON(ctx, DefaultEndpoint+"/models", &payload); err != nil {
		return nil, err
	}
	result := make([]provider.CatalogModel, 0, len(payload))
	seen := make(map[string]bool, len(payload))
	for _, item := range payload {
		id := strings.TrimSpace(item.ID)
		if id == "" || seen[id] || !chatModelType(item.Type) {
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
		return nil, fmt.Errorf("Together returned no chat-completion models")
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result, nil
}

func chatModelType(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "chat", "language", "code":
		return true
	default:
		return false
	}
}
