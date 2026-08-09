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
// edges, member identities, the Team name, and verbatim definitions. Team
// identity and runtime execution policy remain product-owned;
// gateway limits are discovered or enforced by the gateway.
type TeamDraft struct {
	ID           string          `json:"id"`
	Name         string          `json:"name"`
	Gateway      string          `json:"gateway"`
	BaseRevision int             `json:"base_revision"`
	Router       DraftAssignment `json:"router"`
	Members      []DraftMember   `json:"members"`
	RouterEdges  []string        `json:"router_edges"`
	CallEdges    []DraftCallEdge `json:"call_edges"`
	PeerEdges    []DraftPeerEdge `json:"peer_edges"`
}

type DraftAssignment struct {
	Model      ModelChoice `json:"model"`
	Definition string      `json:"definition"`
}

type DraftMember struct {
	ID         string      `json:"id"`
	Model      ModelChoice `json:"model"`
	Definition string      `json:"definition"`
}

type DraftCallEdge struct {
	LeadID   string `json:"lead_id"`
	MemberID string `json:"member_id"`
}

type DraftPeerEdge struct {
	LeadID   string `json:"lead_id"`
	MemberID string `json:"member_id"`
}

type ModelChoice struct {
	Route string `json:"route"`
	ID    string `json:"id"`
}

func NewDraft() TeamDraft {
	return TeamDraft{
		ID:          "team-" + uuid.NewString(),
		Router:      DraftAssignment{},
		Members:     []DraftMember{},
		RouterEdges: []string{},
		CallEdges:   []DraftCallEdge{},
		PeerEdges:   []DraftPeerEdge{},
	}
}

func DraftFromProfile(p profile.Profile, catalog []provider.CatalogModel) TeamDraft {
	draft := TeamDraft{
		ID:           p.ID,
		Name:         p.Name,
		Gateway:      p.Gateway,
		BaseRevision: p.Revision,
		Router: DraftAssignment{
			Model:      choiceForModel(p.Gateway, p.Models[p.Router.Model], catalog),
			Definition: p.RouterDefinition(),
		},
	}
	byID := make(map[string]int)
	for _, lead := range p.Leads {
		member := DraftMember{
			ID:         lead.ID,
			Model:      choiceForModel(p.Gateway, p.Models[lead.Model], catalog),
			Definition: p.LeadDefinition(lead),
		}
		draft.Members = append(draft.Members, member)
		draft.RouterEdges = append(draft.RouterEdges, lead.ID)
		byID[lead.ID] = len(draft.Members) - 1
	}
	for _, member := range p.Members {
		if _, ok := byID[member.ID]; !ok {
			member := DraftMember{
				ID:         member.ID,
				Model:      choiceForModel(p.Gateway, p.Models[member.Model], catalog),
				Definition: p.MemberDefinition(member),
			}
			draft.Members = append(draft.Members, member)
			byID[member.ID] = len(draft.Members) - 1
		}
	}
	for _, lead := range p.Leads {
		for _, memberID := range lead.Calls {
			draft.CallEdges = append(draft.CallEdges, DraftCallEdge{
				LeadID: lead.ID, MemberID: memberID,
			})
		}
		for _, memberID := range lead.Peers {
			draft.PeerEdges = append(draft.PeerEdges, DraftPeerEdge{
				LeadID: lead.ID, MemberID: memberID,
			})
		}
	}
	sort.SliceStable(draft.Members, func(i, j int) bool {
		return draft.Members[i].ID < draft.Members[j].ID
	})
	sort.SliceStable(draft.CallEdges, func(i, j int) bool {
		if draft.CallEdges[i].LeadID != draft.CallEdges[j].LeadID {
			return draft.CallEdges[i].LeadID < draft.CallEdges[j].LeadID
		}
		return draft.CallEdges[i].MemberID < draft.CallEdges[j].MemberID
	})
	sort.Strings(draft.RouterEdges)
	sort.SliceStable(draft.PeerEdges, func(i, j int) bool {
		if draft.PeerEdges[i].LeadID != draft.PeerEdges[j].LeadID {
			return draft.PeerEdges[i].LeadID < draft.PeerEdges[j].LeadID
		}
		return draft.PeerEdges[i].MemberID < draft.PeerEdges[j].MemberID
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
		Router: profile.RouterAssignment{
			Model:      "router",
			Definition: d.Router.Definition,
		},
	}
	value.Models["router"] = d.Router.Model.profileModel()

	leads := make(map[string]bool, len(d.RouterEdges))
	for _, id := range d.RouterEdges {
		leads[id] = true
	}
	for _, member := range d.Members {
		id := strings.TrimSpace(member.ID)
		if id == "" {
			continue
		}
		value.Models[id] = member.Model.profileModel()
		if leads[id] {
			value.Leads = append(value.Leads, profile.LeadAssignment{
				ID: id, Model: id, Definition: member.Definition,
			})
		} else {
			value.Members = append(value.Members, profile.MemberAssignment{
				ID: id, Model: id, Definition: member.Definition,
			})
		}
	}
	for _, edge := range d.CallEdges {
		for index := range value.Leads {
			if value.Leads[index].ID == edge.LeadID {
				value.Leads[index].Calls = appendUnique(
					value.Leads[index].Calls,
					edge.MemberID,
				)
			}
		}
	}
	for _, edge := range d.PeerEdges {
		for index := range value.Leads {
			if value.Leads[index].ID == edge.LeadID {
				value.Leads[index].Peers = appendUnique(
					value.Leads[index].Peers,
					edge.MemberID,
				)
			}
		}
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
	result := make([]Diagnostic, 0, len(raw)+len(d.Members)+4)
	emptyAliases := map[string]bool{}
	for _, member := range d.Members {
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
		case item.Path == "leads":
			continue
		case strings.TrimSpace(d.Router.Model.ID) == "" &&
			(item.Path == "router.model" || strings.HasPrefix(item.Path, "models.router.")):
			continue
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
	if strings.TrimSpace(d.Router.Model.ID) == "" {
		result = append(result, Diagnostic{
			Severity: "error", Path: "router.model", Message: "select a catalog model",
		})
	}
	for index, member := range d.Members {
		if strings.TrimSpace(member.ID) == "" {
			result = append(result, Diagnostic{
				Severity: "error",
				Path:     fmt.Sprintf("members[%d].id", index),
				Message:  "is required in lowercase kebab-case",
			})
		}
		if strings.TrimSpace(member.Model.ID) == "" {
			result = append(result, Diagnostic{
				Severity: "error",
				Path:     fmt.Sprintf("members[%d].model", index),
				Message:  "select a catalog model",
			})
		}
	}
	if len(uniqueStrings(d.RouterEdges)) < 2 {
		result = append(result, Diagnostic{
			Severity: "error", Path: "leads", Message: "ALT teams require at least two Leads",
		})
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
	check("router.model", d.Router.Model)
	for index, member := range d.Members {
		check(fmt.Sprintf("members[%d].model", index), member.Model)
	}
	sort.SliceStable(result, func(i, j int) bool {
		if result[i].Severity != result[j].Severity {
			return result[i].Severity == "error"
		}
		return result[i].Path < result[j].Path
	})
	return result
}

func uniqueStrings(values []string) []string {
	seen := make(map[string]bool, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		if value != "" && !seen[value] {
			seen[value] = true
			result = append(result, value)
		}
	}
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
