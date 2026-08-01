package cli

import (
	"fmt"
	"strings"

	"altv1/internal/tui"

	tea "charm.land/bubbletea/v2"
	"github.com/spf13/cobra"
)

func (s *commandState) resumeCommand() *cobra.Command {
	var last bool
	command := &cobra.Command{
		Use:   "resume [SESSION_ID] [PROMPT]",
		Short: "Resume a previous interactive session",
		Args:  cobra.MaximumNArgs(2),
		RunE: func(_ *cobra.Command, args []string) error {
			if last && len(args) > 1 {
				return fmt.Errorf("--last accepts at most one PROMPT and cannot be combined with SESSION_ID")
			}
			app, err := s.open()
			if err != nil {
				return err
			}
			defer app.Close()

			workspace, err := s.resolveWorkspace()
			if err != nil {
				return err
			}
			reference := ""
			prompt := ""
			if last {
				items, err := app.Store.ListSessions(s.ctx, 1)
				if err != nil {
					return err
				}
				if len(items) == 0 {
					return fmt.Errorf("no saved sessions")
				}
				reference = items[0].ConversationID
				if reference == "" {
					reference = items[0].ID
				}
				if len(args) == 1 {
					prompt = args[0]
				}
			} else {
				if len(args) >= 1 {
					reference = args[0]
				}
				if len(args) == 2 {
					prompt = args[1]
				}
			}

			options := tui.LaunchOptions{
				Workspace: workspace, InitialPrompt: strings.TrimSpace(prompt),
				ResumePicker: reference == "", ResumeSession: reference,
			}
			_, err = tea.NewProgram(tui.NewWithOptions(s.ctx, app, options), tea.WithContext(s.ctx)).Run()
			return err
		},
	}
	command.Flags().BoolVar(&last, "last", false, "continue the most recent session without showing the picker")
	return command
}
