package cli

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"

	"altv1/internal/application"
	"altv1/internal/event"
	"altv1/internal/profile"

	"github.com/spf13/cobra"
)

func (s *commandState) execCommand() *cobra.Command {
	var profileReference string
	var quiet bool
	command := &cobra.Command{
		Use:     "exec [PROMPT]",
		Aliases: []string{"e"},
		Short:   "Run ALT non-interactively",
		Args:    cobra.ArbitraryArgs,
		RunE: func(_ *cobra.Command, args []string) error {
			task := strings.TrimSpace(strings.Join(args, " "))
			if task == "" {
				source, err := io.ReadAll(s.in)
				if err != nil {
					return fmt.Errorf("read task: %w", err)
				}
				task = strings.TrimSpace(string(source))
			}
			if task == "" {
				return fmt.Errorf("task is required")
			}
			app, err := s.open()
			if err != nil {
				return err
			}
			defer app.Close()
			document, err := resolveProfile(s.ctx, app, profileReference)
			if err != nil {
				return err
			}
			workspace, err := s.resolveWorkspace()
			if err != nil {
				return err
			}
			run, err := app.Engine.StartAt(s.ctx, document, task, workspace)
			if err != nil {
				return err
			}
			if !quiet {
				fmt.Fprintf(s.errOut, "session %s · profile %s@%d\n",
					run.SessionID, document.Profile.ID, document.Profile.Revision)
			}
			events, unsubscribe, err := app.Store.Subscribe(s.ctx, run.SessionID, 0)
			if err != nil {
				return err
			}
			defer unsubscribe()
			for item := range events {
				switch item.Kind {
				case event.LeadSelected:
					if !quiet {
						data, _ := event.Decode[event.LeadSelectedData](item)
						fmt.Fprintf(s.errOut, "lead %s · %s\n", data.LeadID, data.Basis)
					}
				case event.DelegationStarted:
					if !quiet {
						fmt.Fprintf(s.errOut, "member %s started\n", item.Actor)
					}
				case event.ToolCalled:
					if !quiet {
						data, _ := event.Decode[event.ToolCallData](item)
						fmt.Fprintf(s.errOut, "tool %s · %s\n", item.Actor, data.Tool)
					}
				case event.FinalTextDelta:
					data, _ := event.Decode[event.TextDeltaData](item)
					fmt.Fprint(s.out, data.Text)
				case event.FinalCompleted:
					fmt.Fprintln(s.out)
					return run.Wait(s.ctx)
				case event.SessionFailed:
					data, _ := event.Decode[event.FailureData](item)
					return fmt.Errorf("%s", data.Error)
				case event.SessionCancelled:
					return fmt.Errorf("session cancelled")
				}
			}
			if err := run.Wait(s.ctx); err != nil {
				return err
			}
			session, err := app.Store.Session(s.ctx, run.SessionID)
			if err == nil && session.FinalAnswer != "" {
				fmt.Fprintln(s.out, session.FinalAnswer)
				return nil
			}
			return fmt.Errorf("session ended without a final answer")
		},
	}
	command.Flags().StringVarP(&profileReference, "team", "t", "", "Team Profile id, optionally id@revision (required)")
	command.Flags().BoolVarP(&quiet, "quiet", "q", false, "print only the final answer")
	return command
}

func resolveProfile(
	ctx context.Context,
	app *application.Application,
	reference string,
) (*profile.Document, error) {
	id, revision, err := parseProfileReference(reference)
	if err != nil {
		return nil, err
	}
	return app.Store.Profile(ctx, id, revision)
}

func parseProfileReference(reference string) (string, int, error) {
	reference = strings.TrimSpace(reference)
	if reference == "" {
		return "", 0, fmt.Errorf("profile reference cannot be empty")
	}
	id, rawRevision, found := strings.Cut(reference, "@")
	if !found {
		return id, 0, nil
	}
	revision, err := strconv.Atoi(rawRevision)
	if err != nil || revision < 1 {
		return "", 0, fmt.Errorf("invalid profile revision %q", rawRevision)
	}
	return id, revision, nil
}
