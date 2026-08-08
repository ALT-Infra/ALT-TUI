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
	"altv1/internal/store"
	"altv1/internal/tooling"
)

type Application struct {
	DataDir       string
	Store         *store.Store
	Providers     *provider.Registry
	Engine        *orchestrator.Engine
	RuntimePolicy RuntimePolicy
}

type Options struct {
	DangerouslyBypassApprovalsAndSandbox bool
}

type RuntimePolicy struct {
	DangerouslyBypassApprovalsAndSandbox bool     `json:"dangerously_bypass_approvals_and_sandbox"`
	FilesystemConfinement                bool     `json:"filesystem_confinement"`
	DirectTerminalNetwork                bool     `json:"direct_terminal_network"`
	ExaConfigured                        bool     `json:"exa_configured"`
	Tools                                []string `json:"tools"`
}

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
	sensitiveEnvironment := []string{"EXA_API_KEY"}
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
	if _, _, lookupErr := credentials.Lookup("exa", "EXA_API_KEY"); lookupErr == nil {
		runtimePolicy.ExaConfigured = true
	} else if !errors.Is(lookupErr, credential.ErrNotFound) {
		ledger.Close()
		return nil, fmt.Errorf("inspect Exa credential: %w", lookupErr)
	}
	return &Application{
		DataDir:       dataDir,
		Store:         ledger,
		Providers:     registry,
		RuntimePolicy: runtimePolicy,
		Engine: orchestrator.NewEngineWithOptions(
			ledger,
			registry,
			orchestrator.EngineOptions{
				DangerouslyBypassApprovalsAndSandbox: options.DangerouslyBypassApprovalsAndSandbox,
				SensitiveEnvironment:                 sensitiveEnvironment,
				ResolveExaCredential: func() (string, error) {
					return credentials.Resolve("exa", "EXA_API_KEY")
				},
			},
		),
	}, nil
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
