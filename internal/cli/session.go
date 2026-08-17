package cli

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"altv1/internal/event"

	"github.com/spf13/cobra"
)

func (s *commandState) sessionCommand() *cobra.Command {
	command := &cobra.Command{
		Use:   "session",
		Short: "Inspect, rename, replay, and cancel durable sessions",
	}
	command.AddCommand(
		s.sessionListCommand(),
		s.sessionShowCommand(),
		s.sessionContinueCommand(),
		s.sessionRenameCommand(),
		s.sessionReplayCommand(),
		s.sessionCancelCommand(),
	)
	return command
}

func (s *commandState) sessionContinueCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "continue <session> <prompt>",
		Short: "Add another turn to a durable conversation",
		Args:  cobra.MinimumNArgs(2),
		RunE: func(_ *cobra.Command, args []string) error {
			app, err := s.open()
			if err != nil {
				return err
			}
			defer app.Close()
			id, err := app.Store.ResolveSessionID(s.ctx, args[0])
			if err != nil {
				return err
			}
			run, err := app.Engine.Continue(s.ctx, id, strings.Join(args[1:], " "))
			if err != nil {
				return err
			}
			fmt.Fprintf(s.errOut, "turn %s · session %s\n", shortID(run.SessionID), shortID(run.ConversationID))
			events, unsubscribe, err := app.Store.Subscribe(s.ctx, run.SessionID, 0)
			if err != nil {
				return err
			}
			defer unsubscribe()
			for item := range events {
				switch item.Kind {
				case event.LeadershipTransferred:
					data, _ := event.Decode[event.LeadershipTransferredData](item)
					fmt.Fprintf(s.errOut, "leader %s · %s\n", data.ToAgentID, data.Reason)
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
			return run.Wait(s.ctx)
		},
	}
}

func (s *commandState) sessionListCommand() *cobra.Command {
	var limit int
	command := &cobra.Command{
		Use:   "list",
		Short: "List recent sessions",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			app, err := s.open()
			if err != nil {
				return err
			}
			defer app.Close()
			items, err := app.Store.ListSessions(s.ctx, limit)
			if err != nil {
				return err
			}
			fmt.Fprintln(s.out, "SESSION\tSTATUS\tUPDATED\tPROFILE\tTITLE")
			for _, item := range items {
				reference := item.ConversationID
				if reference == "" {
					reference = item.ID
				}
				fmt.Fprintf(s.out, "%s\t%s\t%s\t%s@%d\t%s\n",
					shortID(reference), item.Status, relativeTime(item.UpdatedAt),
					item.ProfileID, item.ProfileRevision, item.Title)
			}
			return nil
		},
	}
	command.Flags().IntVarP(&limit, "limit", "n", 30, "maximum sessions to list")
	return command
}

func (s *commandState) sessionShowCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "show <session>",
		Short: "Show session metadata and its final answer",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			app, err := s.open()
			if err != nil {
				return err
			}
			defer app.Close()
			id, err := app.Store.ResolveSessionID(s.ctx, args[0])
			if err != nil {
				return err
			}
			item, err := app.Store.Session(s.ctx, id)
			if err != nil {
				return err
			}
			turns, err := app.Store.ConversationSessions(s.ctx, id)
			if err != nil {
				return err
			}
			fmt.Fprintf(s.out,
				"Session:   %s\nLatest:    %s\nTitle:     %s\nStatus:    %s\nProfile:   %s@%d\nLeader:    %s\nWorkspace: %s\nCreated:   %s\nTurns:     %d\n",
				item.ConversationID, item.ID, item.Title, item.Status, item.ProfileID,
				item.ProfileRevision, valueOrDash(item.LeaderID), item.Workspace,
				turns[0].CreatedAt.Local().Format(time.RFC3339), len(turns))
			for index, turn := range turns {
				fmt.Fprintf(s.out, "\nTurn %d:\n%s\n", index+1, turn.Task)
				if turn.FinalAnswer != "" {
					fmt.Fprintf(s.out, "\nAnswer:\n%s\n", turn.FinalAnswer)
				}
			}
			return nil
		},
	}
}

func (s *commandState) sessionRenameCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "rename <session> <title>",
		Short: "Give a session a memorable title",
		Args:  cobra.MinimumNArgs(2),
		RunE: func(_ *cobra.Command, args []string) error {
			app, err := s.open()
			if err != nil {
				return err
			}
			defer app.Close()
			id, err := app.Store.ResolveSessionID(s.ctx, args[0])
			if err != nil {
				return err
			}
			if err := app.Store.RenameSession(s.ctx, id, strings.Join(args[1:], " ")); err != nil {
				return err
			}
			item, err := app.Store.Session(s.ctx, id)
			if err != nil {
				return err
			}
			fmt.Fprintf(s.out, "renamed %s\n", shortID(item.ConversationID))
			return nil
		},
	}
}

func (s *commandState) sessionReplayCommand() *cobra.Command {
	var rawJSON bool
	command := &cobra.Command{
		Use:   "replay <session>",
		Short: "Replay the durable event history",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			app, err := s.open()
			if err != nil {
				return err
			}
			defer app.Close()
			id, err := app.Store.ResolveSessionID(s.ctx, args[0])
			if err != nil {
				return err
			}
			turns, err := app.Store.ConversationSessions(s.ctx, id)
			if err != nil {
				return err
			}
			encoder := json.NewEncoder(s.out)
			for index, turn := range turns {
				items, err := app.Store.Events(s.ctx, turn.ID, 0)
				if err != nil {
					return err
				}
				if !rawJSON {
					fmt.Fprintf(s.out, "TURN %d  %s\n", index+1, shortID(turn.ID))
				}
				for _, item := range items {
					if rawJSON {
						if err := encoder.Encode(item); err != nil {
							return err
						}
						continue
					}
					fmt.Fprintf(s.out, "%4d  %-30s  %-18s %s\n",
						item.Sequence, item.Kind, valueOrDash(item.Actor), eventSummary(item))
				}
			}
			return nil
		},
	}
	command.Flags().BoolVar(&rawJSON, "json", false, "emit newline-delimited JSON events")
	return command
}

func (s *commandState) sessionCancelCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "cancel <session>",
		Short: "Cancel a running session",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			app, err := s.open()
			if err != nil {
				return err
			}
			defer app.Close()
			id, err := app.Store.ResolveSessionID(s.ctx, args[0])
			if err != nil {
				return err
			}
			if err := app.Engine.Cancel(s.ctx, id, "cancelled from CLI"); err != nil {
				return err
			}
			fmt.Fprintf(s.out, "cancelled %s\n", shortID(id))
			return nil
		},
	}
}

func eventSummary(item event.Event) string {
	switch item.Kind {
	case event.LeadershipTransferred:
		data, _ := event.Decode[event.LeadershipTransferredData](item)
		return data.ToAgentID + " · " + data.Reason
	case event.AgentDecision:
		data, _ := event.Decode[event.AgentDecisionData](item)
		return data.Assessment
	case event.DelegationCreated:
		data, _ := event.Decode[event.DelegationSpec](item)
		return data.SpecialistID + " · " + data.Objective
	case event.ToolCalled:
		data, _ := event.Decode[event.ToolCallData](item)
		return data.Tool
	case event.FinalCompleted:
		return "answer persisted"
	case event.SessionFailed, event.SessionCancelled:
		data, _ := event.Decode[event.FailureData](item)
		return data.Error
	default:
		return ""
	}
}

func shortID(id string) string {
	if len(id) <= 8 {
		return id
	}
	return id[:8]
}

func relativeTime(value time.Time) string {
	elapsed := time.Since(value)
	switch {
	case elapsed < time.Minute:
		return "now"
	case elapsed < time.Hour:
		return strconv.Itoa(int(elapsed/time.Minute)) + "m"
	case elapsed < 24*time.Hour:
		return strconv.Itoa(int(elapsed/time.Hour)) + "h"
	default:
		return strconv.Itoa(int(elapsed/(24*time.Hour))) + "d"
	}
}

func valueOrDash(value string) string {
	if value == "" {
		return "—"
	}
	return value
}
