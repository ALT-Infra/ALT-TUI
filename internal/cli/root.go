package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"altv1/internal/application"
	"altv1/internal/buildinfo"
	"altv1/internal/tui"

	tea "charm.land/bubbletea/v2"
	"github.com/spf13/cobra"
)

type commandState struct {
	ctx                                  context.Context
	in                                   io.Reader
	out                                  io.Writer
	errOut                               io.Writer
	dataDir                              string
	dangerouslyBypassApprovalsAndSandbox bool
	workingDirectory                     string
}

func Execute(
	ctx context.Context,
	args []string,
	in io.Reader,
	out io.Writer,
	errOut io.Writer,
) error {
	state := &commandState{ctx: ctx, in: in, out: out, errOut: errOut}
	var showVersion bool
	root := &cobra.Command{
		Use:                   "alt [OPTIONS] [PROMPT]",
		Short:                 "Adaptive peer-and-specialist orchestration",
		DisableFlagsInUseLine: true,
		SilenceUsage:          true,
		SilenceErrors:         true,
		Args:                  cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if showVersion {
				fmt.Fprintln(out, buildinfo.Version)
				return nil
			}
			app, err := state.open()
			if err != nil {
				return err
			}
			defer app.Close()
			workspace, err := state.resolveWorkspace()
			if err != nil {
				return err
			}
			initialPrompt := ""
			if len(args) == 1 {
				initialPrompt = args[0]
			}
			_, err = tea.NewProgram(tui.NewWithOptions(ctx, app, tui.LaunchOptions{
				Workspace: workspace, InitialPrompt: initialPrompt,
			}), tea.WithContext(ctx)).
				Run()
			return err
		},
	}
	root.SetArgs(args)
	root.SetIn(in)
	root.SetOut(out)
	root.SetErr(errOut)
	root.PersistentFlags().StringVar(
		&state.dataDir,
		"data-dir",
		"",
		"ALT data directory (default: ALT_HOME or the XDG data directory)",
	)
	root.PersistentFlags().BoolVar(
		&state.dangerouslyBypassApprovalsAndSandbox,
		"dangerously-bypass-approvals-and-sandbox",
		false,
		"disable terminal sandboxing and any configured approval gates",
	)
	root.PersistentFlags().Lookup("dangerously-bypass-approvals-and-sandbox").NoOptDefVal = "true"
	root.PersistentFlags().BoolVar(
		&state.dangerouslyBypassApprovalsAndSandbox,
		"yolo",
		false,
		"alias for --dangerously-bypass-approvals-and-sandbox",
	)
	_ = root.PersistentFlags().MarkHidden("yolo")
	root.PersistentFlags().StringVarP(
		&state.workingDirectory,
		"cd", "C", ".",
		"tell ALT to use the specified directory as its working root",
	)
	root.Flags().BoolVarP(&showVersion, "version", "V", false, "print version")
	root.AddCommand(
		state.execCommand(),
		state.resumeCommand(),
		state.profileCommand(),
		state.sessionCommand(),
		state.authCommand(),
		state.researchCommand(),
		state.completionCommand(root),
		state.licensesCommand(),
		state.nativeGUICommand(),
		state.toolExecCommand(),
	)
	return root.ExecuteContext(ctx)
}

func (s *commandState) resolveWorkspace() (string, error) {
	value := strings.TrimSpace(s.workingDirectory)
	if value == "" {
		value = "."
	}
	resolved, err := filepath.Abs(value)
	if err != nil {
		return "", fmt.Errorf("resolve working directory: %w", err)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", fmt.Errorf("working directory: %w", err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("working directory is not a directory: %s", resolved)
	}
	return resolved, nil
}

func (s *commandState) open() (*application.Application, error) {
	dataDir, err := s.resolveDataDir()
	if err != nil {
		return nil, err
	}
	return application.OpenAtWithOptions(s.ctx, dataDir, application.Options{
		DangerouslyBypassApprovalsAndSandbox: s.dangerouslyBypassApprovalsAndSandbox,
	})
}

func (s *commandState) resolveDataDir() (string, error) {
	return application.ResolveDataDir(strings.TrimSpace(s.dataDir))
}

func (s *commandState) terminalInput() (*os.File, bool) {
	file, ok := s.in.(*os.File)
	if !ok {
		return nil, false
	}
	info, err := file.Stat()
	return file, err == nil && info.Mode()&os.ModeCharDevice != 0
}
