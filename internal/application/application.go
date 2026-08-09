package application

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"altv1/internal/credential"
	"altv1/internal/orchestrator"
	"altv1/internal/provider"
	"altv1/internal/provider/cline"
	"altv1/internal/provider/fireworks"
	"altv1/internal/provider/opencode"
	"altv1/internal/provider/together"
	"altv1/internal/provider/zenmux"
	"altv1/internal/research"
	"altv1/internal/store"
	"altv1/internal/tooling"
)

type Application struct {
	DataDir       string
	Store         *store.Store
	Providers     *provider.Registry
	Engine        *orchestrator.Engine
	RuntimePolicy RuntimePolicy
	credentials   credential.Store
}

type Options struct {
	DangerouslyBypassApprovalsAndSandbox bool
}

type RuntimePolicy struct {
	DangerouslyBypassApprovalsAndSandbox bool     `json:"dangerously_bypass_approvals_and_sandbox"`
	FilesystemConfinement                bool     `json:"filesystem_confinement"`
	DirectTerminalNetwork                bool     `json:"direct_terminal_network"`
	ExaConfigured                        bool     `json:"exa_configured"`
	LinkupConfigured                     bool     `json:"linkup_configured"`
	ResearchProvider                     string   `json:"research_provider,omitempty"`
	Tools                                []string `json:"tools"`
}

type ResearchConnectionStatus struct {
	ID         string
	Name       string
	Configured bool
	Selected   bool
}

const researchProviderSetting = "research.provider"

func Open(ctx context.Context) (*Application, error) {
	dataDir, err := ResolveDataDir("")
	if err != nil {
		return nil, err
	}
	return OpenAt(ctx, dataDir)
}

func OpenAt(ctx context.Context, dataDir string) (*Application, error) {
	return OpenAtWithOptions(ctx, dataDir, Options{})
}

func OpenAtWithOptions(ctx context.Context, dataDir string, options Options) (*Application, error) {
	dataDir, err := filepath.Abs(dataDir)
	if err != nil {
		return nil, fmt.Errorf("resolve data directory: %w", err)
	}
	ledger, err := store.Open(ctx, filepath.Join(dataDir, "alt.db"))
	if err != nil {
		return nil, err
	}
	registry, err := NewGatewayRegistry(dataDir)
	if err != nil {
		ledger.Close()
		return nil, err
	}
	credentials := credential.NewStore(dataDir)
	sensitiveEnvironment := []string{"EXA_API_KEY", "LINKUP_API_KEY"}
	for _, descriptor := range registry.Descriptors() {
		if descriptor.CredentialEnvironment != "" {
			sensitiveEnvironment = append(sensitiveEnvironment, descriptor.CredentialEnvironment)
		}
	}
	runtimePolicy := RuntimePolicy{
		DangerouslyBypassApprovalsAndSandbox: options.DangerouslyBypassApprovalsAndSandbox,
		FilesystemConfinement:                !options.DangerouslyBypassApprovalsAndSandbox,
		DirectTerminalNetwork:                options.DangerouslyBypassApprovalsAndSandbox,
		Tools:                                tooling.ToolNames(),
	}
	for _, connection := range research.Connections() {
		_, _, lookupErr := credentials.Lookup(string(connection.ID), connection.CredentialEnvironment)
		configured := lookupErr == nil
		if lookupErr != nil && !errors.Is(lookupErr, credential.ErrNotFound) {
			ledger.Close()
			return nil, fmt.Errorf("inspect %s credential: %w", connection.Name, lookupErr)
		}
		switch connection.ID {
		case research.ProviderExa:
			runtimePolicy.ExaConfigured = configured
		case research.ProviderLinkup:
			runtimePolicy.LinkupConfigured = configured
		}
	}
	var app *Application
	app = &Application{
		DataDir:       dataDir,
		Store:         ledger,
		Providers:     registry,
		RuntimePolicy: runtimePolicy,
		credentials:   credentials,
		Engine: orchestrator.NewEngineWithOptions(
			ledger,
			registry,
			orchestrator.EngineOptions{
				DangerouslyBypassApprovalsAndSandbox: options.DangerouslyBypassApprovalsAndSandbox,
				SensitiveEnvironment:                 sensitiveEnvironment,
				ResolveResearchProvider:              func(callCtx context.Context) (string, error) { return app.ResolveResearchProvider(callCtx) },
				ResolveExaCredential:                 func() (string, error) { return credentials.Resolve("exa", "EXA_API_KEY") },
				ResolveLinkupCredential:              func() (string, error) { return credentials.Resolve("linkup", "LINKUP_API_KEY") },
			},
		),
	}
	if selected, err := app.resolveResearchProvider(ctx, false); err == nil {
		app.RuntimePolicy.ResearchProvider = string(selected)
	}
	return app, nil
}

func (a *Application) ResearchConnections(ctx context.Context) ([]ResearchConnectionStatus, error) {
	if a == nil || a.Store == nil {
		return nil, fmt.Errorf("application is required")
	}
	selected, _ := a.resolveResearchProvider(ctx, false)
	result := make([]ResearchConnectionStatus, 0, len(research.Connections()))
	for _, connection := range research.Connections() {
		_, _, err := a.credentials.Lookup(string(connection.ID), connection.CredentialEnvironment)
		configured := err == nil
		if err != nil && !errors.Is(err, credential.ErrNotFound) {
			return nil, fmt.Errorf("inspect %s credential: %w", connection.Name, err)
		}
		result = append(result, ResearchConnectionStatus{
			ID: string(connection.ID), Name: connection.Name,
			Configured: configured, Selected: connection.ID == selected,
		})
	}
	return result, nil
}

func (a *Application) SelectResearchProvider(ctx context.Context, raw string) error {
	selected, err := research.ParseProvider(raw)
	if err != nil {
		return err
	}
	connection, _ := research.Lookup(selected)
	if _, _, err := a.credentials.Lookup(string(selected), connection.CredentialEnvironment); err != nil {
		if errors.Is(err, credential.ErrNotFound) {
			return fmt.Errorf("%s is not configured; run `alt auth set %s`", connection.Name, selected)
		}
		return fmt.Errorf("inspect %s credential: %w", connection.Name, err)
	}
	if err := a.Store.SetSetting(ctx, researchProviderSetting, string(selected)); err != nil {
		return err
	}
	switch selected {
	case research.ProviderExa:
		a.RuntimePolicy.ExaConfigured = true
	case research.ProviderLinkup:
		a.RuntimePolicy.LinkupConfigured = true
	}
	a.RuntimePolicy.ResearchProvider = string(selected)
	return nil
}

func (a *Application) ResolveResearchProvider(ctx context.Context) (string, error) {
	selected, err := a.resolveResearchProvider(ctx, true)
	return string(selected), err
}

func (a *Application) resolveResearchProvider(ctx context.Context, require bool) (research.Provider, error) {
	raw, found, err := a.Store.Setting(ctx, researchProviderSetting)
	if err != nil {
		return "", err
	}
	configured := make([]research.Provider, 0, len(research.Connections()))
	for _, connection := range research.Connections() {
		_, _, lookupErr := a.credentials.Lookup(string(connection.ID), connection.CredentialEnvironment)
		if lookupErr == nil {
			configured = append(configured, connection.ID)
			continue
		}
		if !errors.Is(lookupErr, credential.ErrNotFound) {
			return "", fmt.Errorf("inspect %s credential: %w", connection.Name, lookupErr)
		}
	}
	if found {
		selected, parseErr := research.ParseProvider(raw)
		if parseErr != nil {
			return "", fmt.Errorf("stored research provider is invalid: %w", parseErr)
		}
		for _, available := range configured {
			if selected == available {
				return selected, nil
			}
		}
		connection, _ := research.Lookup(selected)
		return "", fmt.Errorf("selected research provider %s is not configured; run `alt auth set %s` or choose /research", connection.Name, selected)
	}
	if len(configured) == 1 {
		return configured[0], nil
	}
	if !require {
		return "", nil
	}
	if len(configured) == 0 {
		return "", fmt.Errorf("web research is not configured; run `alt auth set exa` or `alt auth set linkup`")
	}
	return "", fmt.Errorf("choose the active research provider with /research")
}

// NewGatewayRegistry is the single integration composition point. Adding a
// gateway means registering its adapter here; consumers discover all metadata
// through descriptors and contain no gateway-specific branches.
func NewGatewayRegistry(dataDir string) (*provider.Registry, error) {
	registry := provider.NewRegistry()
	credentials := credential.NewStore(dataDir)
	gateways := []provider.Gateway{
		cline.NewFactory(credentials),
		fireworks.NewFactory(credentials),
		opencode.NewFactory(credentials),
		together.NewFactory(credentials),
		zenmux.NewFactory(credentials),
	}
	for _, gateway := range gateways {
		if err := registry.Register(gateway); err != nil {
			return nil, err
		}
	}
	return registry, nil
}

func (a *Application) Close() error {
	return a.Store.Close()
}

func ResolveDataDir(explicit string) (string, error) {
	if strings.TrimSpace(explicit) != "" {
		return filepath.Abs(explicit)
	}
	if value := os.Getenv("ALT_HOME"); value != "" {
		return filepath.Abs(value)
	}
	if value := os.Getenv("XDG_DATA_HOME"); value != "" {
		return filepath.Join(value, "alt-v1"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	return filepath.Join(home, ".local", "share", "alt-v1"), nil
}
