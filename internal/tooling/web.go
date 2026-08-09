package tooling

import (
	"context"
	"fmt"
	"strings"

	"altv1/internal/research/exa"
)

type WebSearchInput struct {
	Query              string            `json:"query" jsonschema:"description=Natural web query describing the evidence to discover."`
	SearchType         string            `json:"search_type,omitempty" jsonschema:"description=Exa search mode: instant, fast, auto, deep-lite, deep, or deep-reasoning. Omit for Exa's recommended auto mode."`
	NumResults         int               `json:"num_results,omitempty" jsonschema:"description=Results to return from 1 through 100. Omit for Exa's default of 10."`
	AdditionalQueries  []string          `json:"additional_queries,omitempty" jsonschema:"description=One through ten alternate formulations for deep search only. Omit to let Exa expand the query itself."`
	IncludeDomains     []string          `json:"include_domains,omitempty" jsonschema:"description=Only return these hosts, wildcard subdomains, or host/path prefixes. Prefer this over site: query syntax."`
	ExcludeDomains     []string          `json:"exclude_domains,omitempty" jsonschema:"description=Exclude these hosts, wildcard subdomains, or host/path prefixes."`
	StartPublishedDate string            `json:"start_published_date,omitempty" jsonschema:"description=ISO 8601 lower publication-date bound."`
	EndPublishedDate   string            `json:"end_published_date,omitempty" jsonschema:"description=ISO 8601 upper publication-date bound."`
	Category           string            `json:"category,omitempty" jsonschema:"description=Optional category hint. Known optimized values: company, publication, news, personal site, financial report, people."`
	UserLocation       string            `json:"user_location,omitempty" jsonschema:"description=Optional two-letter ISO country code for geographically relevant results."`
	Moderation         bool              `json:"moderation,omitempty" jsonschema:"description=Filter unsafe results when true."`
	SystemPrompt       string            `json:"system_prompt,omitempty" jsonschema:"description=Natural-language source or synthesis guidance for this search; useful with deep modes or output_schema."`
	OutputSchema       map[string]any    `json:"output_schema,omitempty" jsonschema:"description=Optional Exa synthesized-output schema. Root type must be text or object; object schemas allow at most depth two and ten properties."`
	Contents           *WebContentsInput `json:"contents,omitempty" jsonschema:"description=Optional content extraction returned with each result. Omit for query-focused highlights; use web_fetch on promising URLs for full evidence."`
}

type WebFetchInput struct {
	URLs     []string         `json:"urls" jsonschema:"description=One through 100 exact URLs or Exa document IDs to retrieve."`
	Contents WebContentsInput `json:"contents,omitempty" jsonschema:"description=Content extraction controls. With no extraction mode, ALT returns up to 50000 text characters per URL."`
}

type WebAnswerInput struct {
	Query               string         `json:"query" jsonschema:"description=Natural-language question for an independent Exa answer with citations."`
	IncludeCitationText *bool          `json:"include_citation_text,omitempty" jsonschema:"description=Include full page text in citation objects. Defaults to true so claims can be checked."`
	OutputSchema        map[string]any `json:"output_schema,omitempty" jsonschema:"description=Optional JSON Schema Draft 7 for a structured answer."`
}

type WebContentsInput struct {
	Text               *WebTextInput       `json:"text,omitempty" jsonschema:"description=Return extracted page text. An empty object enables the provider default."`
	Highlights         *WebHighlightsInput `json:"highlights,omitempty" jsonschema:"description=Return query-relevant passages. An empty object enables the provider default."`
	Summary            *WebSummaryInput    `json:"summary,omitempty" jsonschema:"description=Return a generated per-page summary. An empty object enables the provider default."`
	MaxAgeHours        *int                `json:"max_age_hours,omitempty" jsonschema:"description=Freshness: 0 fetches now, -1 uses cache only, 1 through 720 accepts cache younger than that many hours, omitted uses cached content with live fallback."`
	LivecrawlTimeoutMS int                 `json:"livecrawl_timeout_ms,omitempty" jsonschema:"description=Live fetch timeout in milliseconds from 1 through 90000."`
	FilterEmptyResults *bool               `json:"filter_empty_results,omitempty" jsonschema:"description=Whether results without requested content are removed. Provider default is true."`
	Subpages           int                 `json:"subpages,omitempty" jsonschema:"description=Number of internally linked subpages to retrieve per result, from 0 through 100."`
	SubpageTargets     []string            `json:"subpage_targets,omitempty" jsonschema:"description=Terms used to rank relevant subpages, such as documentation, methodology, or sources."`
	ExtraLinks         int                 `json:"extra_links,omitempty" jsonschema:"description=Number of ordinary links to return from each page."`
	ExtraImageLinks    int                 `json:"extra_image_links,omitempty" jsonschema:"description=Number of image links to return from each page."`
}

type WebTextInput struct {
	MaxCharacters   int      `json:"max_characters,omitempty" jsonschema:"description=Maximum extracted characters per page. Omit for provider default."`
	IncludeHTMLTags bool     `json:"include_html_tags,omitempty" jsonschema:"description=Preserve HTML tags when structural markup matters."`
	Verbosity       string   `json:"verbosity,omitempty" jsonschema:"description=compact, standard, or full. Requires max_age_hours 0."`
	IncludeSections []string `json:"include_sections,omitempty" jsonschema:"description=Only include semantic sections: unspecified, header, navigation, banner, body, sidebar, footer, metadata. Requires max_age_hours 0."`
	ExcludeSections []string `json:"exclude_sections,omitempty" jsonschema:"description=Exclude semantic sections from the same vocabulary. Requires max_age_hours 0."`
}

type WebHighlightsInput struct {
	Query         string `json:"query,omitempty" jsonschema:"description=Passage-ranking query. Omit to reuse the search query when searching."`
	MaxCharacters int    `json:"max_characters,omitempty" jsonschema:"description=Maximum highlight characters per page."`
}

type WebSummaryInput struct {
	Query  string         `json:"query,omitempty" jsonschema:"description=Question the per-page summary should address."`
	Schema map[string]any `json:"schema,omitempty" jsonschema:"description=Optional JSON schema for each page summary."`
}

type WebResearchResult struct {
	Provider string         `json:"provider"`
	Mode     string         `json:"mode"`
	Evidence string         `json:"evidence"`
	Response map[string]any `json:"response"`
}

func (r *Runtime) exaWebSearch(ctx context.Context, input WebSearchInput) (WebResearchResult, error) {
	query := strings.TrimSpace(input.Query)
	if query == "" {
		return WebResearchResult{}, fmt.Errorf("query is required")
	}
	searchType, err := validateSearchType(input.SearchType)
	if err != nil {
		return WebResearchResult{}, err
	}
	if input.NumResults < 0 || input.NumResults > 100 {
		return WebResearchResult{}, fmt.Errorf("num_results must be between 1 and 100 when provided")
	}
	if len(input.AdditionalQueries) > 10 {
		return WebResearchResult{}, fmt.Errorf("additional_queries accepts at most 10 queries")
	}
	if len(input.AdditionalQueries) > 0 && !strings.HasPrefix(searchType, "deep") {
		return WebResearchResult{}, fmt.Errorf("additional_queries requires deep-lite, deep, or deep-reasoning")
	}
	if err := validateLocation(input.UserLocation); err != nil {
		return WebResearchResult{}, err
	}
	category := strings.TrimSpace(input.Category)
	if (category == "company" || category == "people") &&
		(len(input.ExcludeDomains) > 0 || strings.TrimSpace(input.StartPublishedDate) != "" || strings.TrimSpace(input.EndPublishedDate) != "") {
		return WebResearchResult{}, fmt.Errorf("category %q does not support exclude_domains or publication-date filters", category)
	}
	contents, err := buildExaContents(input.Contents, contentDefaults{
		highlightsQuery: query,
		highlightsLimit: 4_000,
	})
	if err != nil {
		return WebResearchResult{}, err
	}
	client := exa.Client{ResolveCredential: r.options.ResolveExaCredential}
	response, err := client.Search(ctx, exa.SearchRequest{
		Query:              query,
		Type:               searchType,
		NumResults:         input.NumResults,
		AdditionalQueries:  cleanStrings(input.AdditionalQueries),
		IncludeDomains:     cleanStrings(input.IncludeDomains),
		ExcludeDomains:     cleanStrings(input.ExcludeDomains),
		StartPublishedDate: strings.TrimSpace(input.StartPublishedDate),
		EndPublishedDate:   strings.TrimSpace(input.EndPublishedDate),
		Category:           category,
		UserLocation:       strings.ToUpper(strings.TrimSpace(input.UserLocation)),
		Moderation:         input.Moderation,
		SystemPrompt:       strings.TrimSpace(input.SystemPrompt),
		OutputSchema:       input.OutputSchema,
		Contents:           contents,
	})
	if err != nil {
		return WebResearchResult{}, err
	}
	return WebResearchResult{
		Provider: "exa",
		Mode:     "search",
		Evidence: "Search results are discovery evidence. Highlights and generated output are useful leads; retrieve the decisive URLs with web_fetch before relying on them, and prefer primary sources.",
		Response: response,
	}, nil
}

func (r *Runtime) exaWebFetch(ctx context.Context, input WebFetchInput) (WebResearchResult, error) {
	urls := cleanStrings(input.URLs)
	if len(urls) == 0 || len(urls) > 100 {
		return WebResearchResult{}, fmt.Errorf("urls must contain between 1 and 100 exact URLs or document IDs")
	}
	for _, rawURL := range urls {
		if len(rawURL) > 2_048 {
			return WebResearchResult{}, fmt.Errorf("each URL or document ID must be at most 2048 characters")
		}
	}
	contents, err := buildExaContents(&input.Contents, contentDefaults{textLimit: 50_000})
	if err != nil {
		return WebResearchResult{}, err
	}
	client := exa.Client{ResolveCredential: r.options.ResolveExaCredential}
	response, err := client.Contents(ctx, exa.ContentsRequest{URLs: urls, Contents: contents})
	if err != nil {
		return WebResearchResult{}, err
	}
	return WebResearchResult{
		Provider: "exa",
		Mode:     "fetch",
		Evidence: "Exa attempted exact content retrieval. Check each per-URL status and the returned text itself; an absent, failed, or empty result supports no claim.",
		Response: response,
	}, nil
}

func (r *Runtime) exaWebAnswer(ctx context.Context, input WebAnswerInput) (WebResearchResult, error) {
	query := strings.TrimSpace(input.Query)
	if query == "" {
		return WebResearchResult{}, fmt.Errorf("query is required")
	}
	includeText := true
	if input.IncludeCitationText != nil {
		includeText = *input.IncludeCitationText
	}
	client := exa.Client{ResolveCredential: r.options.ResolveExaCredential}
	response, err := client.Answer(ctx, exa.AnswerRequest{
		Query: query, Text: includeText, OutputSchema: input.OutputSchema,
	})
	if err != nil {
		return WebResearchResult{}, err
	}
	return WebResearchResult{
		Provider: "exa",
		Mode:     "answer",
		Evidence: "This is an independent Exa synthesis, not ground truth. Validate material claims against its citation text or retrieve the citation URLs with web_fetch.",
		Response: response,
	}, nil
}

type contentDefaults struct {
	textLimit       int
	highlightsQuery string
	highlightsLimit int
}

func buildExaContents(input *WebContentsInput, defaults contentDefaults) (map[string]any, error) {
	if input == nil {
		if defaults.highlightsLimit > 0 {
			return map[string]any{"highlights": map[string]any{
				"query": defaults.highlightsQuery, "maxCharacters": defaults.highlightsLimit,
			}}, nil
		}
		return map[string]any{"text": map[string]any{"maxCharacters": defaults.textLimit}}, nil
	}
	if input.MaxAgeHours != nil && (*input.MaxAgeHours < -1 || *input.MaxAgeHours > 720) {
		return nil, fmt.Errorf("max_age_hours must be -1 through 720")
	}
	if input.LivecrawlTimeoutMS < 0 || input.LivecrawlTimeoutMS > 90_000 {
		return nil, fmt.Errorf("livecrawl_timeout_ms must be between 1 and 90000 when provided")
	}
	if input.Subpages < 0 || input.Subpages > 100 {
		return nil, fmt.Errorf("subpages must be between 0 and 100")
	}
	if input.ExtraLinks < 0 || input.ExtraImageLinks < 0 {
		return nil, fmt.Errorf("extra link counts cannot be negative")
	}
	contents := make(map[string]any)
	if input.Text != nil {
		text, err := buildExaText(*input.Text, input.MaxAgeHours)
		if err != nil {
			return nil, err
		}
		contents["text"] = text
	}
	if input.Highlights != nil {
		if input.Highlights.MaxCharacters < 0 {
			return nil, fmt.Errorf("highlights.max_characters cannot be negative")
		}
		highlights := make(map[string]any)
		if query := strings.TrimSpace(input.Highlights.Query); query != "" {
			highlights["query"] = query
		}
		if input.Highlights.MaxCharacters > 0 {
			highlights["maxCharacters"] = input.Highlights.MaxCharacters
		}
		contents["highlights"] = boolOrObject(highlights)
	}
	if input.Summary != nil {
		summary := make(map[string]any)
		if query := strings.TrimSpace(input.Summary.Query); query != "" {
			summary["query"] = query
		}
		if len(input.Summary.Schema) > 0 {
			summary["schema"] = input.Summary.Schema
		}
		contents["summary"] = boolOrObject(summary)
	}
	if input.Text == nil && input.Highlights == nil && input.Summary == nil {
		switch {
		case defaults.highlightsLimit > 0:
			contents["highlights"] = map[string]any{
				"query": defaults.highlightsQuery, "maxCharacters": defaults.highlightsLimit,
			}
		default:
			contents["text"] = map[string]any{"maxCharacters": defaults.textLimit}
		}
	}
	if input.MaxAgeHours != nil {
		contents["maxAgeHours"] = *input.MaxAgeHours
	}
	if input.LivecrawlTimeoutMS > 0 {
		contents["livecrawlTimeout"] = input.LivecrawlTimeoutMS
	}
	if input.FilterEmptyResults != nil {
		contents["filterEmptyResults"] = *input.FilterEmptyResults
	}
	if input.Subpages > 0 {
		contents["subpages"] = input.Subpages
	}
	if targets := cleanStrings(input.SubpageTargets); len(targets) > 0 {
		contents["subpageTarget"] = targets
	}
	if input.ExtraLinks > 0 || input.ExtraImageLinks > 0 {
		extras := make(map[string]any)
		if input.ExtraLinks > 0 {
			extras["links"] = input.ExtraLinks
		}
		if input.ExtraImageLinks > 0 {
			extras["imageLinks"] = input.ExtraImageLinks
		}
		contents["extras"] = extras
	}
	return contents, nil
}

func buildExaText(input WebTextInput, maxAgeHours *int) (any, error) {
	if input.MaxCharacters < 0 {
		return nil, fmt.Errorf("text.max_characters cannot be negative")
	}
	verbosity := strings.TrimSpace(input.Verbosity)
	if verbosity != "" && verbosity != "compact" && verbosity != "standard" && verbosity != "full" {
		return nil, fmt.Errorf("text.verbosity must be compact, standard, or full")
	}
	if verbosity != "" || len(input.IncludeSections) > 0 || len(input.ExcludeSections) > 0 {
		if maxAgeHours == nil || *maxAgeHours != 0 {
			return nil, fmt.Errorf("text verbosity and semantic section filters require max_age_hours 0")
		}
	}
	if err := validateSections(input.IncludeSections); err != nil {
		return nil, fmt.Errorf("include_sections: %w", err)
	}
	if err := validateSections(input.ExcludeSections); err != nil {
		return nil, fmt.Errorf("exclude_sections: %w", err)
	}
	options := make(map[string]any)
	if input.MaxCharacters > 0 {
		options["maxCharacters"] = input.MaxCharacters
	}
	if input.IncludeHTMLTags {
		options["includeHtmlTags"] = true
	}
	if verbosity != "" {
		options["verbosity"] = verbosity
	}
	if sections := cleanStrings(input.IncludeSections); len(sections) > 0 {
		options["includeSections"] = sections
	}
	if sections := cleanStrings(input.ExcludeSections); len(sections) > 0 {
		options["excludeSections"] = sections
	}
	return boolOrObject(options), nil
}

func boolOrObject(value map[string]any) any {
	if len(value) == 0 {
		return true
	}
	return value
}

func validateSearchType(raw string) (string, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return "", nil
	}
	switch value {
	case "instant", "fast", "auto", "deep-lite", "deep", "deep-reasoning":
		return value, nil
	default:
		return "", fmt.Errorf("search_type must be instant, fast, auto, deep-lite, deep, or deep-reasoning")
	}
}

func validateLocation(raw string) error {
	value := strings.TrimSpace(raw)
	if value == "" {
		return nil
	}
	if len(value) != 2 {
		return fmt.Errorf("user_location must be a two-letter ISO country code")
	}
	for _, character := range value {
		if character < 'A' || (character > 'Z' && character < 'a') || character > 'z' {
			return fmt.Errorf("user_location must be a two-letter ISO country code")
		}
	}
	return nil
}

func validateSections(values []string) error {
	for _, value := range cleanStrings(values) {
		switch value {
		case "unspecified", "header", "navigation", "banner", "body", "sidebar", "footer", "metadata":
		default:
			return fmt.Errorf("unknown semantic section %q", value)
		}
	}
	return nil
}

func cleanStrings(values []string) []string {
	seen := make(map[string]bool, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		result = append(result, value)
	}
	return result
}
