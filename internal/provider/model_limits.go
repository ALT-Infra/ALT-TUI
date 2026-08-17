package provider

import (
	"context"
	"strings"
)

type LimitSource string

const (
	LimitSourceGatewayCatalog LimitSource = "gateway_catalog"
	LimitSourceModelsDev      LimitSource = "models.dev"
	LimitSourceObserved       LimitSource = "observed_provider_failure"
)

// TokenLimit carries both a constraint and its owner. Tokens == 0 means the
// limit is unknown, never "unlimited" and never an invitation to invent a
// fallback model size.
type TokenLimit struct {
	Tokens int         `json:"tokens,omitempty"`
	Source LimitSource `json:"source,omitempty"`
}

type ModelLimits struct {
	ContextWindow TokenLimit `json:"context_window,omitempty"`
	MaxInput      TokenLimit `json:"max_input,omitempty"`
	MaxOutput     TokenLimit `json:"max_output,omitempty"`
}

func NewTokenLimit(tokens int, source LimitSource) TokenLimit {
	if tokens <= 0 {
		return TokenLimit{}
	}
	return TokenLimit{Tokens: tokens, Source: source}
}

// MostRestrictiveLimit combines independently owned ceilings. Equal evidence
// keeps both provenances visible instead of pretending one source won.
func MostRestrictiveLimit(current, candidate TokenLimit) TokenLimit {
	if candidate.Tokens <= 0 {
		return current
	}
	if current.Tokens <= 0 || candidate.Tokens < current.Tokens {
		return candidate
	}
	if candidate.Tokens == current.Tokens && candidate.Source != "" && candidate.Source != current.Source {
		parts := []string{string(current.Source), string(candidate.Source)}
		if parts[0] > parts[1] {
			parts[0], parts[1] = parts[1], parts[0]
		}
		current.Source = LimitSource(strings.Join(parts, "+"))
	}
	return current
}

func MergeModelLimits(current, candidate ModelLimits) ModelLimits {
	current.ContextWindow = MostRestrictiveLimit(current.ContextWindow, candidate.ContextWindow)
	current.MaxInput = MostRestrictiveLimit(current.MaxInput, candidate.MaxInput)
	current.MaxOutput = MostRestrictiveLimit(current.MaxOutput, candidate.MaxOutput)
	return current
}

// PromptCapacity is the hard input-only ceiling before reserving room for the
// next response. The combined context constraint is always relevant, even
// when a provider also publishes a separate input maximum.
func (l ModelLimits) PromptCapacity() int {
	contextTokens := l.ContextWindow.Tokens
	inputTokens := l.MaxInput.Tokens
	switch {
	case contextTokens > 0 && inputTokens > 0:
		return min(contextTokens, inputTokens)
	case inputTokens > 0:
		return inputTokens
	default:
		return contextTokens
	}
}

// CatalogMetadataSource enriches an authenticated selection with non-secret
// model facts. It cannot add or remove model identities.
type CatalogMetadataSource interface {
	Enrich(ctx context.Context, descriptor GatewayDescriptor, models []CatalogModel) ([]CatalogModel, error)
}
