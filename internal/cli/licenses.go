package cli

import (
	"fmt"

	"altv1/internal/licenses"

	"github.com/spf13/cobra"
)

func (s *commandState) licensesCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "licenses",
		Short: "Print third-party software notices embedded in this executable",
		Args:  cobra.NoArgs,
		Run: func(_ *cobra.Command, _ []string) {
			fmt.Fprint(s.out, licenses.Text())
		},
	}
}
