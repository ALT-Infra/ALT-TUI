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
	if len(p.Models) == 0 {
		add(Error, "models", "at least one model is required")
	}
	for alias, model := range p.Models {
		validateID(&out, "models."+alias, alias)
		if strings.TrimSpace(model.Gateway) == "" {
			add(Error, "models."+alias+".gateway", "is required")
		}
		if strings.TrimSpace(model.Route) == "" {
			add(Error, "models."+alias+".route", "is required")
		}
		if strings.TrimSpace(model.Name) == "" {
			add(Error, "models."+alias+".name", "is required")
		}
	}
	if strings.TrimSpace(p.Router.Definition) == "" {
		add(Error, "router.definition", "is required")
	}
	validateModelReference(&out, p, "router.model", p.Router.Model)

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
		if identityForMember, exists := memberModels[memberID]; exists && identityForMember != identity {
			add(Error, path, fmt.Sprintf("member %s resolves to more than one catalog model", memberID))
		} else {
			memberModels[memberID] = identity
		}
	}
	registerModel("router.model", "$router", p.Router.Model)

	if len(p.Leads) < 2 {
		add(Error, "leads", "ALT teams require at least two Leads")
	}
	leadIDs := map[string]bool{}
	definitions := map[string]string{}
	for i, lead := range p.Leads {
		path := fmt.Sprintf("leads[%d]", i)
		validateID(&out, path+".id", lead.ID)
		if leadIDs[lead.ID] {
			add(Error, path+".id", "duplicates another Lead assignment")
		}
		leadIDs[lead.ID] = true
		validateModelReference(&out, p, path+".model", lead.Model)
		registerModel(path+".model", lead.ID, lead.Model)
		validateDefinition(&out, path+".definition", lead.ID, lead.Definition, definitions)
	}

	memberIDs := map[string]bool{}
	for i, member := range p.Members {
		path := fmt.Sprintf("members[%d]", i)
		validateID(&out, path+".id", member.ID)
		if memberIDs[member.ID] || leadIDs[member.ID] {
			add(Error, path+".id", "duplicates another Team member")
		}
		memberIDs[member.ID] = true
		validateModelReference(&out, p, path+".model", member.Model)
		registerModel(path+".model", member.ID, member.Model)
		validateDefinition(&out, path+".definition", member.ID, member.Definition, definitions)
	}

	for i, lead := range p.Leads {
		callIDs := map[string]bool{}
		for j, id := range lead.Calls {
			path := fmt.Sprintf("leads[%d].calls[%d]", i, j)
			validateID(&out, path, id)
			if id == lead.ID {
				add(Error, path, "a Lead cannot call itself")
			}
			if callIDs[id] {
				add(Error, path, "duplicates another callable member")
			}
			callIDs[id] = true
			_, ok := p.Member(id)
			if !ok {
				add(Error, path, "references unknown Team member "+id)
			}
		}
	}
	for id := range memberIDs {
		used := false
		for _, lead := range p.Leads {
			for _, calledID := range lead.Calls {
				used = used || calledID == id
			}
		}
		if !used {
			add(Warning, "members."+id, "is not callable by any Lead")
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
		*out = append(*out, Diagnostic{
			Severity: Warning,
			Path:     path,
			Message:  "duplicates " + other + " after case and whitespace normalization",
		})
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
