package thinking

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"altv1/internal/event"
	"altv1/internal/profile"
	"altv1/internal/store"
)

type Status string

const (
	Idle      Status = "idle"
	Queued    Status = "queued"
	Running   Status = "running"
	Completed Status = "completed"
	Failed    Status = "failed"
	Cancelled Status = "cancelled"
)

// Projection is the live, session-level view consumed by the native GUI.
// A conversation is one session. Store rows are orchestration turns within it.
type Projection struct {
	SessionID    string        `json:"session_id"`
	ActiveTurnID string        `json:"active_turn_id"`
	Revision     uint64        `json:"revision"`
	Turns        []TurnSummary `json:"turns"`
	Active       *Turn         `json:"active"`

	profile profile.Profile
	byTurn  map[string]*Turn
}

type TurnSummary struct {
	ID        string    `json:"id"`
	Ordinal   int       `json:"ordinal"`
	Task      string    `json:"task"`
	Status    Status    `json:"status"`
	Sequence  int64     `json:"sequence"`
	StartedAt time.Time `json:"started_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type Turn struct {
	ID        string           `json:"id"`
	Ordinal   int              `json:"ordinal"`
	Task      string           `json:"task"`
	Status    Status           `json:"status"`
	Sequence  int64            `json:"sequence"`
	StartedAt time.Time        `json:"started_at"`
	UpdatedAt time.Time        `json:"updated_at"`
	Nodes     map[string]*Node `json:"nodes"`
	Edges     map[string]*Edge `json:"edges"`

	selectedLead      string
	delegationActors  map[string]string
	delegationEdges   map[string]string
	delegationRunning map[string]bool
	actorActivity     map[string]int
	toolOwners        map[string]string
}

type Node struct {
	ID       string            `json:"id"`
	Kind     string            `json:"kind"`
	Label    string            `json:"label"`
	Status   Status            `json:"status"`
	Metadata map[string]string `json:"metadata,omitempty"`
}

type Edge struct {
	ID          string            `json:"id"`
	From        string            `json:"from"`
	To          string            `json:"to"`
	Kind        string            `json:"kind"`
	Direction   string            `json:"direction"`
	Status      Status            `json:"status"`
	Count       int               `json:"count,omitempty"`
	Active      int               `json:"active,omitempty"`
	OccurredAt  time.Time         `json:"occurred_at,omitempty"`
	StartedAtMS int64             `json:"started_at_ms,omitempty"`
	Metadata    map[string]string `json:"metadata,omitempty"`
}

func New(sessionID string, p profile.Profile) *Projection {
	return &Projection{
		SessionID: sessionID,
		profile:   p,
		byTurn:    make(map[string]*Turn),
	}
}

func (p *Projection) AddTurn(record store.Session) error {
	if p.SessionID == "" {
		p.SessionID = record.ConversationID
	}
	if record.ConversationID != p.SessionID {
		return fmt.Errorf("turn %s belongs to session %s, projection belongs to %s",
			record.ID, record.ConversationID, p.SessionID)
	}
	if p.byTurn[record.ID] != nil {
		return nil
	}
	turn := newTurn(record.ID, len(p.Turns)+1, record.Task, record.CreatedAt, p.profile)
	turn.Status = statusFromStore(record.Status)
	turn.UpdatedAt = record.UpdatedAt
	p.byTurn[record.ID] = turn
	p.ActiveTurnID = record.ID
	p.Active = turn
	p.Turns = append(p.Turns, turn.summary())
	p.Revision++
	return nil
}

func (p *Projection) Apply(item event.Event) error {
	turn := p.byTurn[item.SessionID]
	if turn == nil {
		return fmt.Errorf("event belongs to unknown turn %s", item.SessionID)
	}
	if item.Sequence <= turn.Sequence {
		return nil
	}
	if item.Sequence != turn.Sequence+1 {
		return fmt.Errorf("event sequence gap in turn %s: at %d, received %d",
			turn.ID, turn.Sequence, item.Sequence)
	}
	if err := turn.apply(item); err != nil {
		return err
	}
	p.ActiveTurnID = turn.ID
	p.Active = turn
	for index := range p.Turns {
		if p.Turns[index].ID == turn.ID {
			p.Turns[index] = turn.summary()
			break
		}
	}
	p.Revision++
	return nil
}

func (p *Projection) TurnSequence(turnID string) int64 {
	if turn := p.byTurn[turnID]; turn != nil {
		return turn.Sequence
	}
	return 0
}

func (p *Projection) HasTurn(turnID string) bool {
	return p.byTurn[turnID] != nil
}

func newTurn(id string, ordinal int, task string, started time.Time, p profile.Profile) *Turn {
	turn := &Turn{
		ID:                id,
		Ordinal:           ordinal,
		Task:              task,
		Status:            Running,
		StartedAt:         started,
		UpdatedAt:         started,
		Nodes:             make(map[string]*Node),
		Edges:             make(map[string]*Edge),
		delegationActors:  make(map[string]string),
		delegationEdges:   make(map[string]string),
		delegationRunning: make(map[string]bool),
		actorActivity:     make(map[string]int),
		toolOwners:        make(map[string]string),
	}
	turn.Nodes["user"] = &Node{ID: "user", Kind: "user", Label: "User", Status: Running}
	turn.Nodes["router"] = &Node{ID: "router", Kind: "router", Label: "Router", Status: Idle}
	turn.Edges["allowed:user:router"] = &Edge{
		ID: "allowed:user:router", From: "user", To: "router",
		Kind: "allowed", Direction: "outward", Status: Idle,
	}
	for _, lead := range p.Leads {
		turn.ensureMember(lead.ID)
		setMetadata(turn.Nodes[memberID(lead.ID)], "lead", "true")
		turn.Edges["allowed:router:"+lead.ID] = &Edge{
			ID: "allowed:router:" + lead.ID, From: "router", To: memberID(lead.ID),
			Kind: "allowed", Direction: "outward", Status: Idle,
		}
		for _, calledID := range lead.Calls {
			turn.ensureMember(calledID)
			turn.Edges["allowed:"+lead.ID+":"+calledID] = &Edge{
				ID:   "allowed:" + lead.ID + ":" + calledID,
				From: memberID(lead.ID), To: memberID(calledID),
				Kind: "allowed", Direction: "outward", Status: Idle,
			}
		}
	}
	for _, member := range p.Members {
		turn.ensureMember(member.ID)
		setMetadata(turn.Nodes[memberID(member.ID)], "member", "true")
	}
	return turn
}

func (t *Turn) apply(item event.Event) error {
	switch item.Kind {
	case event.SessionCreated:
		data, err := event.Decode[event.SessionCreatedData](item)
		if err != nil {
			return err
		}
		t.Task = data.Task
		t.Status = Running
		setMetadata(t.Nodes["user"], "task", data.Task)
	case event.ProfilePinned:
		data, err := event.Decode[event.ProfilePinnedData](item)
		if err != nil {
			return err
		}
		setMetadata(t.Nodes["user"], "team", fmt.Sprintf("%s@%d", data.ProfileID, data.Revision))
	case event.UserInstruction:
		data, err := event.Decode[event.UserInstructionData](item)
		if err != nil {
			return err
		}
		setMetadata(t.Nodes["user"], "latest_instruction", data.Text)
	case event.RouterStarted:
		t.Nodes["router"].Status = Running
		t.flow("request", "user", "router", "request", "outward", Running, item.At, 1)
	case event.LeadSelected:
		data, err := event.Decode[event.LeadSelectedData](item)
		if err != nil {
			return err
		}
		t.selectedLead = data.LeadID
		t.ensureMember(data.LeadID)
		t.Nodes["router"].Status = Completed
		if edge := t.Edges["flow:request"]; edge != nil {
			edge.Status = Completed
			edge.Active = 0
		}
		t.Nodes[memberID(data.LeadID)].Status = Running
		setMetadata(t.Nodes["router"], "basis", data.Basis)
		setMetadata(t.Nodes["router"], "confidence",
			strconv.FormatFloat(data.Confidence, 'f', 3, 64))
		t.flow("route", "router", memberID(data.LeadID), "route", "outward", Completed, item.At, 0)
	case event.LeadTurnStarted:
		data, err := event.Decode[event.LeadTurnData](item)
		if err != nil {
			return err
		}
		node := t.Nodes[memberID(t.selectedLead)]
		setMetadata(node, "decision_cycle", strconv.Itoa(data.Turn))
		setMetadata(node, "triggering_signals", strings.Join(data.SignalKinds, ", "))
	case event.LeadDecision:
		data, err := event.Decode[event.LeadDecisionData](item)
		if err != nil {
			return err
		}
		node := t.Nodes[memberID(t.selectedLead)]
		setMetadata(node, "assessment", data.Assessment)
		setMetadata(node, "will_finalize", strconv.FormatBool(data.WillFinalize))
		setMetadata(node, "delegations_planned", strconv.Itoa(len(data.Delegations)))
	case event.DelegationCreated:
		data, err := event.Decode[event.DelegationSpec](item)
		if err != nil {
			return err
		}
		t.ensureMember(data.MemberID)
		t.delegationActors[data.ID] = data.MemberID
		edgeID := t.flow(
			"delegation:"+t.selectedLead+":"+data.MemberID,
			memberID(t.selectedLead), memberID(data.MemberID),
			"delegation", "outward", Queued, item.At, 0,
		)
		t.delegationEdges[data.ID] = edgeID
		edge := t.Edges[edgeID]
		setEdgeMetadata(edge, "objective", data.Objective)
		setEdgeMetadata(edge, "context", data.Context)
		setEdgeMetadata(edge, "depends_on", strings.Join(data.DependsOn, ", "))
		setEdgeMetadata(edge, "delegation_id", data.ID)
	case event.DelegationStarted:
		data, err := event.Decode[event.DelegationStartedData](item)
		if err != nil {
			return err
		}
		actor := t.delegationActors[data.DelegationID]
		if actor == "" {
			actor = item.Actor
			t.delegationActors[data.DelegationID] = actor
		}
		t.ensureMember(actor)
		if !t.delegationRunning[data.DelegationID] {
			t.delegationRunning[data.DelegationID] = true
			t.actorActivity[actor]++
		}
		t.Nodes[memberID(actor)].Status = Running
		if edge := t.Edges[t.delegationEdges[data.DelegationID]]; edge != nil {
			if edge.Active == 0 {
				edge.StartedAtMS = item.At.UnixMilli()
			}
			edge.Status = Running
			edge.Active++
			edge.OccurredAt = item.At
		}
	case event.DelegationTextDelta, event.DelegationReasoning:
		data, err := event.Decode[event.TextDeltaData](item)
		if err != nil {
			return err
		}
		actor := t.delegationActors[data.DelegationID]
		node := t.Nodes[memberID(actor)]
		key := "response_stream"
		if item.Kind == event.DelegationReasoning {
			key = "provider_reasoning"
		}
		appendMetadata(node, key, data.Text)
	case event.DelegationCompleted:
		data, err := event.Decode[event.DelegationCompletedData](item)
		if err != nil {
			return err
		}
		actor := t.finishDelegation(data.DelegationID, Completed)
		node := t.Nodes[memberID(actor)]
		setMetadata(node, "result", data.Result)
		setMetadata(node, "findings", strings.Join(data.Findings, "\n"))
		setMetadata(node, "confidence", strconv.FormatFloat(data.Confidence, 'f', 3, 64))
		t.flow("result:"+actor+":"+t.selectedLead, memberID(actor), memberID(t.selectedLead),
			"result", "inward", Completed, item.At, 0)
	case event.DelegationFailed:
		data, err := event.Decode[event.DelegationFailedData](item)
		if err != nil {
			return err
		}
		actor := t.finishDelegation(data.DelegationID, Failed)
		setMetadata(t.Nodes[memberID(actor)], "error", data.Error)
		t.flow("failure:"+actor+":"+t.selectedLead, memberID(actor), memberID(t.selectedLead),
			"failure", "inward", Failed, item.At, 0)
	case event.DelegationCancelled:
		data, err := event.Decode[event.DelegationCancelledData](item)
		if err != nil {
			return err
		}
		actor := t.finishDelegation(data.DelegationID, Cancelled)
		setMetadata(t.Nodes[memberID(actor)], "cancel_reason", data.Reason)
	case event.ToolCalled:
		data, err := event.Decode[event.ToolCallData](item)
		if err != nil {
			return err
		}
		owner := t.ownerFor(data.DelegationID)
		toolID := "tool:" + data.ToolCallID
		t.Nodes[toolID] = &Node{
			ID: toolID, Kind: "tool", Label: data.Tool, Status: Running,
			Metadata: map[string]string{"arguments": data.Arguments},
		}
		t.toolOwners[data.ToolCallID] = owner
		t.flow("tool:"+data.ToolCallID, owner, toolID, "tool", "outward", Running, item.At, 1)
	case event.ToolCompleted:
		data, err := event.Decode[event.ToolCompletedData](item)
		if err != nil {
			return err
		}
		owner := t.toolOwners[data.ToolCallID]
		toolID := "tool:" + data.ToolCallID
		status := Completed
		if data.Failed {
			status = Failed
		}
		if node := t.Nodes[toolID]; node != nil {
			node.Status = status
			setMetadata(node, "error", data.Error)
			setMetadata(node, "result", data.Result)
		}
		if edge := t.Edges["flow:tool:"+data.ToolCallID]; edge != nil {
			edge.Status = status
			edge.Active = 0
		}
		t.flow("tool-result:"+data.ToolCallID, toolID, owner,
			"tool-result", "inward", status, item.At, 0)
	case event.FinalStarted:
		t.flow("answer", memberID(t.selectedLead), "user",
			"answer", "inward", Running, item.At, 1)
	case event.FinalTextDelta, event.FinalReasoning:
		data, err := event.Decode[event.TextDeltaData](item)
		if err != nil {
			return err
		}
		key := "answer_stream"
		if item.Kind == event.FinalReasoning {
			key = "provider_reasoning"
		}
		appendMetadata(t.Nodes[memberID(t.selectedLead)], key, data.Text)
	case event.FinalCompleted:
		data, err := event.Decode[event.FinalCompletedData](item)
		if err != nil {
			return err
		}
		if edge := t.Edges["flow:answer"]; edge != nil {
			edge.Status = Completed
			edge.Active = 0
			setEdgeMetadata(edge, "answer", data.Answer)
		}
		t.Status = Completed
		t.terminate(Completed)
	case event.SessionFailed:
		data, err := event.Decode[event.FailureData](item)
		if err != nil {
			return err
		}
		t.Status = Failed
		t.terminate(Failed)
		setMetadata(t.Nodes["user"], "error", data.Error)
	case event.SessionCancelled:
		t.Status = Cancelled
		t.terminate(Cancelled)
	case event.ModelCallStarted:
		data, err := event.Decode[event.ModelCallStartedData](item)
		if err != nil {
			return err
		}
		node := t.modelTarget(item.CorrelationID, data.Purpose)
		incrementMetadata(node, "model_calls", 1)
		setMetadata(node, "model", data.Model)
	case event.ModelUsage:
		data, err := event.Decode[event.ModelUsageData](item)
		if err != nil {
			return err
		}
		node := t.modelTarget(item.CorrelationID, data.Purpose)
		incrementMetadata(node, "prompt_tokens", data.PromptTokens)
		incrementMetadata(node, "completion_tokens", data.CompletionTokens)
		incrementMetadata(node, "reasoning_tokens", data.ReasoningTokens)
		incrementMetadata(node, "total_tokens", data.TotalTokens)
	}
	t.Sequence = item.Sequence
	t.UpdatedAt = item.At
	return nil
}

func (t *Turn) terminate(status Status) {
	for _, node := range t.Nodes {
		if node.Status == Running || node.Status == Queued || node.ID == "user" {
			node.Status = status
		}
	}
	for _, edge := range t.Edges {
		if edge.Kind == "allowed" {
			continue
		}
		if edge.Active > 0 || edge.Status == Running || edge.Status == Queued {
			edge.Active = 0
			edge.Status = status
		}
	}
}

func (t *Turn) ensureMember(actor string) {
	id := memberID(actor)
	if actor == "" || t.Nodes[id] != nil {
		return
	}
	t.Nodes[id] = &Node{ID: id, Kind: "member", Label: actor, Status: Idle}
}

func (t *Turn) flow(
	id, from, to, kind, direction string,
	status Status,
	at time.Time,
	activeDelta int,
) string {
	edgeID := "flow:" + id
	edge := t.Edges[edgeID]
	if edge == nil {
		edge = &Edge{
			ID: edgeID, From: from, To: to, Kind: kind,
			Direction: direction, Status: status,
		}
		t.Edges[edgeID] = edge
	}
	wasActive := edge.Active
	edge.Count++
	edge.Active += activeDelta
	if wasActive == 0 && edge.Active > 0 {
		edge.StartedAtMS = at.UnixMilli()
	}
	if edge.Active > 0 && status == Queued {
		edge.Status = Running
	} else {
		edge.Status = status
	}
	edge.OccurredAt = at
	return edgeID
}

func (t *Turn) finishDelegation(id string, status Status) string {
	actor := t.delegationActors[id]
	if t.delegationRunning[id] {
		t.delegationRunning[id] = false
		if t.actorActivity[actor] > 0 {
			t.actorActivity[actor]--
		}
	}
	if edge := t.Edges[t.delegationEdges[id]]; edge != nil {
		if edge.Active > 0 {
			edge.Active--
		}
		if edge.Active == 0 {
			edge.Status = status
		}
	}
	if node := t.Nodes[memberID(actor)]; node != nil && t.actorActivity[actor] == 0 {
		if actor != t.selectedLead {
			node.Status = status
		}
	}
	return actor
}

func (t *Turn) ownerFor(delegationID string) string {
	if actor := t.delegationActors[delegationID]; actor != "" {
		return memberID(actor)
	}
	return memberID(t.selectedLead)
}

func (t *Turn) modelTarget(correlationID, purpose string) *Node {
	if actor := t.delegationActors[correlationID]; actor != "" {
		return t.Nodes[memberID(actor)]
	}
	switch {
	case purpose == "router":
		return t.Nodes["router"]
	case strings.HasPrefix(purpose, "member:"):
		actor := strings.TrimPrefix(purpose, "member:")
		t.ensureMember(actor)
		return t.Nodes[memberID(actor)]
	case strings.HasPrefix(purpose, "lead:"), strings.HasPrefix(purpose, "final:"):
		return t.Nodes[memberID(t.selectedLead)]
	}
	return t.Nodes["user"]
}

func (t *Turn) summary() TurnSummary {
	return TurnSummary{
		ID: t.ID, Ordinal: t.Ordinal, Task: t.Task, Status: t.Status,
		Sequence: t.Sequence, StartedAt: t.StartedAt, UpdatedAt: t.UpdatedAt,
	}
}

func memberID(actor string) string {
	return "member:" + actor
}

func statusFromStore(status store.SessionStatus) Status {
	switch status {
	case store.SessionCompleted:
		return Completed
	case store.SessionFailed:
		return Failed
	case store.SessionCancelled:
		return Cancelled
	default:
		return Running
	}
}

func setMetadata(node *Node, key, value string) {
	if node == nil || value == "" {
		return
	}
	if node.Metadata == nil {
		node.Metadata = make(map[string]string)
	}
	node.Metadata[key] = value
}

func appendMetadata(node *Node, key, value string) {
	if node == nil || value == "" {
		return
	}
	if node.Metadata == nil {
		node.Metadata = make(map[string]string)
	}
	node.Metadata[key] += value
}

func incrementMetadata(node *Node, key string, delta int) {
	if node == nil {
		return
	}
	current, _ := strconv.Atoi(node.Metadata[key])
	setMetadata(node, key, strconv.Itoa(current+delta))
}

func setEdgeMetadata(edge *Edge, key, value string) {
	if edge == nil || value == "" {
		return
	}
	if edge.Metadata == nil {
		edge.Metadata = make(map[string]string)
	}
	edge.Metadata[key] = value
}
