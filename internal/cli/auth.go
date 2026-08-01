package cli

import (
	"errors"
	"fmt"
	"io"
	"strings"

	"altv1/internal/application"
	"altv1/internal/credential"
	"altv1/internal/provider"
	"altv1/internal/research/exa"

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
		Short:   "Securely store a gateway or research API key",
		Example: "  alt auth set opencode\n  alt auth set exa",
		Args:    cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			connection, err := s.authConnection(args)
			if err != nil {
				return err
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
			connection, err := s.authConnection(args)
			if err != nil {
				return err
			}
			credentials, err := s.credentialStore()
			if err != nil {
				return err
			}
			_, source, err := credentials.Lookup(connection.ID, connection.CredentialEnvironment)
			if err != nil {
				if errors.Is(err, credential.ErrNotFound) {
					fmt.Fprintf(s.out, "%s: not configured\n", connection.ID)
					return nil
				}
				return err
			}
			fmt.Fprintf(s.out, "%s: configured (%s)\n", connection.ID, source)
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
		Short: "Authenticate with a live catalog or minimal Exa search",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			connection, err := s.authConnection(args)
			if err != nil {
				return err
			}
			if connection.ID == "exa" {
				credentials, err := s.credentialStore()
				if err != nil {
					return err
				}
				client := exa.Client{ResolveCredential: func() (string, error) {
					return credentials.Resolve("exa", "EXA_API_KEY")
				}}
				response, err := client.Search(s.ctx, exa.SearchRequest{
					Query: "Exa", NumResults: 1,
					Contents: map[string]any{"text": false},
				})
				if err != nil {
					return err
				}
				results, _ := response["results"].([]any)
				fmt.Fprintf(s.out, "exa: authenticated; live search returned %d result(s)\n", len(results))
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
}

func (s *commandState) authConnection(args []string) (authConnection, error) {
	dataDir, err := application.ResolveDataDir(strings.TrimSpace(s.dataDir))
	if err != nil {
		return authConnection{}, err
	}
	registry, err := application.NewGatewayRegistry(dataDir)
	if err != nil {
		return authConnection{}, err
	}
	available := []authConnection{{
		ID: "exa", Name: "Exa web research", CredentialEnvironment: "EXA_API_KEY",
	}}
	for _, descriptor := range registry.Descriptors() {
		available = append(available, authConnection{
			ID: descriptor.ID, Name: descriptor.Name,
			CredentialEnvironment: descriptor.CredentialEnvironment,
		})
	}
	if len(args) != 1 {
		names := make([]string, 0, len(available))
		for _, item := range available {
			names = append(names, item.ID)
		}
		return authConnection{}, fmt.Errorf(
			"connection name is required; registered connections: %s",
			strings.Join(names, ", "),
		)
	}
	name := strings.ToLower(strings.TrimSpace(args[0]))
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
