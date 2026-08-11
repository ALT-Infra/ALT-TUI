package cli

import (
	"errors"
	"fmt"
	"io"
	"os/exec"
	"runtime"
	"strings"

	"altv1/internal/application"
	"altv1/internal/credential"
	"altv1/internal/provider"
	"altv1/internal/research"
	"altv1/internal/research/exa"
	"altv1/internal/research/linkup"

	"github.com/spf13/cobra"
	"golang.org/x/term"
)

func (s *commandState) authCommand() *cobra.Command {
	command := &cobra.Command{
		Use:   "auth",
		Short: "Manage gateway and research-service credentials",
	}
	command.AddCommand(
		s.authSetCommand(),
		s.authStatusCommand(),
		s.authModelsCommand(),
		s.authRemoveCommand(),
		s.authTestCommand(),
	)
	return command
}

func (s *commandState) authSetCommand() *cobra.Command {
	return &cobra.Command{
		Use:     "set <connection>",
		Short:   "Configure a gateway or research connection",
		Example: "  alt auth set opencode\n  alt auth set cline\n  alt auth set exa\n  alt auth set linkup",
		Args:    cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			connection, err := s.authConnection(args)
			if err != nil {
				return err
			}
			if connection.Authentication == provider.AuthenticationDeviceOAuth {
				return s.authenticateDevice(connection)
			}
			credentials, err := s.credentialStore()
			if err != nil {
				return err
			}
			var secret []byte
			if file, terminal := s.terminalInput(); terminal {
				fmt.Fprintf(s.errOut, "%s API key (input hidden): ", connection.Name)
				secret, err = term.ReadPassword(int(file.Fd()))
				fmt.Fprintln(s.errOut)
			} else {
				secret, err = io.ReadAll(s.in)
			}
			if err != nil {
				return fmt.Errorf("read API key: %w", err)
			}
			source, err := credentials.Set(connection.ID, strings.TrimSpace(string(secret)))
			if err != nil {
				return err
			}
			fmt.Fprintf(s.out, "stored %s credential (%s)\n", connection.ID, source)
			if source == credential.SourcePrivateFile {
				fmt.Fprintln(s.out, "OS keyring unavailable; fallback file permissions are 0600")
			}
			return nil
		},
	}
}

func (s *commandState) authStatusCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "status [connection]",
		Short: "Check whether a connection credential is available",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			available, err := s.authConnections()
			if err != nil {
				return err
			}
			credentials, err := s.credentialStore()
			if err != nil {
				return err
			}
			connections := available
			if len(args) == 1 {
				connection, err := findAuthConnection(available, args[0])
				if err != nil {
					return err
				}
				connections = []authConnection{connection}
			}
			for _, connection := range connections {
				_, source, lookupErr := credentials.Lookup(connection.ID, connection.CredentialEnvironment)
				if errors.Is(lookupErr, credential.ErrInvalid) {
					fmt.Fprintf(s.out, "%s: invalid (%s)\n", connection.ID, lookupErr)
					continue
				}
				if errors.Is(lookupErr, credential.ErrNotFound) {
					fmt.Fprintf(s.out, "%s: not configured\n", connection.ID)
					continue
				}
				if lookupErr != nil {
					return lookupErr
				}
				fmt.Fprintf(s.out, "%s: configured (%s)\n", connection.ID, source)
			}
			return nil
		},
	}
}

func (s *commandState) authRemoveCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "remove [connection]",
		Short: "Remove a stored connection credential",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			connection, err := s.authConnection(args)
			if err != nil {
				return err
			}
			credentials, err := s.credentialStore()
			if err != nil {
				return err
			}
			if err := credentials.Delete(connection.ID); err != nil {
				return err
			}
			fmt.Fprintf(s.out, "removed stored %s credential\n", connection.ID)
			return nil
		},
	}
}

func (s *commandState) authModelsCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "models [gateway]",
		Short: "List the exact models available to the configured credential",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			gateway, err := s.gatewayDescriptor(args)
			if err != nil {
				return err
			}
			app, err := s.open()
			if err != nil {
				return err
			}
			defer app.Close()
			catalog, err := app.Providers.Catalog(s.ctx, gateway.ID)
			if err != nil {
				return err
			}
			fmt.Fprintln(s.out, "MODEL\tROUTE")
			for _, item := range catalog {
				fmt.Fprintf(s.out, "%s\t%s\n", item.ID, item.Route)
			}
			return nil
		},
	}
}

func (s *commandState) credentialStore() (credential.Store, error) {
	dataDir, err := application.ResolveDataDir(strings.TrimSpace(s.dataDir))
	if err != nil {
		return credential.Store{}, err
	}
	return credential.NewStore(dataDir), nil
}

func (s *commandState) authTestCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "test <connection>",
		Short: "Authenticate with a live catalog or minimal research search",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			connection, err := s.authConnection(args)
			if err != nil {
				return err
			}
			if connection.ID == "exa" || connection.ID == "linkup" {
				credentials, err := s.credentialStore()
				if err != nil {
					return err
				}
				var response map[string]any
				switch connection.ID {
				case "exa":
					client := exa.Client{ResolveCredential: func() (string, error) {
						return credentials.Resolve("exa", "EXA_API_KEY")
					}}
					response, err = client.Search(s.ctx, exa.SearchRequest{
						Query: "Exa", NumResults: 1,
					})
				case "linkup":
					client := linkup.Client{ResolveCredential: func() (string, error) {
						return credentials.Resolve("linkup", "LINKUP_API_KEY")
					}}
					response, err = client.Search(s.ctx, linkup.SearchRequest{
						Query: "Linkup", Depth: "fast", OutputType: "searchResults", MaxResults: 1,
					})
				}
				if err != nil {
					return err
				}
				count := researchResultCount(response)
				fmt.Fprintf(s.out, "%s: authenticated; live search returned %d result(s)\n", connection.ID, count)
				return nil
			}
			app, err := s.open()
			if err != nil {
				return err
			}
			defer app.Close()
			catalog, err := app.Providers.Catalog(s.ctx, connection.ID)
			if err != nil {
				return err
			}
			if len(catalog) == 0 {
				return fmt.Errorf("%s authenticated successfully but returned an empty model catalog", connection.ID)
			}
			fmt.Fprintf(s.out, "%s: authenticated; discovered %d exact model choices\n", connection.ID, len(catalog))
			return nil
		},
	}
}

type authConnection struct {
	ID                    string
	Name                  string
	CredentialEnvironment string
	Authentication        provider.AuthenticationKind
}

func (s *commandState) authConnection(args []string) (authConnection, error) {
	available, err := s.authConnections()
	if err != nil {
		return authConnection{}, err
	}
	if len(args) != 1 {
		return authConnection{}, connectionRequiredError(available)
	}
	return findAuthConnection(available, args[0])
}

func (s *commandState) authConnections() ([]authConnection, error) {
	dataDir, err := application.ResolveDataDir(strings.TrimSpace(s.dataDir))
	if err != nil {
		return nil, err
	}
	registry, err := application.NewGatewayRegistry(dataDir)
	if err != nil {
		return nil, err
	}
	available := make([]authConnection, 0, len(research.Connections())+len(registry.Descriptors()))
	for _, connection := range research.Connections() {
		available = append(available, authConnection{
			ID: string(connection.ID), Name: connection.Name + " web research",
			CredentialEnvironment: connection.CredentialEnvironment,
			Authentication:        provider.AuthenticationAPIKey,
		})
	}
	for _, descriptor := range registry.Descriptors() {
		available = append(available, authConnection{
			ID: descriptor.ID, Name: descriptor.Name,
			CredentialEnvironment: descriptor.CredentialEnvironment,
			Authentication:        descriptor.Authentication,
		})
	}
	return available, nil
}

func researchResultCount(response map[string]any) int {
	if values, ok := response["results"].([]any); ok {
		return len(values)
	}
	if data, ok := response["data"].(map[string]any); ok {
		if values, ok := data["results"].([]any); ok {
			return len(values)
		}
	}
	return 0
}

func findAuthConnection(available []authConnection, raw string) (authConnection, error) {
	name := strings.ToLower(strings.TrimSpace(raw))
	for _, item := range available {
		if item.ID == name {
			return item, nil
		}
	}
	names := make([]string, 0, len(available))
	for _, item := range available {
		names = append(names, item.ID)
	}
	return authConnection{}, fmt.Errorf(
		"unsupported connection; registered connections: %s",
		strings.Join(names, ", "),
	)
}

func connectionRequiredError(available []authConnection) error {
	names := make([]string, 0, len(available))
	for _, item := range available {
		names = append(names, item.ID)
	}
	return fmt.Errorf("connection name is required; registered connections: %s", strings.Join(names, ", "))
}

func (s *commandState) authenticateDevice(connection authConnection) error {
	dataDir, err := application.ResolveDataDir(strings.TrimSpace(s.dataDir))
	if err != nil {
		return err
	}
	registry, err := application.NewGatewayRegistry(dataDir)
	if err != nil {
		return err
	}
	authenticator, err := registry.DeviceAuthenticator(connection.ID)
	if err != nil {
		return err
	}
	authorization, err := authenticator.BeginDeviceAuthorization(s.ctx)
	if err != nil {
		return err
	}
	verificationURL := authorization.VerificationURIComplete
	if strings.TrimSpace(verificationURL) == "" {
		verificationURL = authorization.VerificationURI
	}
	fmt.Fprintf(s.out, "Open %s\n", verificationURL)
	fmt.Fprintf(s.out, "%s confirmation code: %s\n", connection.Name, authorization.UserCode)
	if err := openBrowser(verificationURL); err != nil {
		fmt.Fprintln(s.out, "Could not open a browser automatically; use the link above.")
	}
	lastProgress := ""
	err = authenticator.CompleteDeviceAuthorization(s.ctx, authorization, func(message string) {
		if message != lastProgress {
			fmt.Fprintln(s.out, message+"…")
			lastProgress = message
		}
	})
	if err != nil {
		return err
	}
	fmt.Fprintf(s.out, "signed in to %s; rotating account credentials stored securely\n", connection.Name)
	return nil
}

func openBrowser(rawURL string) error {
	var name string
	var args []string
	switch runtime.GOOS {
	case "darwin":
		name, args = "open", []string{rawURL}
	case "windows":
		name, args = "rundll32", []string{"url.dll,FileProtocolHandler", rawURL}
	default:
		name, args = "xdg-open", []string{rawURL}
	}
	return exec.Command(name, args...).Start()
}

func (s *commandState) gatewayDescriptor(args []string) (provider.GatewayDescriptor, error) {
	dataDir, err := application.ResolveDataDir(strings.TrimSpace(s.dataDir))
	if err != nil {
		return provider.GatewayDescriptor{}, err
	}
	registry, err := application.NewGatewayRegistry(dataDir)
	if err != nil {
		return provider.GatewayDescriptor{}, err
	}
	available := registry.Descriptors()
	name := ""
	if len(args) == 1 {
		name = strings.ToLower(strings.TrimSpace(args[0]))
	} else if len(available) == 1 {
		name = available[0].ID
	} else {
		names := make([]string, 0, len(available))
		for _, item := range available {
			names = append(names, item.ID)
		}
		return provider.GatewayDescriptor{}, fmt.Errorf(
			"gateway name is required; registered gateways: %s",
			strings.Join(names, ", "),
		)
	}
	descriptor, err := registry.Descriptor(name)
	if err != nil {
		names := make([]string, 0, len(available))
		for _, item := range available {
			names = append(names, item.ID)
		}
		// Do not echo an unrecognized positional argument. Besides avoiding
		// dependence on guessed API-key formats, this keeps an accidentally
		// pasted credential out of terminal output and logs.
		return provider.GatewayDescriptor{}, fmt.Errorf(
			"unsupported gateway; registered gateways: %s",
			strings.Join(names, ", "),
		)
	}
	return descriptor, nil
}
