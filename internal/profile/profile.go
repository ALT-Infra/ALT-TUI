package profile

import "strings"

// CurrentSchema is the only profile schema ALT accepts. Pre-release schemas
// were deliberately removed rather than carried as permanent compatibility
// surface.
const CurrentSchema = 2

type Profile struct {
	Schema      int                    `yaml:"schema" json:"schema"`
	ID          string                 `yaml:"id" json:"id"`
	Revision    int                    `yaml:"revision" json:"revision"`
	Name        string                 `yaml:"name" json:"name"`
	Gateway     string                 `yaml:"gateway" json:"gateway"`
	Models      map[string]Model       `yaml:"models" json:"models"`
	Primary     AgentAssignment        `yaml:"primary" json:"primary"`
	Peers       []AgentAssignment      `yaml:"peers,omitempty" json:"peers,omitempty"`
	Specialists []SpecialistAssignment `yaml:"specialists,omitempty" json:"specialists,omitempty"`
	Policy      DisclosurePolicy       `yaml:"disclosure,omitempty" json:"disclosure,omitempty"`
	Metadata    map[string]string      `yaml:"metadata,omitempty" json:"metadata,omitempty"`
}

// Model is an exact selection from the Team gateway's catalog. Route and Name
// form its opaque identity inside that one authenticated account. Credentials,
// endpoints, pricing, context windows, and output ceilings remain gateway-owned.
type Model struct {
	// LegacyGateway is accepted only so pre-release Team revisions can be
	// normalized while loading. New profiles never emit it: gateway ownership
	// belongs to Profile.
	LegacyGateway   string            `yaml:"gateway,omitempty" json:"gateway,omitempty"`
	Route           string            `yaml:"route" json:"route"`
	Name            string            `yaml:"name" json:"name"`
	ReasoningEffort string            `yaml:"reasoning_effort,omitempty" json:"reasoning_effort,omitempty"`
	Options         map[string]string `yaml:"options,omitempty" json:"options,omitempty"`
}

// AgentAssignment is a leadership-capable Team participant. Primary is the immutable
// ingress assignment for every user turn; peer relationships form an
// undirected authority channel through which consultation or leadership may
// move. Specialist references are directed call permissions.
type AgentAssignment struct {
	ID          string   `yaml:"id" json:"id"`
	Model       string   `yaml:"model" json:"model"`
	Definition  string   `yaml:"definition" json:"definition"`
	Peers       []string `yaml:"peers,omitempty" json:"peers,omitempty"`
	Specialists []string `yaml:"specialists,omitempty" json:"specialists,omitempty"`
}

// SpecialistAssignment is permanently stateless. Every call starts from the
// stable definition plus the caller-authored prompt and explicitly selected
// attachments; ALT never supplies conversation or prior-call context.
type SpecialistAssignment struct {
	ID         string `yaml:"id" json:"id"`
	Model      string `yaml:"model" json:"model"`
	Definition string `yaml:"definition" json:"definition"`
}

type DisclosurePolicy struct {
	PersistReasoning bool `yaml:"persist_provider_reasoning" json:"persist_provider_reasoning"`
}

func (p Profile) Agent(id string) (AgentAssignment, bool) {
	if p.Primary.ID == id {
		return p.Primary, true
	}
	for _, peer := range p.Peers {
		if peer.ID == id {
			return peer, true
		}
	}
	return AgentAssignment{}, false
}

func (p Profile) Specialist(id string) (SpecialistAssignment, bool) {
	for _, specialist := range p.Specialists {
		if specialist.ID == id {
			return specialist, true
		}
	}
	return SpecialistAssignment{}, false
}

func (p Profile) Agents() []AgentAssignment {
	result := make([]AgentAssignment, 0, len(p.Peers)+1)
	result = append(result, p.Primary)
	result = append(result, p.Peers...)
	return result
}

func (p Profile) SpecialistsFor(agent AgentAssignment) []SpecialistAssignment {
	result := make([]SpecialistAssignment, 0, len(agent.Specialists))
	for _, id := range agent.Specialists {
		if specialist, ok := p.Specialist(id); ok {
			result = append(result, specialist)
		}
	}
	return result
}

func (p Profile) SpecialistFor(agent AgentAssignment, id string) (SpecialistAssignment, bool) {
	for _, specialistID := range agent.Specialists {
		if specialistID == id {
			return p.Specialist(id)
		}
	}
	return SpecialistAssignment{}, false
}

func (p Profile) PeerAgentsFor(agent AgentAssignment) []AgentAssignment {
	var result []AgentAssignment
	for _, candidate := range p.Agents() {
		if candidate.ID == agent.ID {
			continue
		}
		if containsID(agent.Peers, candidate.ID) || containsID(candidate.Peers, agent.ID) {
			result = append(result, candidate)
		}
	}
	return result
}

func (p Profile) PeerAgentFor(agent AgentAssignment, id string) (AgentAssignment, bool) {
	peer, ok := p.Agent(id)
	if !ok || peer.ID == agent.ID {
		return AgentAssignment{}, false
	}
	if containsID(agent.Peers, id) || containsID(peer.Peers, agent.ID) {
		return peer, true
	}
	return AgentAssignment{}, false
}

func (p Profile) AgentDefinition(agent AgentAssignment) string {
	return agent.Definition
}

func (p Profile) SpecialistDefinition(specialist SpecialistAssignment) string {
	return specialist.Definition
}

func containsID(values []string, expected string) bool {
	for _, value := range values {
		if value == expected {
			return true
		}
	}
	return false
}

func ModelIdentity(model Model) string {
	return strings.Join([]string{
		strings.ToLower(strings.TrimSpace(model.Route)),
		strings.TrimSpace(model.Name),
	}, "\x00")
}
