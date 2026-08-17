package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
)

const modelsDevCatalogURL = "https://models.dev/api.json"

// ModelsDevSource resolves live model facts from the same open catalog used by
// OpenCode and Cline. HTTP validators, rather than a local refresh interval,
// decide whether the cached document is current.
type ModelsDevSource struct {
	client *http.Client
	url    string

	mu        sync.Mutex
	etag      string
	providers map[string]modelsDevProvider
}

func NewModelsDevSource(client *http.Client) *ModelsDevSource {
	if client == nil {
		client = http.DefaultClient
	}
	return &ModelsDevSource{
		client:    client,
		url:       modelsDevCatalogURL,
		providers: make(map[string]modelsDevProvider),
	}
}

type modelsDevProvider struct {
	Models map[string]modelsDevModel `json:"models"`
}

type modelsDevModel struct {
	ID               string `json:"id"`
	Name             string `json:"name"`
	ToolCall         *bool  `json:"tool_call"`
	StructuredOutput *bool  `json:"structured_output"`
	Limit            struct {
		Context int `json:"context"`
		Input   int `json:"input"`
		Output  int `json:"output"`
	} `json:"limit"`
	Modalities *struct {
		Input  []string `json:"input"`
		Output []string `json:"output"`
	} `json:"modalities"`
}

func (s *ModelsDevSource) Enrich(
	ctx context.Context,
	descriptor GatewayDescriptor,
	models []CatalogModel,
) ([]CatalogModel, error) {
	if s == nil || len(models) == 0 {
		return append([]CatalogModel(nil), models...), nil
	}
	routeCatalogs := make(map[string]string, len(descriptor.Routes))
	wanted := make(map[string]bool, len(descriptor.Routes))
	for _, route := range descriptor.Routes {
		catalog := strings.TrimSpace(route.MetadataCatalog)
		if catalog == "" {
			continue
		}
		routeCatalogs[strings.TrimSpace(route.ID)] = catalog
		wanted[catalog] = true
	}
	if len(wanted) == 0 {
		return append([]CatalogModel(nil), models...), nil
	}

	providers, err := s.load(ctx, wanted)
	if err != nil {
		return nil, err
	}
	result := append([]CatalogModel(nil), models...)
	for index := range result {
		catalog := routeCatalogs[result[index].Route]
		metadata, ok := providers[catalog].Models[result[index].ID]
		if !ok {
			continue
		}
		applyModelsDevMetadata(&result[index], catalog, metadata)
	}
	return result, nil
}

func (s *ModelsDevSource) load(ctx context.Context, wanted map[string]bool) (map[string]modelsDevProvider, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	allCached := true
	for key := range wanted {
		if _, ok := s.providers[key]; !ok {
			allCached = false
			break
		}
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, s.url, nil)
	if err != nil {
		return nil, fmt.Errorf("create models.dev metadata request: %w", err)
	}
	request.Header.Set("Accept", "application/json")
	if allCached && s.etag != "" {
		request.Header.Set("If-None-Match", s.etag)
	}
	response, err := s.client.Do(request)
	if err != nil {
		if allCached {
			return copyRequestedProviders(s.providers, wanted), nil
		}
		return nil, fmt.Errorf("fetch models.dev metadata: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotModified && allCached {
		return copyRequestedProviders(s.providers, wanted), nil
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		if allCached {
			return copyRequestedProviders(s.providers, wanted), nil
		}
		return nil, fmt.Errorf("fetch models.dev metadata: HTTP %d", response.StatusCode)
	}
	loaded, err := decodeModelsDevProviders(response, wanted)
	if err != nil {
		if allCached {
			return copyRequestedProviders(s.providers, wanted), nil
		}
		return nil, err
	}
	for key, value := range loaded {
		s.providers[key] = value
	}
	if etag := strings.TrimSpace(response.Header.Get("ETag")); etag != "" {
		s.etag = etag
	}
	return copyRequestedProviders(s.providers, wanted), nil
}

func decodeModelsDevProviders(response *http.Response, wanted map[string]bool) (map[string]modelsDevProvider, error) {
	decoder := json.NewDecoder(response.Body)
	token, err := decoder.Token()
	if err != nil {
		return nil, fmt.Errorf("decode models.dev metadata: %w", err)
	}
	if delimiter, ok := token.(json.Delim); !ok || delimiter != '{' {
		return nil, fmt.Errorf("decode models.dev metadata: expected top-level object")
	}
	result := make(map[string]modelsDevProvider, len(wanted))
	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return nil, fmt.Errorf("decode models.dev provider key: %w", err)
		}
		key, _ := keyToken.(string)
		if !wanted[key] {
			var discard struct{}
			if err := decoder.Decode(&discard); err != nil {
				return nil, fmt.Errorf("skip models.dev provider %q: %w", key, err)
			}
			continue
		}
		var provider modelsDevProvider
		if err := decoder.Decode(&provider); err != nil {
			return nil, fmt.Errorf("decode models.dev provider %q: %w", key, err)
		}
		result[key] = provider
	}
	if _, err := decoder.Token(); err != nil {
		return nil, fmt.Errorf("finish models.dev metadata: %w", err)
	}
	missing := make([]string, 0)
	for key := range wanted {
		if _, ok := result[key]; !ok {
			missing = append(missing, key)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return nil, fmt.Errorf("models.dev omitted adapter-declared catalog(s): %s", strings.Join(missing, ", "))
	}
	return result, nil
}

func applyModelsDevMetadata(target *CatalogModel, catalog string, metadata modelsDevModel) {
	if target == nil {
		return
	}
	if target.DisplayName == "" {
		target.DisplayName = strings.TrimSpace(metadata.Name)
	}
	if target.Capabilities.ToolCalling == "" || target.Capabilities.ToolCalling == CapabilityUnknown {
		target.Capabilities.ToolCalling = capabilityFromBool(metadata.ToolCall)
	}
	if target.Capabilities.StructuredOutput == "" || target.Capabilities.StructuredOutput == CapabilityUnknown {
		target.Capabilities.StructuredOutput = capabilityFromBool(metadata.StructuredOutput)
	}
	if (target.Capabilities.ImageInput == "" || target.Capabilities.ImageInput == CapabilityUnknown) && metadata.Modalities != nil {
		target.Capabilities.ImageInput = CapabilityUnsupported
		for _, modality := range metadata.Modalities.Input {
			if strings.EqualFold(strings.TrimSpace(modality), "image") {
				target.Capabilities.ImageInput = CapabilitySupported
				break
			}
		}
	}
	source := LimitSource("models.dev:" + catalog)
	target.Limits = MergeModelLimits(target.Limits, ModelLimits{
		ContextWindow: NewTokenLimit(metadata.Limit.Context, source),
		MaxInput:      NewTokenLimit(metadata.Limit.Input, source),
		MaxOutput:     NewTokenLimit(metadata.Limit.Output, source),
	})
}

func capabilityFromBool(value *bool) CapabilityState {
	if value == nil {
		return CapabilityUnknown
	}
	if *value {
		return CapabilitySupported
	}
	return CapabilityUnsupported
}

func copyRequestedProviders(source map[string]modelsDevProvider, wanted map[string]bool) map[string]modelsDevProvider {
	result := make(map[string]modelsDevProvider, len(wanted))
	for key := range wanted {
		result[key] = source[key]
	}
	return result
}
