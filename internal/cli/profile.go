package cli

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"altv1/internal/profile"

	"github.com/spf13/cobra"
)

func (s *commandState) profileCommand() *cobra.Command {
	command := &cobra.Command{
		Use:   "profile",
		Short: "Create and manage immutable Team Profile revisions",
	}
	command.AddCommand(
		s.profileListCommand(),
		s.profileShowCommand(),
		s.profileValidateCommand(),
		s.profileImportCommand(),
	)
	return command
}

func (s *commandState) profileListCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List Team Profile revisions",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			app, err := s.open()
			if err != nil {
				return err
			}
			defer app.Close()
			items, err := app.Store.ListProfiles(s.ctx)
			if err != nil {
				return err
			}
			fmt.Fprintln(s.out, "PROFILE\tREVISION\tNAME")
			for _, item := range items {
				fmt.Fprintf(s.out, "%s\t%d\t%s\n", item.ID, item.Revision, item.Name)
			}
			return nil
		},
	}
}

func (s *commandState) profileShowCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "show <id[@revision]>",
		Short: "Print a Team Profile",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			app, err := s.open()
			if err != nil {
				return err
			}
			defer app.Close()
			document, err := resolveProfile(s.ctx, app, args[0])
			if err != nil {
				return err
			}
			_, err = s.out.Write(document.Source)
			return err
		},
	}
}

func (s *commandState) profileValidateCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "validate <file>",
		Short: "Strictly parse and lint a Team Profile",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			document, err := profile.LoadFile(args[0])
			if err != nil {
				return err
			}
			diagnostics := profile.Validate(document.Profile)
			printDiagnostics(s.out, diagnostics)
			if profile.HasErrors(diagnostics) {
				return fmt.Errorf("profile is invalid")
			}
			fmt.Fprintf(s.out, "valid %s@%d · %s\n",
				document.Profile.ID, document.Profile.Revision, document.Digest[:12])
			return nil
		},
	}
}

func (s *commandState) profileImportCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "import <file>",
		Short: "Import an immutable Team Profile revision",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			document, err := profile.LoadFile(args[0])
			if err != nil {
				return err
			}
			diagnostics := profile.Validate(document.Profile)
			printDiagnostics(s.errOut, diagnostics)
			if profile.HasErrors(diagnostics) {
				return fmt.Errorf("profile is invalid")
			}
			app, err := s.open()
			if err != nil {
				return err
			}
			defer app.Close()
			if err := app.Store.ImportProfile(s.ctx, document); err != nil {
				return err
			}
			path, err := persistManagedProfile(app.DataDir, document)
			if err != nil {
				return err
			}
			fmt.Fprintf(s.out, "imported %s@%d · %s\n",
				document.Profile.ID, document.Profile.Revision, path)
			return nil
		},
	}
}

func printDiagnostics(writer io.Writer, diagnostics []profile.Diagnostic) {
	for _, diagnostic := range diagnostics {
		fmt.Fprintf(writer, "%s\t%s\t%s\n",
			strings.ToUpper(string(diagnostic.Severity)), diagnostic.Path, diagnostic.Message)
	}
}

func persistManagedProfile(dataDir string, document *profile.Document) (string, error) {
	directory := filepath.Join(dataDir, "profiles")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return "", fmt.Errorf("create profiles directory: %w", err)
	}
	path := filepath.Join(directory,
		fmt.Sprintf("%s-v%d.yaml", document.Profile.ID, document.Profile.Revision))
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if os.IsExist(err) {
			existing, readErr := profile.LoadFile(path)
			if readErr == nil && existing.Digest == document.Digest {
				return path, nil
			}
		}
		return "", fmt.Errorf("persist managed profile: %w", err)
	}
	if _, err := file.Write(document.Source); err != nil {
		file.Close()
		return "", err
	}
	if err := file.Close(); err != nil {
		return "", err
	}
	return path, nil
}
