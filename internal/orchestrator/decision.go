package orchestrator

type RouterDecision struct {
	LeadID     string  `json:"lead_id"`
	Confidence float64 `json:"confidence"`
	Basis      string  `json:"basis"`
}

type LeadDecision struct {
	Assessment   string               `json:"assessment"`
	Delegations  []ProposedDelegation `json:"delegations"`
	PeerTurns    []ProposedPeerTurn   `json:"peer_turns"`
	Cancel       []string             `json:"cancel"`
	Finalize     bool                 `json:"finalize"`
	FinalBrief   string               `json:"final_brief"`
	observedWork int
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
	Key         string   `json:"key"`
	MemberID    string   `json:"member_id"`
	Objective   string   `json:"objective"`
	Context     string   `json:"context"`
	Attachments []string `json:"attachments"`
	DependsOn   []string `json:"depends_on"`
}

type MemberResult struct {
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
