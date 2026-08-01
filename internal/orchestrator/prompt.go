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
		encoded, _ := json.MarshalIndent(conversationHistory(history), "", "  ")
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
      "depends_on": ["existing delegation id or an earlier key in this decision"],
      "required_tools": ["runtime tool names that must actually be called"]
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
make progress. When an objective depends on workspace evidence, name the
minimum required tools; ALT will reject a result that did not call them. Leave
required_tools empty for conceptual work.
The required_tools list is also the member's complete runtime tool
capability set for that delegation. Include every tool the objective needs and
no others.

Every called member starts fresh and stateless. It receives only its stable
member definition, the objective and context you author for that call, and the
runtime tools you explicitly require. A dependency controls scheduling only;
its result is not transmitted to a later member. If later work needs earlier
evidence, curate the relevant evidence into that call's context.

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
	type delegationView struct {
		ID        string           `json:"id"`
		MemberID  string           `json:"member_id"`
		Objective string           `json:"objective"`
		DependsOn []string         `json:"depends_on,omitempty"`
		Status    DelegationStatus `json:"status"`
		Result    string           `json:"result,omitempty"`
		Findings  []string         `json:"findings,omitempty"`
		Risks     []string         `json:"risks,omitempty"`
		Error     string           `json:"error,omitempty"`
	}
	var members []memberView
	for _, member := range p.CallableMembersFor(lead) {
		members = append(members, memberView{
			ID: member.ID, Definition: p.MemberDefinition(member),
		})
	}
	var delegations []delegationView
	for _, delegation := range state.SortedDelegations() {
		delegations = append(delegations, delegationView{
			ID:        delegation.Spec.ID,
			MemberID:  delegation.Spec.MemberID,
			Objective: delegation.Spec.Objective,
			DependsOn: delegation.Spec.DependsOn,
			Status:    delegation.Status,
			Result:    delegation.Result,
			Findings:  delegation.Findings,
			Risks:     delegation.Risks,
			Error:     delegation.Error,
		})
	}
	payload := map[string]any{
		"user_task":               state.Task,
		"conversation_history":    conversationHistory(state.ConversationHistory),
		"user_instructions":       state.UserInstructions,
		"permitted_members":       members,
		"inherited_runtime_tools": tooling.Supported(),
		"delegations":             delegations,
		"new_signals":             signals,
		"lead_turn":               state.LeadTurns + 1,
	}
	encoded, _ := json.MarshalIndent(payload, "", "  ")
	return system, "CURRENT SESSION STATE:\n" + string(encoded)
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
	if len(delegation.Spec.RequiredTools) > 0 {
		system += "\n\nRequired tool calls:\n- " + strings.Join(delegation.Spec.RequiredTools, "\n- ")
		system += "\nYou must call every required tool before returning the final JSON result."
	}
	system += "\nOnly the listed runtime tools are available. Do not create a result file unless the objective explicitly requires a file; return the result in the final assistant message. Never announce work you intend to do later. Call a tool now, or return the complete final JSON result now."
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
call. Distinguish the state observed at a particular point from current state
when later actions could have changed it. Return only the answer, not JSON.

The user defined the responsible Lead assignment in the following words:
` + p.LeadDefinition(lead)

	type resultView struct {
		Member    string   `json:"member"`
		Objective string   `json:"objective"`
		Result    string   `json:"result"`
		Findings  []string `json:"findings,omitempty"`
		Risks     []string `json:"risks,omitempty"`
	}
	var results []resultView
	for _, delegation := range state.SortedDelegations() {
		if delegation.Status == DelegationCompleted {
			results = append(results, resultView{
				Member:    delegation.Spec.MemberID,
				Objective: delegation.Spec.Objective,
				Result:    delegation.Result,
				Findings:  delegation.Findings,
				Risks:     delegation.Risks,
			})
		}
	}
	payload, _ := json.MarshalIndent(map[string]any{
		"user_task":            state.Task,
		"conversation_history": conversationHistory(state.ConversationHistory),
		"user_instructions":    state.UserInstructions,
		"final_brief":          brief,
		"evidence":             results,
	}, "", "  ")
	return system, string(payload)
}

func conversationHistory(history []ConversationTurn) []ConversationTurn {
	if len(history) == 0 {
		return nil
	}
	return append([]ConversationTurn(nil), history...)
}
