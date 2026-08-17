package orchestrator

type delegationView struct {
	ID              string           `json:"id"`
	SpecialistID    string           `json:"specialist_id"`
	Objective       string           `json:"objective"`
	DependsOn       []string         `json:"depends_on,omitempty"`
	Status          DelegationStatus `json:"status"`
	Result          string           `json:"result,omitempty"`
	Findings        []string         `json:"findings,omitempty"`
	Risks           []string         `json:"risks,omitempty"`
	Error           string           `json:"error,omitempty"`
	SpecReference   string           `json:"spec_reference,omitempty"`
	ResultReference string           `json:"result_reference,omitempty"`
	Compacted       bool             `json:"compacted,omitempty"`
}

type peerTurnView struct {
	ID              string           `json:"id"`
	CollaborationID string           `json:"collaboration_id"`
	PeerID          string           `json:"peer_id"`
	Round           int              `json:"round"`
	Objective       string           `json:"objective"`
	Status          DelegationStatus `json:"status"`
	Result          string           `json:"result,omitempty"`
	Findings        []string         `json:"findings,omitempty"`
	Risks           []string         `json:"risks,omitempty"`
	Error           string           `json:"error,omitempty"`
	SpecReference   string           `json:"spec_reference,omitempty"`
	ResultReference string           `json:"result_reference,omitempty"`
	Compacted       bool             `json:"compacted,omitempty"`
}

type evidenceArchiveView struct {
	Omitted               int            `json:"omitted_entries"`
	ByAssignment          map[string]int `json:"by_assignment"`
	ByStatus              map[string]int `json:"by_status"`
	SourceSequenceFrom    int64          `json:"source_sequence_from,omitempty"`
	SourceSequenceThrough int64          `json:"source_sequence_through,omitempty"`
	Recall                string         `json:"recall"`
}

func agentEvidenceViews(state *Projection, signals []Signal) ([]delegationView, []peerTurnView, *evidenceArchiveView) {
	// The provider-derived model compactor owns visibility bounds. Keeping a
	// second count-based eviction policy here used to discard useful evidence
	// after 12 entries regardless of whether the selected model had 8K or 1M
	// tokens. Projection data is already backed by exact context references, so
	// this layer supplies the complete structured state and lets the shared
	// budget reduce the final model request when the current route requires it.
	_ = signals
	delegations := state.SortedDelegations()
	var delegationViews []delegationView
	for _, delegation := range delegations {
		delegationViews = append(delegationViews, delegationView{
			ID: delegation.Spec.ID, SpecialistID: delegation.Spec.SpecialistID,
			Objective: delegation.Spec.Objective,
			DependsOn: delegation.Spec.DependsOn, Status: delegation.Status,
			Result: delegation.Result, Findings: append([]string(nil), delegation.Findings...),
			Risks: append([]string(nil), delegation.Risks...), Error: delegation.Error,
			SpecReference: delegation.SpecReference, ResultReference: delegation.ResultReference,
		})
	}

	peerTurns := state.SortedPeerTurns()
	var peerViews []peerTurnView
	for _, turn := range peerTurns {
		peerViews = append(peerViews, peerTurnView{
			ID: turn.Spec.ID, CollaborationID: turn.Spec.CollaborationID,
			PeerID: turn.Spec.PeerID, Round: turn.Spec.Round,
			Objective: turn.Spec.Objective, Status: turn.Status,
			Result: turn.Result, Findings: append([]string(nil), turn.Findings...),
			Risks: append([]string(nil), turn.Risks...), Error: turn.Error,
			SpecReference: turn.SpecReference, ResultReference: turn.ResultReference,
		})
	}
	return delegationViews, peerViews, nil
}

type conversationHistoryView struct {
	Recent   []conversationTurnView   `json:"recent,omitempty"`
	Archived *conversationArchiveView `json:"archived,omitempty"`
}

type conversationTurnView struct {
	SessionID       string                  `json:"session_id,omitempty"`
	Task            string                  `json:"task"`
	TaskReference   string                  `json:"task_reference,omitempty"`
	Answer          string                  `json:"answer,omitempty"`
	AnswerReference string                  `json:"answer_reference,omitempty"`
	Status          string                  `json:"status"`
	LeaderID        string                  `json:"leader_id,omitempty"`
	ObservableTrace []conversationTraceView `json:"observable_trace,omitempty"`
}

type conversationTraceView struct {
	Reference     string `json:"reference,omitempty"`
	Sequence      int64  `json:"sequence"`
	Kind          string `json:"kind"`
	Actor         string `json:"actor,omitempty"`
	CorrelationID string `json:"correlation_id,omitempty"`
	Data          string `json:"data,omitempty"`
}

type observableTraceView struct {
	Recent   []conversationTraceView `json:"recent,omitempty"`
	Archived int                     `json:"archived_occurrences,omitempty"`
	Recall   string                  `json:"recall,omitempty"`
}

func boundedObservableTrace(trace []ConversationTrace, alreadyArchived int) observableTraceView {
	view := observableTraceView{Archived: alreadyArchived}
	if view.Archived > 0 {
		view.Recall = "Earlier current-turn tool occurrences remain exact. Browse or search only if the recent delivered evidence is insufficient, then open the exact reference."
	}
	for _, item := range trace {
		view.Recent = append(view.Recent, conversationTraceView{
			Reference: item.Reference, Sequence: item.Sequence, Kind: string(item.Kind),
			Actor: item.Actor, CorrelationID: item.CorrelationID,
			Data: string(item.Data),
		})
	}
	return view
}

type instructionView struct {
	Text      string `json:"text"`
	Reference string `json:"reference,omitempty"`
}

type instructionHistoryView struct {
	Recent   []instructionView `json:"recent,omitempty"`
	Archived int               `json:"archived_occurrences,omitempty"`
	Recall   string            `json:"recall,omitempty"`
}

func boundedUserInstructions(values, references []string, alreadyArchived int) instructionHistoryView {
	view := instructionHistoryView{Archived: alreadyArchived}
	if view.Archived > 0 {
		view.Recall = "Earlier user instructions remain exact in the conversation archive. Browse or search when one becomes relevant, then open its reference."
	}
	for index := range values {
		reference := ""
		if index < len(references) {
			reference = references[index]
		}
		view.Recent = append(view.Recent, instructionView{
			Text: values[index], Reference: reference,
		})
	}
	return view
}

type conversationArchiveView struct {
	OmittedTurns int            `json:"omitted_turns"`
	ByStatus     map[string]int `json:"by_status"`
	Recall       string         `json:"recall"`
}

func boundedConversationHistory(history []ConversationTurn) conversationHistoryView {
	if len(history) == 0 {
		return conversationHistoryView{}
	}
	view := conversationHistoryView{}
	for _, turn := range history {
		entry := conversationTurnView{
			SessionID: turn.SessionID, Task: turn.Task,
			TaskReference:   turn.TaskReference,
			Answer:          turn.Answer,
			AnswerReference: turn.AnswerReference, Status: turn.Status, LeaderID: turn.LeaderID,
		}
		for _, trace := range turn.ObservableTrace {
			entry.ObservableTrace = append(entry.ObservableTrace, conversationTraceView{
				Reference: trace.Reference, Sequence: trace.Sequence, Kind: string(trace.Kind),
				Actor: trace.Actor, CorrelationID: trace.CorrelationID,
				Data: string(trace.Data),
			})
		}
		view.Recent = append(view.Recent, entry)
	}
	return view
}
