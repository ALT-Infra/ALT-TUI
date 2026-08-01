package profile

import "strings"

// CurrentSchema is the only profile schema ALT accepts. Pre-release schemas
// were deliberately removed rather than carried as permanent compatibility
// surface.
const CurrentSchema = 1

type Profile struct {
	Schema   int                `yaml:"schema" json:"schema"`
	ID       string             `yaml:"id" json:"id"`
	Revision int                `yaml:"revision" json:"revision"`
	Name     string             `yaml:"name" json:"name"`
	Models   map[string]Model   `yaml:"models" json:"models"`
	Router   RouterAssignment   `yaml:"router" json:"router"`
	Leads    []LeadAssignment   `yaml:"leads" json:"leads"`
	Members  []MemberAssignment `yaml:"members,omitempty" json:"members,omitempty"`
	Policy   DisclosurePolicy   `yaml:"disclosure,omitempty" json:"disclosure,omitempty"`
	Metadata map[string]string  `yaml:"metadata,omitempty" json:"metadata,omitempty"`
}

// Model is an exact catalog-issued selection. Gateway, Route, and Name form
// the opaque executable identity. Credentials, endpoints, pricing, context
// windows, and output ceilings remain owned by the gateway adapter.
type Model struct {
	Gateway         string            `yaml:"gateway" json:"gateway"`
	Route           string            `yaml:"route" json:"route"`
	Name            string            `yaml:"name" json:"name"`
	ReasoningEffort string            `yaml:"reasoning_effort,omitempty" json:"reasoning_effort,omitempty"`
	Options         map[string]string `yaml:"options,omitempty" json:"options,omitempty"`
}

type RouterAssignment struct {
	Model      string `yaml:"model" json:"model"`
	Definition string `yaml:"definition" json:"definition"`
}

type LeadAssignment struct {
	ID         string   `yaml:"id" json:"id"`
	Model      string   `yaml:"model" json:"model"`
	Definition string   `yaml:"definition" json:"definition"`
	Calls      []string `yaml:"calls,omitempty" json:"calls,omitempty"`
}

type MemberAssignment struct {
	ID         string `yaml:"id" json:"id"`
	Model      string `yaml:"model" json:"model"`
	Definition string `yaml:"definition" json:"definition"`
}

type DisclosurePolicy struct {
	PersistReasoning bool `yaml:"persist_provider_reasoning" json:"persist_provider_reasoning"`
}

func (p Profile) Lead(id string) (LeadAssignment, bool) {
	for _, lead := range p.Leads {
		if lead.ID == id {
			return lead, true
		}
	}
	return LeadAssignment{}, false
}

func (p Profile) Member(id string) (MemberAssignment, bool) {
	for _, member := range p.Members {
		if member.ID == id {
			return member, true
		}
	}
	if lead, ok := p.Lead(id); ok {
		return MemberAssignment{
			ID: lead.ID, Model: lead.Model, Definition: lead.Definition,
		}, true
	}
	return MemberAssignment{}, false
}

func (p Profile) CallableMembersFor(lead LeadAssignment) []MemberAssignment {
	result := make([]MemberAssignment, 0, len(lead.Calls))
	for _, id := range lead.Calls {
		if member, ok := p.Member(id); ok {
			result = append(result, member)
		}
	}
	return result
}

func (p Profile) CallableMemberFor(lead LeadAssignment, id string) (MemberAssignment, bool) {
	for _, memberID := range lead.Calls {
		if memberID == id {
			return p.Member(id)
		}
	}
	return MemberAssignment{}, false
}

func (p Profile) RouterDefinition() string {
	return p.Router.Definition
}

func (p Profile) LeadDefinition(lead LeadAssignment) string {
	return lead.Definition
}

func (p Profile) MemberDefinition(member MemberAssignment) string {
	return member.Definition
}

func ModelIdentity(model Model) string {
	return strings.Join([]string{
		strings.ToLower(strings.TrimSpace(model.Gateway)),
		strings.ToLower(strings.TrimSpace(model.Route)),
		strings.TrimSpace(model.Name),
	}, "\x00")
}
