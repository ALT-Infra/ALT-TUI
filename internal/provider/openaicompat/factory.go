package openaicompat

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"

	"altv1/internal/credential"
	"altv1/internal/profile"
	"altv1/internal/provider"

	einoopenai "github.com/cloudwego/eino-ext/components/model/openai"
	"github.com/cloudwego/eino/components/model"
)

// Config contains only facts owned by a gateway adapter. Model IDs remain
// opaque catalog values and are never normalized by this shared transport.
type Config struct {
	Descriptor provider.GatewayDescriptor
	Route      string
	BaseURL    string
	Hostname   string
}

// Factory implements the common authenticated OpenAI-compatible execution
// path. A concrete adapter embeds it and owns its catalog contract.
type Factory struct {
	Credentials credential.Store
	HTTPClient  *http.Client
	config      Config
}

func NewFactory(credentials credential.Store, config Config) *Factory {
	return &Factory{
		Credentials: credentials,
		HTTPClient:  &http.Client{Transport: http.DefaultTransport},
		config:      config,
	}
}

func (f *Factory) Descriptor() provider.GatewayDescriptor {
	return f.config.Descriptor
}

func (*Factory) Capabilities(profile.Model) provider.Capabilities {
	return provider.Capabilities{
		StructuredOutput: provider.CapabilityUnknown,
		ToolCalling:      provider.CapabilityUnknown,
	}
}

func (f *Factory) NewChatModel(
	ctx context.Context,
	spec profile.Model,
	mode provider.Mode,
) (model.BaseChatModel, error) {
	if strings.TrimSpace(spec.Route) != f.config.Route {
		return nil, fmt.Errorf(
			"unknown %s catalog route %q",
			f.config.Descriptor.Name,
			spec.Route,
		)
	}
	if err := ValidateEndpoint(
		f.config.Descriptor.Name,
		f.config.BaseURL,
		f.config.Hostname,
	); err != nil {
		return nil, err
	}
	key, err := f.ResolveCredential()
	if err != nil {
		return nil, err
	}
	config := &einoopenai.ChatModelConfig{
		APIKey:     key,
		BaseURL:    f.config.BaseURL,
		Model:      spec.Name,
		HTTPClient: f.HTTPClient,
	}
	// A generic OpenAI wire format does not prove that a particular model
	// implements native structured output. ALT requests JSON in the prompt and
	// validates the response itself unless authenticated catalog evidence says
	// more.
	_ = mode
	if spec.ReasoningEffort != "" {
		config.ExtraFields = map[string]any{
			"reasoning_effort": spec.ReasoningEffort,
		}
	}
	return einoopenai.NewChatModel(ctx, config)
}

func (f *Factory) ResolveCredential() (string, error) {
	return f.Credentials.Resolve(
		f.config.Descriptor.ID,
		f.config.Descriptor.CredentialEnvironment,
	)
}

// GetJSON performs an authenticated catalog request without making an
// inference call. The adapter supplies the exact documented URL.
func (f *Factory) GetJSON(
	ctx context.Context,
	rawURL string,
	target any,
) error {
	if err := ValidateEndpoint(
		f.config.Descriptor.Name,
		rawURL,
		f.config.Hostname,
	); err != nil {
		return err
	}
	key, err := f.ResolveCredential()
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+key)
	request.Header.Set("Accept", "application/json")
	response, err := f.HTTPClient.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("HTTP %d", response.StatusCode)
	}
	if err := json.NewDecoder(response.Body).Decode(target); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}

func ValidateEndpoint(gatewayName, raw, hostname string) error {
	parsed, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("parse %s endpoint: %w", gatewayName, err)
	}
	if os.Getenv("ALT_ALLOW_INSECURE_PROVIDER_ENDPOINT") == "1" {
		if parsed.Scheme != "http" && parsed.Scheme != "https" {
			return fmt.Errorf("%s endpoint must use HTTP or HTTPS", gatewayName)
		}
		return nil
	}
	if parsed.Scheme != "https" {
		return fmt.Errorf("%s endpoint must use HTTPS", gatewayName)
	}
	if parsed.Hostname() != hostname {
		return fmt.Errorf(
			"refusing to send a %s credential to %s",
			gatewayName,
			parsed.Hostname(),
		)
	}
	return nil
}
