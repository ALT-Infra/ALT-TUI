package cli

import (
	"encoding/json"
	"fmt"

	"altv1/internal/nativegui"

	"github.com/spf13/cobra"
)

func (s *commandState) nativeGUICommand() *cobra.Command {
	var (
		mode      string
		profileID string
		revision  int
		sessionID string
	)
	command := &cobra.Command{
		Use:    "__native-gui",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			app, err := s.open()
			if err != nil {
				return err
			}
			defer app.Close()
			published, err := nativegui.Run(s.ctx, app, nativegui.Launch{
				Mode:      nativegui.Mode(mode),
				ProfileID: profileID,
				Revision:  revision,
				SessionID: sessionID,
			}, s.in)
			if err != nil {
				return err
			}
			result := struct {
				Published *nativegui.Published `json:"published,omitempty"`
			}{Published: published}
			if err := json.NewEncoder(s.out).Encode(result); err != nil {
				return fmt.Errorf("write native GUI result: %w", err)
			}
			return nil
		},
	}
	command.Flags().StringVar(&mode, "mode", "", "internal native GUI mode")
	command.Flags().StringVar(&profileID, "profile", "", "profile id")
	command.Flags().IntVar(&revision, "revision", 0, "profile revision")
	command.Flags().StringVar(&sessionID, "session", "", "session id")
	_ = command.MarkFlagRequired("mode")
	return command
}
