package orchestrator

import (
	"encoding/json"
	"fmt"
	"sort"

	"altv1/internal/event"
)

type DelegationStatus string

const (
	DelegationPending   DelegationStatus = "pending"
	DelegationRunning   DelegationStatus = "running"
	DelegationCompleted DelegationStatus = "completed"
	DelegationFailed    DelegationStatus = "failed"
	DelegationCancelled DelegationStatus = "cancelled"
)

type Delegation struct {
	Spec        event.DelegationSpec
	Status      DelegationStatus
	Attempt     int
	Interrupted bool
	Result      string
	Findings    []string
	Risks       []string
	Confidence  float64
	Error       string
}

type Projection struct {
	SessionID           string
	Task                string
	ConversationHistory []ConversationTurn
	UserInstructions    []string
	LeadID              string
	LeadConfidence      float64
	LeadBasis           string
	LeadTurns           int
	Delegations         map[string]*Delegation
	FinalAnswer         string
	Terminal            bool
	Failed              bool
	Cancelled           bool
	ModelCalls          int
	TotalTokens         int
	LastSequence        int64
}

type ConversationTurn struct {
	Task            string              `json:"task"`
	Answer          string              `json:"answer,omitempty"`
	Status          string              `json:"status"`
	LeadID          string              `json:"lead_id,omitempty"`
	ObservableTrace []ConversationTrace `json:"observable_trace,omitempty"`
}

// ConversationTrace is observable, durable orchestration provenance. It is
// never fabricated model thought: every entry is copied from an event that ALT
// actually persisted for an earlier turn in this conversation.
type ConversationTrace struct {
	Sequence      int64           `json:"sequence"`
	Kind          event.Kind      `json:"kind"`
	Actor         string          `json:"actor,omitempty"`
	CorrelationID string          `json:"correlation_id,omitempty"`
	Data          json.RawMessage `json:"data"`
}

func Replay(sessionID string, events []event.Event) (*Projection, error) {
	state := &Projection{SessionID: sessionID, Delegations: make(map[string]*Delegation)}
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
	case event.UserInstruction:
		data, err := event.Decode[event.UserInstructionData](item)
		if err != nil {
			return err
		}
		p.UserInstructions = append(p.UserInstructions, data.Text)
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
	case event.DelegationCreated:
		data, err := event.Decode[event.DelegationSpec](item)
		if err != nil {
			return err
		}
		spec := data
		p.Delegations[data.ID] = &Delegation{Spec: spec, Status: DelegationPending}
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

func (p *Projection) ActiveCount() int {
	count := 0
	for _, delegation := range p.Delegations {
		if delegation.Status == DelegationPending || delegation.Status == DelegationRunning {
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
	return count
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
