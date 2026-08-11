package orchestrator

import (
	"encoding/json"
	"fmt"
	"sort"
	"unicode/utf8"

	"altv1/internal/event"
)

const (
	recentDelegationLimit   = 12
	recentPeerTurnLimit     = 12
	recentConversationLimit = 6
	recentTraceLimit        = 16
	recentInstructionLimit  = 12
	resultVisibleLimit      = 12_000
)

type delegationView struct {
	ID              string           `json:"id"`
	MemberID        string           `json:"member_id"`
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

func leadEvidenceViews(state *Projection, signals []Signal) ([]delegationView, []peerTurnView, *evidenceArchiveView) {
	protected := make(map[string]bool, len(signals))
	for _, signal := range signals {
		if signal.DelegationID != "" {
			protected[signal.DelegationID] = true
		}
	}
	archive := newEvidenceArchive()
	delegations := state.SortedDelegations()
	delegationKeepFrom := max(0, len(delegations)-recentDelegationLimit)
	var delegationViews []delegationView
	for index, delegation := range delegations {
		active := delegation.Status == DelegationPending || delegation.Status == DelegationRunning
		if !active && index < delegationKeepFrom && !protected[delegation.Spec.ID] {
			archive.add(delegation.Spec.MemberID, string(delegation.Status), firstPositive(delegation.SpecSequence, delegation.ResultSequence), max(delegation.SpecSequence, delegation.ResultSequence))
			continue
		}
		result, compacted := compactReferencedText(delegation.Result, delegation.ResultReference, resultVisibleLimit)
		delegationViews = append(delegationViews, delegationView{
			ID: delegation.Spec.ID, MemberID: delegation.Spec.MemberID,
			Objective: compactPlainText(delegation.Spec.Objective, 4_000),
			DependsOn: delegation.Spec.DependsOn, Status: delegation.Status,
			Result: result, Findings: compactStringList(delegation.Findings, 6_000),
			Risks: compactStringList(delegation.Risks, 4_000), Error: compactPlainText(delegation.Error, 2_000),
			SpecReference: delegation.SpecReference, ResultReference: delegation.ResultReference,
			Compacted: compacted,
		})
	}

	peerTurns := state.SortedPeerTurns()
	peerKeepFrom := max(0, len(peerTurns)-recentPeerTurnLimit)
	var peerViews []peerTurnView
	for index, turn := range peerTurns {
		active := turn.Status == DelegationPending || turn.Status == DelegationRunning
		if !active && index < peerKeepFrom && !protected[turn.Spec.ID] {
			archive.add(turn.Spec.PeerID, string(turn.Status), firstPositive(turn.SpecSequence, turn.ResultSequence), max(turn.SpecSequence, turn.ResultSequence))
			continue
		}
		result, compacted := compactReferencedText(turn.Result, turn.ResultReference, resultVisibleLimit)
		peerViews = append(peerViews, peerTurnView{
			ID: turn.Spec.ID, CollaborationID: turn.Spec.CollaborationID,
			PeerID: turn.Spec.PeerID, Round: turn.Spec.Round,
			Objective: compactPlainText(turn.Spec.Objective, 4_000), Status: turn.Status,
			Result: result, Findings: compactStringList(turn.Findings, 6_000),
			Risks: compactStringList(turn.Risks, 4_000), Error: compactPlainText(turn.Error, 2_000),
			SpecReference: turn.SpecReference, ResultReference: turn.ResultReference,
			Compacted: compacted,
		})
	}
	if archive.Omitted == 0 {
		archive = nil
	}
	return delegationViews, peerViews, archive
}

func newEvidenceArchive() *evidenceArchiveView {
	return &evidenceArchiveView{
		ByAssignment: make(map[string]int), ByStatus: make(map[string]int),
		Recall: "Use context_browse when you do not know the right terms, context_search to locate relevant evidence, then context_open on its exact reference. The canonical records were not summarized or discarded.",
	}
}

func (v *evidenceArchiveView) add(assignment, status string, from, through int64) {
	v.Omitted++
	v.ByAssignment[assignment]++
	v.ByStatus[status]++
	if from > 0 && (v.SourceSequenceFrom == 0 || from < v.SourceSequenceFrom) {
		v.SourceSequenceFrom = from
	}
	if through > v.SourceSequenceThrough {
		v.SourceSequenceThrough = through
	}
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
	LeadID          string                  `json:"lead_id,omitempty"`
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
	start := max(0, len(trace)-recentTraceLimit)
	view := observableTraceView{Archived: alreadyArchived + start}
	if view.Archived > 0 {
		view.Recall = "Earlier current-turn tool occurrences remain exact. Browse or search only if the recent delivered evidence is insufficient, then open the exact reference."
	}
	for _, item := range trace[start:] {
		limit := 4_000
		if item.Kind == event.ToolCompleted {
			limit = resultVisibleLimit
		}
		view.Recent = append(view.Recent, conversationTraceView{
			Reference: item.Reference, Sequence: item.Sequence, Kind: string(item.Kind),
			Actor: item.Actor, CorrelationID: item.CorrelationID,
			Data: compactPlainText(string(item.Data), limit),
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
	start := max(0, len(values)-recentInstructionLimit)
	view := instructionHistoryView{Archived: alreadyArchived + start}
	if view.Archived > 0 {
		view.Recall = "Earlier user instructions remain exact in the conversation archive. Browse or search when one becomes relevant, then open its reference."
	}
	for index := start; index < len(values); index++ {
		reference := ""
		if index < len(references) {
			reference = references[index]
		}
		view.Recent = append(view.Recent, instructionView{
			Text: compactPlainText(values[index], 8_000), Reference: reference,
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
	start := max(0, len(history)-recentConversationLimit)
	view := conversationHistoryView{}
	if start > 0 {
		view.Archived = &conversationArchiveView{
			OmittedTurns: start, ByStatus: make(map[string]int),
			Recall: "Earlier conversation turns remain exact in ALT's context archive. Use context_browse, context_search, and context_open when their details become relevant.",
		}
		for _, turn := range history[:start] {
			view.Archived.ByStatus[turn.Status]++
		}
	}
	for _, turn := range history[start:] {
		entry := conversationTurnView{
			SessionID: turn.SessionID, Task: compactPlainText(turn.Task, 8_000),
			TaskReference:   turn.TaskReference,
			Answer:          compactReferencedTextOnly(turn.Answer, turn.AnswerReference, 12_000),
			AnswerReference: turn.AnswerReference, Status: turn.Status, LeadID: turn.LeadID,
		}
		traceStart := max(0, len(turn.ObservableTrace)-recentTraceLimit)
		for _, trace := range turn.ObservableTrace[traceStart:] {
			entry.ObservableTrace = append(entry.ObservableTrace, conversationTraceView{
				Reference: trace.Reference, Sequence: trace.Sequence, Kind: string(trace.Kind),
				Actor: trace.Actor, CorrelationID: trace.CorrelationID,
				Data: compactPlainText(string(trace.Data), 2_000),
			})
		}
		view.Recent = append(view.Recent, entry)
	}
	return view
}

func compactReferencedText(value, reference string, limit int) (string, bool) {
	if len(value) <= limit {
		return value, false
	}
	return compactPlainText(value, limit) + fmt.Sprintf("\n[Exact occurrence: %s]", reference), true
}

func compactReferencedTextOnly(value, reference string, limit int) string {
	result, _ := compactReferencedText(value, reference, limit)
	return result
}

func compactPlainText(value string, limit int) string {
	if limit <= 0 || len(value) <= limit {
		return value
	}
	head := limit * 2 / 3
	tail := limit - head
	for head > 0 && head < len(value) && !utf8.RuneStart(value[head]) {
		head--
	}
	start := len(value) - tail
	for start < len(value) && !utf8.RuneStart(value[start]) {
		start++
	}
	return value[:head] + fmt.Sprintf("\n… [%d bytes omitted from this working view] …\n", start-head) + value[start:]
}

func compactStringList(values []string, limit int) []string {
	if len(values) == 0 {
		return nil
	}
	copy := append([]string(nil), values...)
	remaining := limit
	for index, value := range copy {
		if remaining <= 0 {
			return append(copy[:index], fmt.Sprintf("… %d additional entries remain in the exact referenced record", len(copy)-index))
		}
		copy[index] = compactPlainText(value, min(remaining, 2_000))
		remaining -= len(copy[index])
	}
	return copy
}

func firstPositive(values ...int64) int64 {
	result := int64(0)
	for _, value := range values {
		if value > 0 && (result == 0 || value < result) {
			result = value
		}
	}
	return result
}

func estimatedTokensJSON(value any) int {
	encoded, _ := json.Marshal(value)
	return (len(encoded) + 3) / 4
}

func sortedCountKeys(values map[string]int) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
