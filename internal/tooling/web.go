package tooling

import (
	"context"
	"fmt"
	"strings"

	"altv1/internal/research/exa"
)

type WebSearchInput struct {
	Query              string   `json:"query,omitempty" jsonschema:"description=Semantic web query. Provide query or urls, never both."`
	URLs               []string `json:"urls,omitempty" jsonschema:"description=Exact URLs to retrieve. Provide urls or query, never both."`
	Type               string   `json:"type,omitempty" jsonschema:"description=Exa search type such as auto, neural, keyword, fast, deep-lite, deep, or deep-reasoning. Empty lets Exa choose."`
	NumResults         int      `json:"num_results,omitempty" jsonschema:"description=Requested result count. Zero lets Exa use its current default."`
	IncludeDomains     []string `json:"include_domains,omitempty" jsonschema:"description=Domains Exa should include."`
	ExcludeDomains     []string `json:"exclude_domains,omitempty" jsonschema:"description=Domains Exa should exclude."`
	StartPublishedDate string   `json:"start_published_date,omitempty" jsonschema:"description=Inclusive publication start date accepted by Exa."`
	EndPublishedDate   string   `json:"end_published_date,omitempty" jsonschema:"description=Inclusive publication end date accepted by Exa."`
	Category           string   `json:"category,omitempty" jsonschema:"description=Optional Exa category."`
	Livecrawl          string   `json:"livecrawl,omitempty" jsonschema:"description=Optional Exa livecrawl policy such as fallback, preferred, always, or never."`
	MaxCharacters      int      `json:"max_characters,omitempty" jsonschema:"description=Requested maximum text characters per result. Zero requests Exa's current full/default text behavior."`
}

type WebSearchResult struct {
	Mode     string         `json:"mode"`
	Evidence string         `json:"evidence"`
	Response map[string]any `json:"response"`
}

func (r *Runtime) webSearch(ctx context.Context, input WebSearchInput) (WebSearchResult, error) {
	query := strings.TrimSpace(input.Query)
	hasURLs := len(input.URLs) > 0
	if (query == "") == !hasURLs {
		return WebSearchResult{}, fmt.Errorf("provide exactly one of query or urls")
	}
	if input.NumResults < 0 {
		return WebSearchResult{}, fmt.Errorf("num_results cannot be negative")
	}
	if input.MaxCharacters < 0 {
		return WebSearchResult{}, fmt.Errorf("max_characters cannot be negative")
	}
	text := any(true)
	if input.MaxCharacters > 0 {
		text = map[string]any{"maxCharacters": input.MaxCharacters}
	}
	contents := map[string]any{"text": text}
	if livecrawl := strings.TrimSpace(input.Livecrawl); livecrawl != "" {
		contents["livecrawl"] = livecrawl
	}
	client := exa.Client{ResolveCredential: r.options.ResolveExaCredential}
	if hasURLs {
		response, err := client.Contents(ctx, exa.ContentsRequest{
			URLs: input.URLs, Contents: contents,
		})
		if err != nil {
			return WebSearchResult{}, err
		}
		return WebSearchResult{
			Mode:     "retrieve",
			Evidence: "Exa returned retrieved page content for the exact requested URLs. Verify each claim against that content; an absent or empty text field is not evidence.",
			Response: response,
		}, nil
	}
	response, err := client.Search(ctx, exa.SearchRequest{
		Query:              query,
		Type:               strings.TrimSpace(input.Type),
		NumResults:         input.NumResults,
		IncludeDomains:     input.IncludeDomains,
		ExcludeDomains:     input.ExcludeDomains,
		StartPublishedDate: strings.TrimSpace(input.StartPublishedDate),
		EndPublishedDate:   strings.TrimSpace(input.EndPublishedDate),
		Category:           strings.TrimSpace(input.Category),
		Contents:           contents,
	})
	if err != nil {
		return WebSearchResult{}, err
	}
	return WebSearchResult{
		Mode:     "search-and-retrieve",
		Evidence: "Exa returned semantic candidates together with retrieved page text. Titles and snippets are discovery leads only; support claims with the retrieved text and prefer primary sources.",
		Response: response,
	}, nil
}
