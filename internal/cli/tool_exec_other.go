//go:build !linux

package cli

import (
	"fmt"

	"github.com/spf13/cobra"
)

func (s *commandState) toolExecCommand() *cobra.Command {
	return &cobra.Command{
		Use:    "__tool-exec",
		Hidden: true,
		RunE: func(_ *cobra.Command, _ []string) error {
			return fmt.Errorf("confined command execution is only available on Linux")
		},
	}
}
