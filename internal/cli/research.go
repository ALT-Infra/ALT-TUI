package cli

import (
	"fmt"

	"github.com/spf13/cobra"
)

func (s *commandState) researchCommand() *cobra.Command {
	command := &cobra.Command{
		Use:   "research",
		Short: "Choose the web research connection",
	}
	command.AddCommand(s.researchStatusCommand(), s.researchSetCommand())
	return command
}

func (s *commandState) researchStatusCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show research connections and the active choice",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			app, err := s.open()
			if err != nil {
				return err
			}
			defer app.Close()
			connections, err := app.ResearchConnections(s.ctx)
			if err != nil {
				return err
			}
			for _, connection := range connections {
				state := "not configured"
				if connection.Configured {
					state = "ready"
				}
				if connection.Selected {
					state += ", current"
				}
				fmt.Fprintf(s.out, "%s: %s\n", connection.ID, state)
			}
			return nil
		},
	}
}

func (s *commandState) researchSetCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "set <exa|linkup>",
		Short: "Select one configured research connection",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			app, err := s.open()
			if err != nil {
				return err
			}
			defer app.Close()
			if err := app.SelectResearchProvider(s.ctx, args[0]); err != nil {
				return err
			}
			fmt.Fprintf(s.out, "%s research selected\n", app.RuntimePolicy.ResearchProvider)
			return nil
		},
	}
}
