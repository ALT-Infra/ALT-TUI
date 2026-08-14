package orchestrator

import (
	"encoding/json"
	"fmt"
	"strings"

	"altv1/internal/profile"
	"altv1/internal/tooling"
)

func routerMessages(
	p profile.Profile,
	task string,
	history []ConversationTurn,
) (string, string) {
	var candidates strings.Builder
	for _, lead := range p.Leads {
		fmt.Fprintf(&candidates, "\nLEAD %s\n%s\nCallable members:\n", lead.ID, p.LeadDefinition(lead))
		for _, member := range p.CallableMembersFor(lead) {
			fmt.Fprintf(&candidates, "- %s: %s\n", member.ID, p.MemberDefinition(member))
		}
		fmt.Fprintln(&candidates, "Stateful peers:")
		for _, peer := range p.PeerMembersFor(lead) {
			fmt.Fprintf(&candidates, "- %s: %s\n", peer.ID, p.MemberDefinition(peer))
		}
	}
	system := `Select exactly one of the listed Leads to receive the user's request.
Base the choice on the result the user actually wants and on which Lead, together
with its callable members, is better suited to remain responsible for the
whole task. Classify ownership by the work and deliverable needed to satisfy the
request, not by whether its sentence is phrased as a question, explanation, or
command. Explanation is the primary result only when explanation itself is the
requested deliverable, rather than the wording used to request some other
artifact. Do not solve or plan the task while routing it.

Return only JSON with these exact field names:
{
  "lead_id": "one eligible Lead ID",
  "confidence": 0.0,
  "basis": "concise decision basis"
}
The basis should explain the choice without private chain-of-thought.`
	if definition := p.RouterDefinition(); definition != "" {
		system += "\n\nThe user defined the routing assignment this way:\n" + definition
	}
	user := "CURRENT USER TASK:\n" + task
	if len(history) > 0 {
		encoded, _ := json.MarshalIndent(boundedConversationHistory(history), "", "  ")
		user += "\n\nPRIOR CONVERSATION CONTEXT:\n" + string(encoded)
	}
	user += "\n\nELIGIBLE LEAD-PLUS-TEAM ASSIGNMENTS:\n" + candidates.String()
	return system, user
}

func leadMessages(p profile.Profile, lead profile.LeadAssignment, state *Projection, signals []Signal) (string, string) {
	system := `ALT selected this Lead assignment to remain responsible for the
user's request and its final answer. Decide the next useful work from the
current state, then reconsider after new evidence arrives. Independent work may
run in parallel. A later delegation can be formulated after learning from an
earlier result, and those sequential lines of work may coexist with parallel
ones. Delegation is limited to the listed members. Leadership cannot be
transferred, and members cannot delegate or answer the user.

Return only JSON:
{
  "assessment": "brief observable coordination decision, not private reasoning",
  "delegations": [
    {
      "key": "unique key within this decision",
      "member_id": "permitted member id",
      "objective": "self-contained bounded task",
      "context": "only context needed beyond the supplied state",
	  "attachments": ["immutable attachment references needed for this call"],
      "depends_on": ["existing delegation id or an earlier key in this decision"]
    }
  ],
  "peer_turns": [
    {
      "key": "unique key within this decision",
      "peer_id": "permitted peer id",
      "collaboration_id": "existing collaboration id to continue, or empty to begin",
      "objective": "the next concrete contribution sought from the peer",
	  "context": "new context for this round",
	  "attachments": ["immutable attachment references needed for this round"]
    }
  ],
  "cancel": ["active delegation ids no longer useful"],
  "finalize": false,
  "final_brief": ""
}

Use an empty delegation list while useful work is already running. Set finalize
only when you can synthesize the answer from current evidence; final_brief then
states what the final synthesis must emphasize. If no useful work is active and
you create no delegation, set finalize true; an idle non-final decision cannot
make progress.

Every called member starts fresh and stateless. It receives only its stable
member definition, the objective and context you author for that call, and a
runtime tool catalogue it may discover as needed. A dependency controls scheduling only;
its result is not transmitted to a later member. If later work needs earlier
evidence, curate the relevant evidence into that call's context.

Attachments are also explicit per call. The current state lists immutable
attachment references. Include only the references a member needs in that
delegation or peer turn. This does not grant authority: it only transmits the
selected evidence through the already-authorized graph edge.

Peer work is different. A peer relationship permits iterative collaboration
with that contributor while this Lead remains solely accountable. Start a new
collaboration by leaving collaboration_id empty. Continue only a recorded
collaboration with the same peer; that peer receives the completed rounds from
that collaboration and no history from unrelated collaborations or sessions.
Plan at most one new turn for a given collaboration at once, then reconsider
after its result. A peer cannot route, become the Lead, or answer the user.

Current-turn tool observations are delivered in the state below. Do not repeat
a completed immutable call merely to rediscover the same result. Recheck only
when the underlying state could have changed or the delivered evidence is
insufficient; use its exact reference before rerunning an expensive operation.

The session state is cumulative: user_task is the same original request on
every Lead turn, not a fresh request. A completed delegation remains completed
evidence for that request. Do not repeat its objective unless its recorded
result exposes a specific unresolved gap; describe that gap in the new
objective.`
	system += "\n\nThe user defined this Lead assignment in the following words:\n" +
		p.LeadDefinition(lead)

	type memberView struct {
		ID         string `json:"id"`
		Definition string `json:"definition"`
	}
	var members []memberView
	for _, member := range p.CallableMembersFor(lead) {
		members = append(members, memberView{
			ID: member.ID, Definition: p.MemberDefinition(member),
		})
	}
	var peers []memberView
	for _, peer := range p.PeerMembersFor(lead) {
		peers = append(peers, memberView{ID: peer.ID, Definition: p.MemberDefinition(peer)})
	}
	delegations, peerTurns, archivedEvidence := leadEvidenceViews(state, signals)
	payload := map[string]any{
		"user_task":               state.Task,
		"user_task_reference":     state.TaskReference,
		"conversation_history":    boundedConversationHistory(state.ConversationHistory),
		"user_instructions":       boundedUserInstructions(state.UserInstructions, state.UserInstructionReferences, state.UserInstructionsArchived),
		"permitted_members":       members,
		"permitted_peers":         peers,
		"inherited_runtime_tools": tooling.Supported(),
		"delegations":             delegations,
		"peer_turns":              peerTurns,
		"current_tool_evidence":   boundedObservableTrace(state.ObservableTrace, state.ObservableTraceArchived),
		"archived_evidence":       archivedEvidence,
		"new_signals":             signals,
		"lead_turn":               state.LeadTurns + 1,
		"available_attachments":   projectionAttachmentReferences(state),
	}
	encoded, _ := json.MarshalIndent(payload, "", "  ")
	return system, "CURRENT SESSION STATE:\n" + string(encoded)
}

func peerMessages(p profile.Profile, peer profile.MemberAssignment, turn *PeerTurn, history []*PeerTurn) (string, string) {
	system := `Contribute to an iterative collaboration with the selected Lead.
The Lead remains solely accountable for the request and final answer. You may
challenge, refine, or extend the work, but cannot route, delegate, take over the
request, or answer the user.

Return only JSON:
{
  "result": "concise but complete contribution for this round",
  "findings": ["material findings"],
  "risks": ["material risks or uncertainties"],
  "confidence": 0.0
}`
	system += "\n\nThe user defined this contributor in the following words:\n" + p.MemberDefinition(peer)
	type priorRound struct {
		Round     int      `json:"round"`
		Objective string   `json:"objective"`
		Result    string   `json:"result"`
		Findings  []string `json:"findings,omitempty"`
		Risks     []string `json:"risks,omitempty"`
	}
	var prior []priorRound
	start := max(0, len(history)-recentPeerTurnLimit)
	for _, earlier := range history[start:] {
		if earlier.Spec.ID == turn.Spec.ID || earlier.Status != DelegationCompleted {
			continue
		}
		result, _ := compactReferencedText(earlier.Result, earlier.ResultReference, resultVisibleLimit)
		prior = append(prior, priorRound{Round: earlier.Spec.Round, Objective: compactPlainText(earlier.Spec.Objective, 4_000), Result: result, Findings: compactStringList(earlier.Findings, 6_000), Risks: compactStringList(earlier.Risks, 4_000)})
	}
	payload, _ := json.MarshalIndent(map[string]any{
		"collaboration_id": turn.Spec.CollaborationID,
		"prior_rounds":     prior,
		"archived_rounds":  start,
		"archive_recall":   "Use context_browse, context_search, and context_open for earlier rounds; access remains limited to this collaboration.",
		"current_round":    turn.Spec.Round,
		"objective":        turn.Spec.Objective,
		"context":          turn.Spec.Context,
	}, "", "  ")
	return system, string(payload)
}

func memberMessages(
	p profile.Profile,
	member profile.MemberAssignment,
	delegation *Delegation,
) (string, string) {
	system := `Complete only the delegated objective and return the result to the
selected Lead. This assignment cannot delegate, route, transfer leadership, take
ownership of the whole request, or answer the user directly.

Return only JSON:
{
  "result": "concise but complete member result",
  "findings": ["material findings"],
  "risks": ["material risks or uncertainties"],
  "confidence": 0.0
}`
	system += "\n\nThe user defined this member assignment in the following words:\n" +
		p.MemberDefinition(member)
	system += "\nDiscover runtime tools with tool_search when the objective needs them. Do not create a result file unless the objective explicitly requires a file; return the result in the final assistant message. Never announce work you intend to do later. Call a tool now, or return the complete final JSON result now."
	payload := map[string]any{
		"objective": delegation.Spec.Objective,
		"context":   delegation.Spec.Context,
	}
	encoded, _ := json.MarshalIndent(payload, "", "  ")
	return system, string(encoded)
}

func finalMessages(p profile.Profile, lead profile.LeadAssignment, state *Projection, brief string) (string, string) {
	system := `Write the sole final answer to the user. Reconcile the available
evidence, resolve conflicts where possible, and be candid about uncertainty.
Do not mention orchestration mechanics unless they matter to the answer, and do
not expose private chain-of-thought. A tool call which returned an expected
error still failed; describe it as an expected failure, never as a successful
call. Current-turn tool observations are delivered below. Do not repeat a
completed immutable call merely to rediscover the same result; recheck only
when the state could have changed or the delivered evidence is insufficient.
Distinguish the state observed at a particular point from current state
when later actions could have changed it. Return only the answer, not JSON.

The user defined the responsible Lead assignment in the following words:
` + p.LeadDefinition(lead)

	delegations, peerTurns, archivedEvidence := leadEvidenceViews(state, nil)
	payload, _ := json.MarshalIndent(map[string]any{
		"user_task":             state.Task,
		"user_task_reference":   state.TaskReference,
		"conversation_history":  boundedConversationHistory(state.ConversationHistory),
		"user_instructions":     boundedUserInstructions(state.UserInstructions, state.UserInstructionReferences, state.UserInstructionsArchived),
		"final_brief":           brief,
		"delegation_evidence":   delegations,
		"peer_evidence":         peerTurns,
		"current_tool_evidence": boundedObservableTrace(state.ObservableTrace, state.ObservableTraceArchived),
		"archived_evidence":     archivedEvidence,
	}, "", "  ")
	return system, string(payload)
}
