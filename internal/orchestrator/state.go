package orchestrator

import (
	"encoding/json"
	"fmt"
	"sort"

	"altv1/internal/event"
	"altv1/internal/store"
)

type DelegationStatus string

const (
	DelegationPending   DelegationStatus = "pending"
	DelegationRunning   DelegationStatus = "running"
	DelegationCompleted DelegationStatus = "completed"
	DelegationFailed    DelegationStatus = "failed"
	DelegationCancelled DelegationStatus = "cancelled"
)

const (
	projectionTraceLimit       = 64
	projectionInstructionLimit = 64
)

type Delegation struct {
	Spec            event.DelegationSpec
	SpecReference   string
	SpecSequence    int64
	Status          DelegationStatus
	Attempt         int
	Interrupted     bool
	Result          string
	ResultReference string
	ResultSequence  int64
	Findings        []string
	Risks           []string
	Confidence      float64
	Error           string
}

type PeerTurn struct {
	Spec            event.PeerTurnSpec
	SpecReference   string
	SpecSequence    int64
	Status          DelegationStatus
	Attempt         int
	Interrupted     bool
	Result          string
	ResultReference string
	ResultSequence  int64
	Findings        []string
	Risks           []string
	Confidence      float64
	Error           string
}

type Projection struct {
	SessionID                 string
	Task                      string
	TaskReference             string
	ConversationHistory       []ConversationTurn
	UserInstructions          []string
	UserInstructionReferences []string
	UserInstructionsArchived  int
	ObservableTrace           []ConversationTrace
	ObservableTraceArchived   int
	LeadID                    string
	LeadConfidence            float64
	LeadBasis                 string
	LeadTurns                 int
	Delegations               map[string]*Delegation
	PeerTurns                 map[string]*PeerTurn
	FinalAnswer               string
	Terminal                  bool
	Failed                    bool
	Cancelled                 bool
	ModelCalls                int
	TotalTokens               int
	LastSequence              int64
}

type ConversationTurn struct {
	SessionID       string              `json:"session_id"`
	Task            string              `json:"task"`
	TaskReference   string              `json:"task_reference,omitempty"`
	Answer          string              `json:"answer,omitempty"`
	AnswerReference string              `json:"answer_reference,omitempty"`
	Status          string              `json:"status"`
	LeadID          string              `json:"lead_id,omitempty"`
	ObservableTrace []ConversationTrace `json:"observable_trace,omitempty"`
}

// ConversationTrace is observable, durable orchestration provenance. It is
// never fabricated model thought: every entry is copied from an event that ALT
// actually persisted for an earlier turn in this conversation.
type ConversationTrace struct {
	Reference     string          `json:"reference"`
	Sequence      int64           `json:"sequence"`
	Kind          event.Kind      `json:"kind"`
	Actor         string          `json:"actor,omitempty"`
	CorrelationID string          `json:"correlation_id,omitempty"`
	Data          json.RawMessage `json:"data"`
}

func Replay(sessionID string, events []event.Event) (*Projection, error) {
	state := &Projection{SessionID: sessionID, Delegations: make(map[string]*Delegation), PeerTurns: make(map[string]*PeerTurn)}
	for _, item := range events {
		if err := state.Apply(item); err != nil {
			return nil, err
		}
	}
	return state, nil
}

func (p *Projection) Apply(item event.Event) error {
	if item.SessionID != p.SessionID {
		return fmt.Errorf("event session %s does not match projection %s", item.SessionID, p.SessionID)
	}
	if item.Sequence <= p.LastSequence {
		return nil
	}
	switch item.Kind {
	case event.SessionCreated:
		data, err := event.Decode[event.SessionCreatedData](item)
		if err != nil {
			return err
		}
		p.Task = data.Task
		p.TaskReference = store.ContextReferenceForEvent(item)
	case event.UserInstruction:
		data, err := event.Decode[event.UserInstructionData](item)
		if err != nil {
			return err
		}
		p.UserInstructions = append(p.UserInstructions, data.Text)
		p.UserInstructionReferences = append(p.UserInstructionReferences, store.ContextReferenceForEvent(item))
		if len(p.UserInstructions) > projectionInstructionLimit {
			remove := len(p.UserInstructions) - projectionInstructionLimit
			p.UserInstructions = append([]string(nil), p.UserInstructions[remove:]...)
			p.UserInstructionReferences = append([]string(nil), p.UserInstructionReferences[remove:]...)
			p.UserInstructionsArchived += remove
		}
	case event.LeadSelected:
		data, err := event.Decode[event.LeadSelectedData](item)
		if err != nil {
			return err
		}
		p.LeadID = data.LeadID
		p.LeadConfidence = data.Confidence
		p.LeadBasis = data.Basis
	case event.LeadTurnStarted:
		data, err := event.Decode[event.LeadTurnData](item)
		if err != nil {
			return err
		}
		if data.Turn > p.LeadTurns {
			p.LeadTurns = data.Turn
		}
	case event.ToolCalled, event.ToolCompleted:
		p.ObservableTrace = append(p.ObservableTrace, ConversationTrace{
			Reference: store.ContextReferenceForEvent(item), Sequence: item.Sequence,
			Kind: item.Kind, Actor: item.Actor, CorrelationID: item.CorrelationID,
			Data: append(json.RawMessage(nil), item.Data...),
		})
		if len(p.ObservableTrace) > projectionTraceLimit {
			remove := len(p.ObservableTrace) - projectionTraceLimit
			p.ObservableTrace = append([]ConversationTrace(nil), p.ObservableTrace[remove:]...)
			p.ObservableTraceArchived += remove
		}
	case event.DelegationCreated:
		data, err := event.Decode[event.DelegationSpec](item)
		if err != nil {
			return err
		}
		spec := data
		p.Delegations[data.ID] = &Delegation{Spec: spec, SpecReference: store.ContextReferenceForEvent(item), SpecSequence: item.Sequence, Status: DelegationPending}
	case event.DelegationStarted:
		data, err := event.Decode[event.DelegationStartedData](item)
		if err != nil {
			return err
		}
		if delegation := p.Delegations[data.DelegationID]; delegation != nil {
			delegation.Status = DelegationRunning
			delegation.Attempt = data.Attempt
			delegation.Interrupted = false
			delegation.Error = ""
		}
	case event.DelegationCompleted:
		data, err := event.Decode[event.DelegationCompletedData](item)
		if err != nil {
			return err
		}
		if delegation := p.Delegations[data.DelegationID]; delegation != nil {
			delegation.Status = DelegationCompleted
			delegation.Attempt = data.Attempt
			delegation.Result = data.Result
			delegation.ResultReference = store.ContextReferenceForEvent(item)
			delegation.ResultSequence = item.Sequence
			delegation.Findings = append([]string(nil), data.Findings...)
			delegation.Risks = append([]string(nil), data.Risks...)
			delegation.Confidence = data.Confidence
			delegation.Interrupted = false
			delegation.Error = ""
		}
	case event.DelegationFailed:
		data, err := event.Decode[event.DelegationFailedData](item)
		if err != nil {
			return err
		}
		if delegation := p.Delegations[data.DelegationID]; delegation != nil {
			delegation.Status = DelegationFailed
			delegation.Attempt = data.Attempt
			delegation.Error = data.Error
			delegation.Interrupted = item.Actor == "recovery"
		}
	case event.DelegationCancelled:
		data, err := event.Decode[event.DelegationCancelledData](item)
		if err != nil {
			return err
		}
		if delegation := p.Delegations[data.DelegationID]; delegation != nil {
			delegation.Status = DelegationCancelled
			delegation.Interrupted = false
		}
	case event.PeerTurnCreated:
		data, err := event.Decode[event.PeerTurnSpec](item)
		if err != nil {
			return err
		}
		spec := data
		p.PeerTurns[data.ID] = &PeerTurn{Spec: spec, SpecReference: store.ContextReferenceForEvent(item), SpecSequence: item.Sequence, Status: DelegationPending}
	case event.PeerTurnStarted:
		data, err := event.Decode[event.PeerTurnStartedData](item)
		if err != nil {
			return err
		}
		if turn := p.PeerTurns[data.PeerTurnID]; turn != nil {
			turn.Status, turn.Attempt, turn.Interrupted, turn.Error = DelegationRunning, data.Attempt, false, ""
		}
	case event.PeerTurnCompleted:
		data, err := event.Decode[event.PeerTurnCompletedData](item)
		if err != nil {
			return err
		}
		if turn := p.PeerTurns[data.PeerTurnID]; turn != nil {
			turn.Status, turn.Attempt, turn.Result = DelegationCompleted, data.Attempt, data.Result
			turn.ResultReference = store.ContextReferenceForEvent(item)
			turn.ResultSequence = item.Sequence
			turn.Findings, turn.Risks, turn.Confidence = append([]string(nil), data.Findings...), append([]string(nil), data.Risks...), data.Confidence
			turn.Interrupted, turn.Error = false, ""
		}
	case event.PeerTurnFailed:
		data, err := event.Decode[event.PeerTurnFailedData](item)
		if err != nil {
			return err
		}
		if turn := p.PeerTurns[data.PeerTurnID]; turn != nil {
			turn.Status, turn.Attempt, turn.Error = DelegationFailed, data.Attempt, data.Error
			turn.Interrupted = item.Actor == "recovery"
		}
	case event.PeerTurnCancelled:
		data, err := event.Decode[event.PeerTurnCancelledData](item)
		if err != nil {
			return err
		}
		if turn := p.PeerTurns[data.PeerTurnID]; turn != nil {
			turn.Status, turn.Interrupted = DelegationCancelled, false
		}
	case event.ModelCallStarted:
		p.ModelCalls++
	case event.ModelUsage:
		data, err := event.Decode[event.ModelUsageData](item)
		if err != nil {
			return err
		}
		p.TotalTokens += data.TotalTokens
	case event.FinalCompleted:
		data, err := event.Decode[event.FinalCompletedData](item)
		if err != nil {
			return err
		}
		p.FinalAnswer = data.Answer
		p.Terminal = true
	case event.SessionFailed:
		p.Terminal = true
		p.Failed = true
	case event.SessionCancelled:
		p.Terminal = true
		p.Cancelled = true
	}
	p.LastSequence = item.Sequence
	return nil
}

func (p *Projection) SortedDelegations() []*Delegation {
	result := make([]*Delegation, 0, len(p.Delegations))
	for _, delegation := range p.Delegations {
		result = append(result, delegation)
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].Spec.ID < result[j].Spec.ID
	})
	return result
}

func (p *Projection) SortedPeerTurns() []*PeerTurn {
	result := make([]*PeerTurn, 0, len(p.PeerTurns))
	for _, turn := range p.PeerTurns {
		result = append(result, turn)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Spec.ID < result[j].Spec.ID })
	return result
}

func (p *Projection) ActiveCount() int {
	count := 0
	for _, delegation := range p.Delegations {
		if delegation.Status == DelegationPending || delegation.Status == DelegationRunning {
			count++
		}
	}
	for _, turn := range p.PeerTurns {
		if turn.Status == DelegationPending || turn.Status == DelegationRunning {
			count++
		}
	}
	return count
}

// WorkCount includes active work and work whose process was interrupted before
// it could record a semantic result. Actual gateway/model failures are evidence
// returned to the Lead and are never silently repeated.
func (p *Projection) WorkCount() int {
	count := 0
	for _, delegation := range p.Delegations {
		switch delegation.Status {
		case DelegationPending, DelegationRunning:
			count++
		case DelegationFailed:
			if delegation.Interrupted {
				count++
			}
		}
	}
	for _, turn := range p.PeerTurns {
		switch turn.Status {
		case DelegationPending, DelegationRunning:
			count++
		case DelegationFailed:
			if turn.Interrupted {
				count++
			}
		}
	}
	return count
}

func (p *Projection) CollaborationTurns(collaborationID string) []*PeerTurn {
	var result []*PeerTurn
	for _, turn := range p.PeerTurns {
		if turn.Spec.CollaborationID == collaborationID {
			result = append(result, turn)
		}
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Spec.Round != result[j].Spec.Round {
			return result[i].Spec.Round < result[j].Spec.Round
		}
		return result[i].Spec.ID < result[j].Spec.ID
	})
	return result
}

func (p *Projection) DependenciesCompleted(spec event.DelegationSpec) bool {
	for _, id := range spec.DependsOn {
		dependency := p.Delegations[id]
		if dependency == nil || dependency.Status != DelegationCompleted {
			return false
		}
	}
	return true
}
