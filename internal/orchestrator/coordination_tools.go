package orchestrator

import (
	"context"
	"fmt"
	"strings"

	"github.com/cloudwego/eino/components/tool"
	toolutils "github.com/cloudwego/eino/components/tool/utils"
)

const (
	toolNameCoordinateTeam    = "coordinate_team"
	toolNameHandoffLeadership = "handoff_leadership"
)

type coordinateTeamInput struct {
	Assessment  string               `json:"assessment" jsonschema:"description=Brief observable reason why Team work is needed before answering the user."`
	Delegations []ProposedDelegation `json:"delegations,omitempty" jsonschema:"description=Independent or dependency-ordered calls to authorized thoroughly stateless specialists."`
	PeerTurns   []ProposedPeerTurn   `json:"peer_turns,omitempty" jsonschema:"description=Consultations with authorized context-bearing peers while the caller retains leadership."`
	Cancel      []string             `json:"cancel,omitempty" jsonschema:"description=IDs of active specialist calls or peer consultations that are no longer useful."`
}

type handoffLeadershipInput struct {
	PeerID string `json:"peer_id" jsonschema:"description=Exact ID of the authorized peer that should become sole leader."`
	Reason string `json:"reason" jsonschema:"description=Observable reason this peer should own the user's requested end result."`
}

// coordinationTools are always-visible Eino tools. Unlike runtime tools, they
// return directly from the current ReAct run: ALT validates and commits the
// transition, then starts the next adaptive agent turn. This keeps Team
// authority in ALT while letting models use ordinary tool calling instead of
// having to manufacture control JSON as prose.
func coordinationTools() ([]tool.BaseTool, map[string]bool, error) {
	coordinate, err := toolutils.InferTool(
		toolNameCoordinateTeam,
		"Pause the current agent turn to call stateless specialists, consult peers while retaining leadership, cancel obsolete Team work, or combine those non-handoff actions. Do not use this to answer the user or transfer leadership.",
		func(_ context.Context, input coordinateTeamInput) (AgentDecision, error) {
			return AgentDecision{
				Kind: "coordinate", Assessment: strings.TrimSpace(input.Assessment),
				Delegations: input.Delegations, PeerTurns: input.PeerTurns,
				Cancel: input.Cancel,
			}, nil
		},
	)
	if err != nil {
		return nil, nil, fmt.Errorf("create Team coordination tool: %w", err)
	}
	handoff, err := toolutils.InferTool(
		toolNameHandoffLeadership,
		"Transfer sole leadership for the exact current user request to an authorized context-bearing peer. The peer receives the exact original input and durable Team state, runs the same loop, and may answer the user directly. This ends the caller's current agent turn.",
		func(_ context.Context, input handoffLeadershipInput) (AgentDecision, error) {
			peerID := strings.TrimSpace(input.PeerID)
			reason := strings.TrimSpace(input.Reason)
			return AgentDecision{
				Kind: "coordinate", Assessment: reason,
				Handoff: &ProposedHandoff{PeerID: peerID, Reason: reason},
			}, nil
		},
	)
	if err != nil {
		return nil, nil, fmt.Errorf("create leadership handoff tool: %w", err)
	}
	return []tool.BaseTool{coordinate, handoff}, map[string]bool{
		toolNameCoordinateTeam:    true,
		toolNameHandoffLeadership: true,
	}, nil
}

func isCoordinationTool(name string) bool {
	return name == toolNameCoordinateTeam || name == toolNameHandoffLeadership
}
