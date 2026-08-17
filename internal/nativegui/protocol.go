package nativegui

import (
	"fmt"
	"sort"
	"strings"

	"altv1/internal/profile"
	"altv1/internal/provider"
	"altv1/internal/store"

	"github.com/google/uuid"
)

type Mode string

const (
	ModeTeam     Mode = "team"
	ModeThinking Mode = "thinking"
)

type TeamView string

const (
	TeamViewNew     TeamView = "new"
	TeamViewEdit    TeamView = "edit"
	TeamViewInspect TeamView = "inspect"
)

type Request struct {
	Operation string     `json:"operation"`
	Draft     *TeamDraft `json:"draft,omitempty"`
	Gateway   string     `json:"gateway,omitempty"`
	View      TeamView   `json:"view,omitempty"`
	ProfileID string     `json:"profile_id,omitempty"`
	Revision  int        `json:"revision,omitempty"`
}

type Response struct {
	OK          bool          `json:"ok"`
	Error       string        `json:"error,omitempty"`
	Initial     *InitialState `json:"initial,omitempty"`
	Diagnostics []Diagnostic  `json:"diagnostics,omitempty"`
	Published   *Published    `json:"published,omitempty"`
	Thinking    any           `json:"thinking,omitempty"`
}

type InitialState struct {
	Mode        Mode                         `json:"mode"`
	View        TeamView                     `json:"view,omitempty"`
	Runtime     RuntimeCapabilities          `json:"runtime"`
	Catalog     []provider.CatalogModel      `json:"catalog,omitempty"`
	Gateways    []provider.GatewayDescriptor `json:"gateways,omitempty"`
	Profiles    []store.ProfileSummary       `json:"profiles,omitempty"`
	Draft       *TeamDraft                   `json:"draft,omitempty"`
	Thinking    any                          `json:"thinking,omitempty"`
	Diagnostics []Diagnostic                 `json:"diagnostics,omitempty"`
}

type RuntimeCapabilities struct {
	DangerouslyBypassApprovalsAndSandbox bool     `json:"dangerously_bypass_approvals_and_sandbox"`
	FilesystemConfinement                bool     `json:"filesystem_confinement"`
	DirectTerminalNetwork                bool     `json:"direct_terminal_network"`
	ExaConfigured                        bool     `json:"exa_configured"`
	LinkupConfigured                     bool     `json:"linkup_configured"`
	ResearchProvider                     string   `json:"research_provider,omitempty"`
	Tools                                []string `json:"tools"`
}

type Diagnostic struct {
	Severity string `json:"severity"`
	Path     string `json:"path"`
	Message  string `json:"message"`
}

type Published struct {
	ID       string `json:"id"`
	Revision int    `json:"revision"`
	Digest   string `json:"digest"`
}

// TeamDraft is deliberately narrower than profile.Profile. It contains only
// decisions the user owns: one gateway account, model assignments, graph
// edges, participant identities, the Team name, and verbatim definitions. Team
// identity and runtime execution policy remain product-owned;
// gateway limits are discovered or enforced by the gateway.
type TeamDraft struct {
	ID              string                `json:"id"`
	Name            string                `json:"name"`
	Gateway         string                `json:"gateway"`
	BaseRevision    int                   `json:"base_revision"`
	Primary         DraftMember           `json:"primary"`
	Peers           []DraftMember         `json:"peers"`
	Specialists     []DraftMember         `json:"specialists"`
	PeerEdges       []DraftPeerEdge       `json:"peer_edges"`
	SpecialistEdges []DraftSpecialistEdge `json:"specialist_edges"`
}

type DraftMember struct {
	ID         string      `json:"id"`
	Model      ModelChoice `json:"model"`
	Definition string      `json:"definition"`
}

type DraftPeerEdge struct {
	FirstAgentID  string `json:"first_agent_id"`
	SecondAgentID string `json:"second_agent_id"`
}

type DraftSpecialistEdge struct {
	AgentID      string `json:"agent_id"`
	SpecialistID string `json:"specialist_id"`
}

type ModelChoice struct {
	Route string `json:"route"`
	ID    string `json:"id"`
}

func NewDraft() TeamDraft {
	return TeamDraft{
		ID:              "team-" + uuid.NewString(),
		Primary:         DraftMember{ID: "primary"},
		Peers:           []DraftMember{},
		Specialists:     []DraftMember{},
		PeerEdges:       []DraftPeerEdge{},
		SpecialistEdges: []DraftSpecialistEdge{},
	}
}

func DraftFromProfile(p profile.Profile, catalog []provider.CatalogModel) TeamDraft {
	draft := TeamDraft{
		ID:           p.ID,
		Name:         p.Name,
		Gateway:      p.Gateway,
		BaseRevision: p.Revision,
		Primary: DraftMember{
			ID:         p.Primary.ID,
			Model:      choiceForModel(p.Gateway, p.Models[p.Primary.Model], catalog),
			Definition: p.AgentDefinition(p.Primary),
		},
	}
	for _, peer := range p.Peers {
		draft.Peers = append(draft.Peers, DraftMember{
			ID:         peer.ID,
			Model:      choiceForModel(p.Gateway, p.Models[peer.Model], catalog),
			Definition: p.AgentDefinition(peer),
		})
	}
	for _, specialist := range p.Specialists {
		draft.Specialists = append(draft.Specialists, DraftMember{
			ID:         specialist.ID,
			Model:      choiceForModel(p.Gateway, p.Models[specialist.Model], catalog),
			Definition: p.SpecialistDefinition(specialist),
		})
	}
	seenPeerEdges := make(map[string]bool)
	for _, agent := range p.Agents() {
		for _, peer := range p.PeerAgentsFor(agent) {
			first, second := agent.ID, peer.ID
			if second < first {
				first, second = second, first
			}
			key := first + "\x00" + second
			if seenPeerEdges[key] {
				continue
			}
			seenPeerEdges[key] = true
			draft.PeerEdges = append(draft.PeerEdges, DraftPeerEdge{
				FirstAgentID: first, SecondAgentID: second,
			})
		}
		for _, specialistID := range agent.Specialists {
			draft.SpecialistEdges = append(draft.SpecialistEdges, DraftSpecialistEdge{
				AgentID: agent.ID, SpecialistID: specialistID,
			})
		}
	}
	sort.SliceStable(draft.Peers, func(i, j int) bool {
		return draft.Peers[i].ID < draft.Peers[j].ID
	})
	sort.SliceStable(draft.Specialists, func(i, j int) bool {
		return draft.Specialists[i].ID < draft.Specialists[j].ID
	})
	sort.SliceStable(draft.PeerEdges, func(i, j int) bool {
		if draft.PeerEdges[i].FirstAgentID != draft.PeerEdges[j].FirstAgentID {
			return draft.PeerEdges[i].FirstAgentID < draft.PeerEdges[j].FirstAgentID
		}
		return draft.PeerEdges[i].SecondAgentID < draft.PeerEdges[j].SecondAgentID
	})
	sort.SliceStable(draft.SpecialistEdges, func(i, j int) bool {
		if draft.SpecialistEdges[i].AgentID != draft.SpecialistEdges[j].AgentID {
			return draft.SpecialistEdges[i].AgentID < draft.SpecialistEdges[j].AgentID
		}
		return draft.SpecialistEdges[i].SpecialistID < draft.SpecialistEdges[j].SpecialistID
	})
	return draft
}

func (d TeamDraft) Profile() profile.Profile {
	value := profile.Profile{
		Schema:  profile.CurrentSchema,
		ID:      strings.TrimSpace(d.ID),
		Name:    strings.TrimSpace(d.Name),
		Gateway: strings.ToLower(strings.TrimSpace(d.Gateway)),
		Models:  make(map[string]profile.Model),
		Primary: profile.AgentAssignment{
			ID: strings.TrimSpace(d.Primary.ID), Model: strings.TrimSpace(d.Primary.ID),
			Definition: d.Primary.Definition,
		},
	}
	if value.Primary.ID != "" {
		value.Models[value.Primary.ID] = d.Primary.Model.profileModel()
	}
	for _, peer := range d.Peers {
		id := strings.TrimSpace(peer.ID)
		if id == "" {
			continue
		}
		value.Models[id] = peer.Model.profileModel()
		value.Peers = append(value.Peers, profile.AgentAssignment{ID: id, Model: id, Definition: peer.Definition})
	}
	for _, specialist := range d.Specialists {
		id := strings.TrimSpace(specialist.ID)
		if id == "" {
			continue
		}
		value.Models[id] = specialist.Model.profileModel()
		value.Specialists = append(value.Specialists, profile.SpecialistAssignment{ID: id, Model: id, Definition: specialist.Definition})
	}
	for _, edge := range d.PeerEdges {
		assignPeer(&value, edge.FirstAgentID, edge.SecondAgentID)
	}
	for _, edge := range d.SpecialistEdges {
		assignSpecialist(&value, edge.AgentID, edge.SpecialistID)
	}
	return value
}

func DiagnosticsForDraft(d TeamDraft, catalog []provider.CatalogModel) []Diagnostic {
	value := d.Profile()
	// A draft has no immutable revision yet. Validate the revision that publish
	// would assign so the authoring surface does not report a storage-owned
	// field as a user error.
	value.Revision = d.BaseRevision + 1
	// FromValue applies product defaults before validation without exposing
	// those controls in the authoring UI.
	document, err := profile.FromValue(value)
	if err != nil {
		return []Diagnostic{{Severity: "error", Path: "draft", Message: err.Error()}}
	}
	raw := mapDiagnostics(profile.Validate(document.Profile))
	assignments := append([]DraftMember{d.Primary}, d.Peers...)
	assignments = append(assignments, d.Specialists...)
	result := make([]Diagnostic, 0, len(raw)+len(assignments)+3)
	emptyAliases := map[string]bool{}
	for _, member := range assignments {
		if strings.TrimSpace(member.Model.ID) == "" && strings.TrimSpace(member.ID) != "" {
			emptyAliases[strings.TrimSpace(member.ID)] = true
		}
	}
	for _, item := range raw {
		switch {
		case item.Path == "id" && strings.TrimSpace(d.ID) == "":
			continue
		case item.Path == "name":
			item.Path = "team.name"
		}
		skip := false
		for alias := range emptyAliases {
			if strings.HasPrefix(item.Path, "models."+alias+".") {
				skip = true
				break
			}
		}
		if !skip {
			result = append(result, item)
		}
	}
	if strings.TrimSpace(d.ID) == "" {
		result = append(result, Diagnostic{
			Severity: "error", Path: "team.id", Message: "is required in lowercase kebab-case",
		})
	}
	if strings.TrimSpace(d.Primary.Model.ID) == "" {
		result = append(result, Diagnostic{
			Severity: "error", Path: "primary.model", Message: "select a catalog model",
		})
	}
	if strings.TrimSpace(d.Primary.ID) == "" {
		result = append(result, Diagnostic{
			Severity: "error", Path: "primary.id", Message: "is required in lowercase kebab-case",
		})
	}
	for index, member := range d.Peers {
		if strings.TrimSpace(member.ID) == "" {
			result = append(result, Diagnostic{
				Severity: "error",
				Path:     fmt.Sprintf("peers[%d].id", index),
				Message:  "is required in lowercase kebab-case",
			})
		}
		if strings.TrimSpace(member.Model.ID) == "" {
			result = append(result, Diagnostic{
				Severity: "error",
				Path:     fmt.Sprintf("peers[%d].model", index),
				Message:  "select a catalog model",
			})
		}
	}
	for index, specialist := range d.Specialists {
		if strings.TrimSpace(specialist.ID) == "" {
			result = append(result, Diagnostic{Severity: "error", Path: fmt.Sprintf("specialists[%d].id", index), Message: "is required in lowercase kebab-case"})
		}
		if strings.TrimSpace(specialist.Model.ID) == "" {
			result = append(result, Diagnostic{Severity: "error", Path: fmt.Sprintf("specialists[%d].model", index), Message: "select a catalog model"})
		}
	}
	agentIDs := make(map[string]bool, len(d.Peers)+1)
	if id := strings.TrimSpace(d.Primary.ID); id != "" {
		agentIDs[id] = true
	}
	for _, peer := range d.Peers {
		if id := strings.TrimSpace(peer.ID); id != "" {
			agentIDs[id] = true
		}
	}
	specialistIDs := make(map[string]bool, len(d.Specialists))
	for _, specialist := range d.Specialists {
		if id := strings.TrimSpace(specialist.ID); id != "" {
			specialistIDs[id] = true
		}
	}
	peerEdges := make(map[string]bool)
	for index, edge := range d.PeerEdges {
		first, second := strings.TrimSpace(edge.FirstAgentID), strings.TrimSpace(edge.SecondAgentID)
		path := fmt.Sprintf("peer_edges[%d]", index)
		if !agentIDs[first] || !agentIDs[second] {
			result = append(result, Diagnostic{Severity: "error", Path: path, Message: "both endpoints must be leadership-capable agents"})
			continue
		}
		if first == second {
			result = append(result, Diagnostic{Severity: "error", Path: path, Message: "an agent cannot peer with itself"})
			continue
		}
		if second < first {
			first, second = second, first
		}
		key := first + "\x00" + second
		if peerEdges[key] {
			result = append(result, Diagnostic{Severity: "error", Path: path, Message: "duplicates another peer edge"})
		}
		peerEdges[key] = true
	}
	for index, edge := range d.SpecialistEdges {
		path := fmt.Sprintf("specialist_edges[%d]", index)
		if !agentIDs[strings.TrimSpace(edge.AgentID)] {
			result = append(result, Diagnostic{Severity: "error", Path: path, Message: "caller must be a leadership-capable agent"})
		}
		if !specialistIDs[strings.TrimSpace(edge.SpecialistID)] {
			result = append(result, Diagnostic{Severity: "error", Path: path, Message: "callee must be a stateless specialist"})
		}
	}
	available := make(map[string]bool, len(catalog))
	for _, item := range catalog {
		available[provider.CatalogIdentity(item)] = true
	}
	check := func(path string, choice ModelChoice) {
		if strings.TrimSpace(choice.ID) == "" {
			return
		}
		identity := provider.CatalogIdentity(provider.CatalogModel{
			Gateway: d.Gateway, Route: choice.Route, ID: choice.ID,
		})
		if !available[identity] {
			result = append(result, Diagnostic{
				Severity: "error",
				Path:     path,
				Message:  fmt.Sprintf("model %s is not in the current authenticated gateway catalog; ALT will not substitute it", choice.ID),
			})
		}
	}
	check("primary.model", d.Primary.Model)
	for index, peer := range d.Peers {
		check(fmt.Sprintf("peers[%d].model", index), peer.Model)
	}
	for index, specialist := range d.Specialists {
		check(fmt.Sprintf("specialists[%d].model", index), specialist.Model)
	}
	sort.SliceStable(result, func(i, j int) bool {
		if result[i].Severity != result[j].Severity {
			return result[i].Severity == "error"
		}
		return result[i].Path < result[j].Path
	})
	return result
}

func mapDiagnostics(values []profile.Diagnostic) []Diagnostic {
	result := make([]Diagnostic, 0, len(values))
	for _, item := range values {
		result = append(result, Diagnostic{
			Severity: string(item.Severity),
			Path:     item.Path,
			Message:  item.Message,
		})
	}
	return result
}

func hasErrors(values []Diagnostic) bool {
	for _, item := range values {
		if item.Severity == "error" {
			return true
		}
	}
	return false
}

func choiceForModel(gateway string, model profile.Model, catalog []provider.CatalogModel) ModelChoice {
	for _, item := range catalog {
		if strings.EqualFold(item.Gateway, gateway) &&
			strings.EqualFold(item.Route, model.Route) && item.ID == model.Name {
			return ModelChoice{
				Route: item.Route,
				ID:    item.ID,
			}
		}
	}
	return ModelChoice{
		Route: model.Route,
		ID:    model.Name,
	}
}

func (m ModelChoice) profileModel() profile.Model {
	return profile.Model{
		Route: strings.ToLower(strings.TrimSpace(m.Route)),
		Name:  strings.TrimSpace(m.ID),
	}
}

func appendUnique(values []string, value string) []string {
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}

func assignPeer(p *profile.Profile, first, second string) {
	first, second = strings.TrimSpace(first), strings.TrimSpace(second)
	if first == "" || second == "" {
		return
	}
	if updateAgent(p, first, func(agent *profile.AgentAssignment) {
		agent.Peers = appendUnique(agent.Peers, second)
	}) {
		return
	}
	updateAgent(p, second, func(agent *profile.AgentAssignment) {
		agent.Peers = appendUnique(agent.Peers, first)
	})
}

func assignSpecialist(p *profile.Profile, agentID, specialistID string) {
	agentID, specialistID = strings.TrimSpace(agentID), strings.TrimSpace(specialistID)
	updateAgent(p, agentID, func(agent *profile.AgentAssignment) {
		agent.Specialists = appendUnique(agent.Specialists, specialistID)
	})
}

func updateAgent(p *profile.Profile, id string, update func(*profile.AgentAssignment)) bool {
	if p.Primary.ID == id {
		update(&p.Primary)
		return true
	}
	for index := range p.Peers {
		if p.Peers[index].ID == id {
			update(&p.Peers[index])
			return true
		}
	}
	return false
}
