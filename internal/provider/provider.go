package provider

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"

	"altv1/internal/profile"

	"github.com/cloudwego/eino/components/model"
)

type Mode string

const (
	Text       Mode = "text"
	Structured Mode = "structured"
)

type CapabilityState string

const (
	CapabilityUnknown     CapabilityState = "unknown"
	CapabilitySupported   CapabilityState = "supported"
	CapabilityUnsupported CapabilityState = "unsupported"
)

type Capabilities struct {
	StructuredOutput CapabilityState `json:"structured_output"`
	ToolCalling      CapabilityState `json:"tool_calling"`
}

type GatewayRoute struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

// GatewayDescriptor contains the integration facts owned by an adapter. ALT's
// CLI, GUI, and profile layer consume this descriptor instead of embedding
// gateway-specific names, credential variables, routes, or endpoint rules.
type GatewayDescriptor struct {
	ID                    string         `json:"id"`
	Name                  string         `json:"name"`
	CredentialEnvironment string         `json:"credential_environment"`
	Routes                []GatewayRoute `json:"routes"`
	MultiModelCatalog     bool           `json:"multi_model_catalog"`
}

// CatalogModel is the exact gateway-issued choice ALT persists and executes.
// Gateway + Route + ID is its opaque identity; ALT neither rewrites the ID nor
// accepts an authored endpoint.
type CatalogModel struct {
	Gateway      string       `json:"gateway"`
	Route        string       `json:"route"`
	ID           string       `json:"id"`
	DisplayName  string       `json:"display_name,omitempty"`
	Capabilities Capabilities `json:"capabilities"`
}

type Gateway interface {
	Descriptor() GatewayDescriptor
	ListModels(ctx context.Context) ([]CatalogModel, error)
	NewChatModel(ctx context.Context, spec profile.Model, mode Mode) (model.BaseChatModel, error)
}

// CapabilitySource is optional because honest Unknown is preferable to a
// fabricated boolean. An adapter implements it only when it can determine the
// selected model's capabilities from authenticated catalog evidence.
type CapabilitySource interface {
	Capabilities(spec profile.Model) Capabilities
}

type Registry struct {
	mu           sync.RWMutex
	gateways     map[string]Gateway
	capabilities map[string]Capabilities
}

func NewRegistry() *Registry {
	return &Registry{
		gateways:     make(map[string]Gateway),
		capabilities: make(map[string]Capabilities),
	}
}

func (r *Registry) Register(gateway Gateway) error {
	if gateway == nil {
		return fmt.Errorf("gateway adapter is required")
	}
	descriptor := gateway.Descriptor()
	descriptor.ID = strings.ToLower(strings.TrimSpace(descriptor.ID))
	if descriptor.ID == "" || strings.TrimSpace(descriptor.Name) == "" {
		return fmt.Errorf("gateway ID and name are required")
	}
	if !descriptor.MultiModelCatalog {
		return fmt.Errorf("%s does not expose the multi-model catalog ALT requires", descriptor.ID)
	}
	if strings.TrimSpace(descriptor.CredentialEnvironment) == "" {
		return fmt.Errorf("%s credential environment is required", descriptor.ID)
	}
	seenRoutes := map[string]bool{}
	for _, route := range descriptor.Routes {
		id := strings.TrimSpace(route.ID)
		if id == "" || seenRoutes[id] {
			return fmt.Errorf("%s declares an empty or duplicate route", descriptor.ID)
		}
		seenRoutes[id] = true
	}
	if len(seenRoutes) == 0 {
		return fmt.Errorf("%s must declare at least one catalog route", descriptor.ID)
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.gateways[descriptor.ID]; exists {
		return fmt.Errorf("gateway %s is already registered", descriptor.ID)
	}
	r.gateways[descriptor.ID] = gateway
	return nil
}

func (r *Registry) Descriptor(name string) (GatewayDescriptor, error) {
	r.mu.RLock()
	gateway := r.gateways[strings.ToLower(strings.TrimSpace(name))]
	r.mu.RUnlock()
	if gateway == nil {
		return GatewayDescriptor{}, fmt.Errorf("gateway %s is not registered", name)
	}
	return gateway.Descriptor(), nil
}

func (r *Registry) Descriptors() []GatewayDescriptor {
	r.mu.RLock()
	result := make([]GatewayDescriptor, 0, len(r.gateways))
	for _, gateway := range r.gateways {
		result = append(result, gateway.Descriptor())
	}
	r.mu.RUnlock()
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result
}

func (r *Registry) Model(ctx context.Context, p profile.Profile, reference string, mode Mode) (model.BaseChatModel, profile.Model, error) {
	spec, ok := p.Models[reference]
	if !ok {
		return nil, profile.Model{}, fmt.Errorf("model reference %s is not defined", reference)
	}
	r.mu.RLock()
	gateway := r.gateways[strings.ToLower(strings.TrimSpace(spec.Gateway))]
	r.mu.RUnlock()
	if gateway == nil {
		return nil, profile.Model{}, fmt.Errorf("gateway %s is not registered", spec.Gateway)
	}
	instance, err := gateway.NewChatModel(ctx, spec, mode)
	if err != nil {
		return nil, profile.Model{}, fmt.Errorf("create %s model %s: %w", spec.Gateway, spec.Name, err)
	}
	return instance, spec, nil
}

func (r *Registry) Catalog(ctx context.Context, name string) ([]CatalogModel, error) {
	r.mu.RLock()
	gateway := r.gateways[strings.ToLower(strings.TrimSpace(name))]
	r.mu.RUnlock()
	if gateway == nil {
		return nil, fmt.Errorf("gateway %s is not registered", name)
	}
	models, err := gateway.ListModels(ctx)
	if err != nil {
		return nil, fmt.Errorf("list %s models: %w", name, err)
	}
	r.mu.Lock()
	for _, item := range models {
		r.capabilities[CatalogIdentity(item)] = item.Capabilities
	}
	r.mu.Unlock()
	return models, nil
}

func (r *Registry) Capabilities(spec profile.Model) Capabilities {
	unknown := Capabilities{
		StructuredOutput: CapabilityUnknown,
		ToolCalling:      CapabilityUnknown,
	}
	r.mu.RLock()
	gateway := r.gateways[strings.ToLower(strings.TrimSpace(spec.Gateway))]
	catalogValue, catalogKnown := r.capabilities[profile.ModelIdentity(spec)]
	r.mu.RUnlock()
	if catalogKnown {
		if catalogValue.StructuredOutput == "" {
			catalogValue.StructuredOutput = CapabilityUnknown
		}
		if catalogValue.ToolCalling == "" {
			catalogValue.ToolCalling = CapabilityUnknown
		}
		return catalogValue
	}
	source, ok := gateway.(CapabilitySource)
	if !ok {
		return unknown
	}
	value := source.Capabilities(spec)
	if value.StructuredOutput == "" {
		value.StructuredOutput = CapabilityUnknown
	}
	if value.ToolCalling == "" {
		value.ToolCalling = CapabilityUnknown
	}
	return value
}

func CatalogIdentity(item CatalogModel) string {
	return strings.Join([]string{
		strings.ToLower(strings.TrimSpace(item.Gateway)),
		strings.ToLower(strings.TrimSpace(item.Route)),
		strings.TrimSpace(item.ID),
	}, "\x00")
}

// ValidateProfile resolves every persisted selection against fresh,
// authenticated catalogs. Missing models are reported; no adapter is allowed
// to substitute or rewrite a selection.
func (r *Registry) ValidateProfile(ctx context.Context, value profile.Profile) error {
	catalogs := make(map[string]map[string]bool)
	for alias, selected := range value.Models {
		gatewayID := strings.ToLower(strings.TrimSpace(selected.Gateway))
		available := catalogs[gatewayID]
		if available == nil {
			catalog, err := r.Catalog(ctx, gatewayID)
			if err != nil {
				return fmt.Errorf("validate model %s: %w", alias, err)
			}
			available = make(map[string]bool, len(catalog))
			for _, item := range catalog {
				available[CatalogIdentity(item)] = true
			}
			catalogs[gatewayID] = available
		}
		if !available[profile.ModelIdentity(selected)] {
			return fmt.Errorf(
				"model %s selects %s/%s/%s, which is absent from the current authenticated catalog; ALT will not substitute it",
				alias, selected.Gateway, selected.Route, selected.Name,
			)
		}
	}
	return nil
}
