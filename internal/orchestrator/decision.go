package orchestrator

type AgentDecision struct {
	Kind         string               `json:"kind"`
	Assessment   string               `json:"assessment"`
	Delegations  []ProposedDelegation `json:"delegations"`
	PeerTurns    []ProposedPeerTurn   `json:"peer_turns"`
	Cancel       []string             `json:"cancel"`
	Handoff      *ProposedHandoff     `json:"handoff,omitempty"`
	observedWork int
}

type AgentOutcome struct {
	Decision *AgentDecision
	Answer   string
}

type ProposedHandoff struct {
	PeerID string `json:"peer_id"`
	Reason string `json:"reason"`
}

type ProposedPeerTurn struct {
	Key             string   `json:"key"`
	PeerID          string   `json:"peer_id"`
	CollaborationID string   `json:"collaboration_id"`
	Objective       string   `json:"objective"`
	Context         string   `json:"context"`
	Attachments     []string `json:"attachments"`
}

type ProposedDelegation struct {
	Key          string   `json:"key"`
	SpecialistID string   `json:"specialist_id"`
	Objective    string   `json:"objective"`
	Context      string   `json:"context"`
	Attachments  []string `json:"attachments"`
	DependsOn    []string `json:"depends_on"`
}

type SpecialistResult struct {
	Result     string   `json:"result"`
	Findings   []string `json:"findings"`
	Risks      []string `json:"risks"`
	Confidence float64  `json:"confidence"`
}

type Signal struct {
	Kind         string `json:"kind"`
	EventID      string `json:"event_id,omitempty"`
	DelegationID string `json:"delegation_id,omitempty"`
}
