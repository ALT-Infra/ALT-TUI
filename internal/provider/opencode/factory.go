package opencode

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"

	"altv1/internal/credential"
	"altv1/internal/profile"
	"altv1/internal/provider"

	einoopenai "github.com/cloudwego/eino-ext/components/model/openai"
	"github.com/cloudwego/eino/components/model"
)

const (
	Name            = "opencode"
	DefaultEndpoint = "https://opencode.ai/zen/go/v1"
	ZenEndpoint     = "https://opencode.ai/zen/v1"
	GoRoute         = "go"
	ZenRoute        = "zen"
)

type Factory struct {
	Credentials credential.Store
	HTTPClient  *http.Client
}

func (*Factory) Descriptor() provider.GatewayDescriptor {
	return provider.GatewayDescriptor{
		ID:                    Name,
		Name:                  "OpenCode",
		CredentialEnvironment: "ALT_OPENCODE_API_KEY",
		MultiModelCatalog:     true,
		Routes: []provider.GatewayRoute{
			{ID: GoRoute, Label: "Go", MetadataCatalog: "opencode-go"},
			{ID: ZenRoute, Label: "Zen", MetadataCatalog: "opencode"},
		},
	}
}

func (*Factory) Capabilities(profile.Model) provider.Capabilities {
	return provider.Capabilities{
		StructuredOutput: provider.CapabilityUnknown,
		ToolCalling:      provider.CapabilityUnknown,
	}
}

func NewFactory(credentials credential.Store) *Factory {
	return &Factory{
		Credentials: credentials,
		HTTPClient:  &http.Client{Transport: http.DefaultTransport},
	}
}

func (f *Factory) NewChatModel(ctx context.Context, spec profile.Model, mode provider.Mode) (model.BaseChatModel, error) {
	endpoint, ok := routeEndpoint(spec.Route)
	if !ok {
		return nil, fmt.Errorf("unknown OpenCode catalog route %q", spec.Route)
	}
	if err := validateEndpoint(endpoint); err != nil {
		return nil, err
	}
	key, err := f.Credentials.Resolve(Name, f.Descriptor().CredentialEnvironment)
	if err != nil {
		return nil, err
	}

	config := &einoopenai.ChatModelConfig{
		APIKey:     key,
		BaseURL:    endpoint,
		Model:      spec.Name,
		HTTPClient: provider.CacheAwareHTTPClient(f.HTTPClient),
	}
	// OpenCode's authenticated catalog does not currently attest native
	// structured-output support. Unknown is not promoted to Supported: ALT
	// requests JSON in the prompt and validates it itself.
	_ = mode
	if spec.ReasoningEffort != "" {
		config.ExtraFields = map[string]any{"reasoning_effort": spec.ReasoningEffort}
	}
	chat, err := einoopenai.NewChatModel(ctx, config)
	if err != nil {
		return nil, err
	}
	return provider.ObserveCacheUsage(chat), nil
}

type modelsResponse struct {
	Data []struct {
		ID string `json:"id"`
	} `json:"data"`
}

// ListModels asks OpenCode for both catalogs that the configured credential
// can see. A model is not invented or substituted locally: every returned
// choice was present in a successful provider response.
func (f *Factory) ListModels(ctx context.Context) ([]provider.CatalogModel, error) {
	key, err := f.Credentials.Resolve(Name, "ALT_OPENCODE_API_KEY")
	if err != nil {
		return nil, err
	}
	type catalog struct {
		endpoint string
		route    string
	}
	catalogs := []catalog{
		{endpoint: DefaultEndpoint, route: GoRoute},
		{endpoint: ZenEndpoint, route: ZenRoute},
	}
	var (
		result []provider.CatalogModel
		errs   []string
	)
	for _, item := range catalogs {
		models, listErr := f.listEndpoint(ctx, key, item.endpoint)
		if listErr != nil {
			errs = append(errs, item.route+": "+listErr.Error())
			continue
		}
		for _, id := range models {
			result = append(result, provider.CatalogModel{
				Gateway: Name,
				Route:   item.route,
				ID:      id,
				Capabilities: provider.Capabilities{
					StructuredOutput: provider.CapabilityUnknown,
					ToolCalling:      provider.CapabilityUnknown,
				},
			})
		}
	}
	if len(result) == 0 {
		return nil, fmt.Errorf("no OpenCode catalog was reachable (%s)", strings.Join(errs, "; "))
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Route != result[j].Route {
			return result[i].Route < result[j].Route
		}
		return result[i].ID < result[j].ID
	})
	return result, nil
}

func (f *Factory) listEndpoint(ctx context.Context, key, endpoint string) ([]string, error) {
	if err := validateEndpoint(endpoint); err != nil {
		return nil, err
	}
	request, err := http.NewRequestWithContext(
		ctx,
		http.MethodGet,
		strings.TrimRight(endpoint, "/")+"/models",
		nil,
	)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+key)
	request.Header.Set("Accept", "application/json")
	response, err := f.HTTPClient.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, fmt.Errorf("HTTP %d", response.StatusCode)
	}
	var payload modelsResponse
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	models := make([]string, 0, len(payload.Data))
	for _, model := range payload.Data {
		if id := strings.TrimSpace(model.ID); id != "" {
			models = append(models, id)
		}
	}
	return models, nil
}

func validateEndpoint(raw string) error {
	parsed, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("parse OpenCode endpoint: %w", err)
	}
	if os.Getenv("ALT_ALLOW_INSECURE_PROVIDER_ENDPOINT") == "1" {
		if parsed.Scheme != "http" && parsed.Scheme != "https" {
			return fmt.Errorf("OpenCode endpoint must use HTTP or HTTPS")
		}
		return nil
	}
	if parsed.Scheme != "https" {
		return fmt.Errorf("OpenCode endpoint must use HTTPS")
	}
	if parsed.Hostname() != "opencode.ai" {
		return fmt.Errorf("refusing to send an OpenCode credential to %s", parsed.Hostname())
	}
	return nil
}

func routeEndpoint(route string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(route)) {
	case GoRoute:
		return DefaultEndpoint, true
	case ZenRoute:
		return ZenEndpoint, true
	default:
		return "", false
	}
}
