package cli

import (
	"fmt"

	"github.com/spf13/cobra"
)

func (s *commandState) completionCommand(root *cobra.Command) *cobra.Command {
	return &cobra.Command{
		Use:       "completion [bash|zsh|fish|powershell]",
		Short:     "Generate shell completion scripts",
		Args:      cobra.MaximumNArgs(1),
		ValidArgs: []string{"bash", "zsh", "fish", "powershell"},
		RunE: func(_ *cobra.Command, args []string) error {
			shell := "bash"
			if len(args) == 1 {
				shell = args[0]
			}
			switch shell {
			case "bash":
				return root.GenBashCompletion(s.out)
			case "zsh":
				return root.GenZshCompletion(s.out)
			case "fish":
				return root.GenFishCompletion(s.out, true)
			case "powershell":
				return root.GenPowerShellCompletion(s.out)
			default:
				return fmt.Errorf("unsupported shell %q", shell)
			}
		},
	}
}
