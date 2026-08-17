package profile

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

type Severity string

const (
	Error   Severity = "error"
	Warning Severity = "warning"
)

type Diagnostic struct {
	Severity Severity
	Path     string
	Message  string
}

var identifier = regexp.MustCompile(`^[a-z][a-z0-9]*(?:-[a-z0-9]+)*$`)

// Validate checks only gateway-independent Team structure. Whether an exact
// model selection is currently executable is checked against the authenticated
// gateway catalog at publication and session start.
func Validate(p Profile) []Diagnostic {
	var out []Diagnostic
	add := func(severity Severity, path, message string) {
		out = append(out, Diagnostic{Severity: severity, Path: path, Message: message})
	}

	if p.Schema != CurrentSchema {
		add(Error, "schema", fmt.Sprintf("must be %d", CurrentSchema))
	}
	validateID(&out, "id", p.ID)
	if p.Revision < 1 {
		add(Error, "revision", "must be at least 1")
	}
	if strings.TrimSpace(p.Name) == "" {
		add(Error, "name", "is required")
	}
	if strings.TrimSpace(p.Gateway) == "" {
		add(Error, "gateway", "select an authenticated gateway")
	}
	if len(p.Models) == 0 {
		add(Error, "models", "at least one model is required")
	}
	for alias, model := range p.Models {
		validateID(&out, "models."+alias, alias)
		if strings.TrimSpace(model.Route) == "" {
			add(Error, "models."+alias+".route", "is required")
		}
		if strings.TrimSpace(model.Name) == "" {
			add(Error, "models."+alias+".name", "is required")
		}
	}

	modelOwners := map[string]string{}
	memberModels := map[string]string{}
	registerModel := func(path, memberID, alias string) {
		selected, ok := p.Models[alias]
		if !ok {
			return
		}
		identity := ModelIdentity(selected)
		if owner, exists := modelOwners[identity]; exists && owner != memberID {
			add(Error, path, fmt.Sprintf(
				"assigns %s to %s, but that exact catalog model already belongs to %s; one model is one Team member",
				selected.Name, memberID, owner,
			))
		} else {
			modelOwners[identity] = memberID
		}
		if existing, exists := memberModels[memberID]; exists && existing != identity {
			add(Error, path, fmt.Sprintf("member %s resolves to more than one catalog model", memberID))
		} else {
			memberModels[memberID] = identity
		}
	}

	agentIDs := map[string]bool{}
	definitions := map[string]string{}
	agents := p.Agents()
	for index, agent := range agents {
		path := "primary"
		if index > 0 {
			path = fmt.Sprintf("peers[%d]", index-1)
		}
		validateID(&out, path+".id", agent.ID)
		if agentIDs[agent.ID] {
			add(Error, path+".id", "duplicates another leadership-capable agent")
		}
		agentIDs[agent.ID] = true
		validateModelReference(&out, p, path+".model", agent.Model)
		registerModel(path+".model", agent.ID, agent.Model)
		validateDefinition(&out, path+".definition", agent.ID, agent.Definition, definitions)
	}

	specialistIDs := map[string]bool{}
	for index, specialist := range p.Specialists {
		path := fmt.Sprintf("specialists[%d]", index)
		validateID(&out, path+".id", specialist.ID)
		if specialistIDs[specialist.ID] || agentIDs[specialist.ID] {
			add(Error, path+".id", "duplicates another Team member")
		}
		specialistIDs[specialist.ID] = true
		validateModelReference(&out, p, path+".model", specialist.Model)
		registerModel(path+".model", specialist.ID, specialist.Model)
		validateDefinition(&out, path+".definition", specialist.ID, specialist.Definition, definitions)
	}

	// Peer declarations are undirected. A declaration on either endpoint grants
	// consultation and leadership transfer in both directions; spelling the same
	// edge on both endpoints is rejected so its durable identity is unambiguous.
	peerEdges := map[string]string{}
	usedSpecialists := map[string]bool{}
	for index, agent := range agents {
		path := "primary"
		if index > 0 {
			path = fmt.Sprintf("peers[%d]", index-1)
		}
		localPeers := map[string]bool{}
		for peerIndex, id := range agent.Peers {
			itemPath := fmt.Sprintf("%s.peers[%d]", path, peerIndex)
			validateID(&out, itemPath, id)
			if id == agent.ID {
				add(Error, itemPath, "an agent cannot peer with itself")
			}
			if localPeers[id] {
				add(Error, itemPath, "duplicates another peer relationship")
			}
			localPeers[id] = true
			if !agentIDs[id] {
				add(Error, itemPath, "references unknown leadership-capable peer "+id)
				continue
			}
			first, second := agent.ID, id
			if second < first {
				first, second = second, first
			}
			key := first + "\x00" + second
			if previous, exists := peerEdges[key]; exists {
				add(Error, itemPath, "duplicates undirected peer relationship declared at "+previous)
			} else {
				peerEdges[key] = itemPath
			}
		}
		localSpecialists := map[string]bool{}
		for specialistIndex, id := range agent.Specialists {
			itemPath := fmt.Sprintf("%s.specialists[%d]", path, specialistIndex)
			validateID(&out, itemPath, id)
			if localSpecialists[id] {
				add(Error, itemPath, "duplicates another specialist permission")
			}
			localSpecialists[id] = true
			if !specialistIDs[id] {
				if agentIDs[id] {
					add(Error, itemPath, "references a leadership-capable agent; peers and specialists are exclusive roles")
				} else {
					add(Error, itemPath, "references unknown specialist "+id)
				}
				continue
			}
			usedSpecialists[id] = true
		}
	}
	for id := range specialistIDs {
		if !usedSpecialists[id] {
			add(Warning, "specialists."+id, "is not callable by any leadership-capable agent")
		}
	}

	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Severity != out[j].Severity {
			return out[i].Severity == Error
		}
		return out[i].Path < out[j].Path
	})
	return out
}

func validateDefinition(out *[]Diagnostic, path, id, definition string, definitions map[string]string) {
	normalized := strings.ToLower(strings.Join(strings.Fields(definition), " "))
	if normalized == "" {
		*out = append(*out, Diagnostic{Severity: Error, Path: path, Message: "is required"})
		return
	}
	if other, exists := definitions[normalized]; exists {
		*out = append(*out, Diagnostic{Severity: Warning, Path: path, Message: "duplicates " + other + " after case and whitespace normalization"})
	}
	definitions[normalized] = id
}

func HasErrors(diagnostics []Diagnostic) bool {
	for _, diagnostic := range diagnostics {
		if diagnostic.Severity == Error {
			return true
		}
	}
	return false
}

func validateID(out *[]Diagnostic, path, value string) {
	if !identifier.MatchString(value) {
		*out = append(*out, Diagnostic{Severity: Error, Path: path, Message: "must be lowercase kebab-case"})
	}
}

func validateModelReference(out *[]Diagnostic, p Profile, path, name string) {
	if _, ok := p.Models[name]; !ok {
		*out = append(*out, Diagnostic{Severity: Error, Path: path, Message: "references unknown model " + name})
	}
}
