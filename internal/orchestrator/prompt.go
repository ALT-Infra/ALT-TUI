package orchestrator

import (
	"encoding/json"

	"altv1/internal/profile"
)

type assignmentView struct {
	ID         string `json:"id"`
	Definition string `json:"definition"`
}

type teamRosterView struct {
	Primary     string           `json:"primary"`
	Peers       []assignmentView `json:"authorized_peers"`
	Specialists []assignmentView `json:"authorized_specialists"`
}

func activeAgentMessages(
	p profile.Profile,
	agent profile.AgentAssignment,
	state *Projection,
	signals []Signal,
	includeConversationHistory bool,
) (string, string) {
	system := agentSystemPrompt(p, agent)
	delegations, peerTurns, archivedEvidence := agentEvidenceViews(state, signals)
	payload := map[string]any{
		"current_leader":             agent.ID,
		"exact_user_input_reference": state.TaskReference,
		"user_instructions":          boundedUserInstructions(state.UserInstructions, state.UserInstructionReferences, state.UserInstructionsArchived),
		"delegations":                delegations,
		"peer_consultations":         peerTurns,
		"current_tool_evidence":      boundedObservableTrace(state.ObservableTrace, state.ObservableTraceArchived),
		"archived_evidence":          archivedEvidence,
		"new_signals":                signals,
		"agent_turn":                 state.AgentTurns + 1,
		"available_attachments":      state.TaskInput.AttachmentRefs(),
	}
	if includeConversationHistory {
		payload["conversation_history"] = boundedConversationHistory(state.ConversationHistory)
	}
	encoded, _ := json.Marshal(payload)
	return system, string(encoded)
}

func peerMessages(
	p profile.Profile,
	peer profile.AgentAssignment,
	turn *PeerTurn,
	history []*PeerTurn,
	state *Projection,
	includeConversationHistory bool,
) (string, string) {
	system := agentSystemPrompt(p, peer)
	type priorRound struct {
		Round     int      `json:"round"`
		Objective string   `json:"objective"`
		Result    string   `json:"result"`
		Findings  []string `json:"findings,omitempty"`
		Risks     []string `json:"risks,omitempty"`
	}
	var prior []priorRound
	for _, earlier := range history {
		if earlier.Spec.ID == turn.Spec.ID || earlier.Status != DelegationCompleted {
			continue
		}
		prior = append(prior, priorRound{
			Round: earlier.Spec.Round, Objective: earlier.Spec.Objective,
			Result: earlier.Result, Findings: append([]string(nil), earlier.Findings...),
			Risks: append([]string(nil), earlier.Risks...),
		})
	}
	delegations, peerTurns, archivedEvidence := agentEvidenceViews(state, nil)
	view := map[string]any{
		"invocation_mode":        "peer_consultation",
		"holds_leadership":       false,
		"caller":                 turn.Spec.CallerID,
		"current_task_reference": state.TaskReference,
		"user_instructions":      boundedUserInstructions(state.UserInstructions, state.UserInstructionReferences, state.UserInstructionsArchived),
		"team_evidence": map[string]any{
			"delegations": delegations, "peer_consultations": peerTurns,
			"tool_evidence": boundedObservableTrace(state.ObservableTrace, state.ObservableTraceArchived),
			"archived":      archivedEvidence,
		},
		"collaboration_id": turn.Spec.CollaborationID,
		"prior_rounds":     prior,
		"archived_rounds":  0,
		"archive_recall":   "Use context_browse, context_search, and context_open for exact earlier context.",
		"current_round":    turn.Spec.Round,
		"objective":        turn.Spec.Objective,
		"context":          turn.Spec.Context,
	}
	if includeConversationHistory {
		view["conversation_history"] = boundedConversationHistory(state.ConversationHistory)
	}
	payload, _ := json.Marshal(view)
	return system, string(payload)
}

func agentSystemPrompt(p profile.Profile, agent profile.AgentAssignment) string {
	peers := make([]assignmentView, 0)
	for _, peer := range p.PeerAgentsFor(agent) {
		peers = append(peers, assignmentView{ID: peer.ID, Definition: p.AgentDefinition(peer)})
	}
	specialists := make([]assignmentView, 0)
	for _, specialist := range p.SpecialistsFor(agent) {
		specialists = append(specialists, assignmentView{ID: specialist.ID, Definition: p.SpecialistDefinition(specialist)})
	}
	roster, _ := json.Marshal(teamRosterView{
		Primary: p.Primary.ID, Peers: peers, Specialists: specialists,
	})
	return `You are a durable, context-bearing ALT Team agent. ALT supplies the
user's exact message separately and appends runtime snapshots without rewriting
earlier context. The newest runtime snapshot states whether you currently hold
leadership or are contributing through a consultation.

Exactly one agent may lead at a time. When you hold leadership, work on the
user's exact message and either answer it directly, call authorized stateless
specialists, consult authorized peers while retaining leadership, or hand sole
leadership to an authorized peer. The receiving peer may run the same loop.
When the snapshot says this is a peer consultation, return only the requested
consultation result to the caller. You have not received leadership in that
mode: do not answer the user, create Team work, or transfer leadership.
Return the consultation as exactly one JSON object and no Markdown:
{"result":"concise but complete contribution","findings":["material findings"],"risks":["material risks or uncertainties"],"confidence":0.0}

A specialist is thoroughly stateless. Every specialist call is a clean slate
containing only its stable definition, the complete standalone prompt authored
by its caller, explicitly selected attachments, and runtime tools. ALT never
adds conversation history, prior-call memory, or hidden supporting context to
a specialist invocation. A leader may call specialists and peers repeatedly
and run independent work in parallel.

When leading and ALT must perform an orchestration transition, use the
always-visible coordinate_team tool for specialist calls, retained-leadership
peer consultations, or cancellations. Use handoff_leadership for an exclusive
leadership transfer. Both tools end that agent run so ALT can commit the
transition. A normal user answer must not be wrapped in a coordination object.

The next user turn always begins at the Team primary regardless of which peer
answered the present turn. Never expose private chain-of-thought.

The user defined your Team role in these exact words:
` + p.AgentDefinition(agent) + `

Your fixed Team relationships for this pinned profile revision are:
` + string(roster)
}

const coordinationFallbackInstruction = `

The authenticated catalog marks this model as tool-call unsupported. To request
an ALT orchestration transition, return exactly one JSON object with this shape
and no Markdown:
{
  "kind": "coordinate",
  "assessment": "brief observable reason",
  "delegations": [{"key":"local work key","specialist_id":"authorized specialist ID","objective":"complete standalone task","context":"all context the stateless specialist needs","attachments":["explicit attachment reference"],"depends_on":["earlier work key"]}],
  "peer_turns": [{"key":"local work key","peer_id":"authorized peer ID","collaboration_id":"optional continuing collaboration ID","objective":"requested contribution","context":"relevant durable context","attachments":["explicit attachment reference"]}],
  "cancel": ["active delegation or peer-turn ID"],
  "handoff": null
}
For an exclusive handoff, delegations, peer_turns, and cancel must be empty and
handoff must instead be {"peer_id":"authorized peer ID","reason":"observable
reason"}. A normal user answer must not use this object. Use supplied state and
Team collaboration honestly.`

func specialistMessages(
	p profile.Profile,
	specialist profile.SpecialistAssignment,
	delegation *Delegation,
) (string, string) {
	system := `Complete only the caller-authored objective and return the result
to the caller. You are a thoroughly stateless specialist. ALT has supplied no
conversation history, prior invocation, Team state, or implicit context. Treat
the objective below as the entire task. You cannot consult, delegate, receive
leadership, transfer leadership, or answer the user directly.

Return only JSON:
{
  "result": "concise but complete specialist result",
  "findings": ["material findings"],
  "risks": ["material risks or uncertainties"],
  "confidence": 0.0
}`
	system += "\n\nThe user defined this specialist in these exact words:\n" + p.SpecialistDefinition(specialist)
	system += "\nRuntime tools are directly available when the objective needs them. Never assume information absent from the objective or attachments."
	payload := map[string]any{
		"objective": delegation.Spec.Objective,
		"context":   delegation.Spec.Context,
	}
	encoded, _ := json.Marshal(payload)
	return system, string(encoded)
}
