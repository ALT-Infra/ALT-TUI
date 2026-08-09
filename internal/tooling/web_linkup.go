package tooling

import (
	"context"
	"fmt"
	"strings"

	"altv1/internal/research/linkup"

	"github.com/cloudwego/eino/components/tool"
	toolutils "github.com/cloudwego/eino/components/tool/utils"
)

type LinkupWebSearchInput struct {
	Query            string         `json:"query" jsonschema:"description=Natural web query describing the evidence to discover."`
	Depth            string         `json:"depth,omitempty" jsonschema:"description=fast for lightweight latency-sensitive discovery, standard for broader agentic search, or deep for several comprehensive iterations. Defaults to standard."`
	MaxResults       int            `json:"max_results,omitempty" jsonschema:"description=Maximum number of results. Omit for Linkup's current default."`
	IncludeDomains   []string       `json:"include_domains,omitempty" jsonschema:"description=Only use these domains; Linkup accepts up to 100."`
	ExcludeDomains   []string       `json:"exclude_domains,omitempty" jsonschema:"description=Exclude these domains."`
	FromDate         string         `json:"from_date,omitempty" jsonschema:"description=ISO date lower bound."`
	ToDate           string         `json:"to_date,omitempty" jsonschema:"description=ISO date upper bound."`
	IncludeImages    bool           `json:"include_images,omitempty" jsonschema:"description=Include image results when visually relevant."`
	StructuredSchema map[string]any `json:"structured_schema,omitempty" jsonschema:"description=Optional object-rooted JSON schema. When present, Linkup returns structured output plus its sources instead of ordinary search results."`
}

type LinkupWebFetchInput struct {
	URL               string `json:"url" jsonschema:"description=Exact URL to retrieve as clean Markdown."`
	RenderJS          bool   `json:"render_js,omitempty" jsonschema:"description=Execute page JavaScript before extraction for client-rendered sites."`
	IncludeRawContent bool   `json:"include_raw_content,omitempty" jsonschema:"description=Also return the raw response content and content type when exact markup or data is needed."`
	ExtractImages     bool   `json:"extract_images,omitempty" jsonschema:"description=Return the page's extracted image list."`
}

type LinkupWebAnswerInput struct {
	Query                  string   `json:"query" jsonschema:"description=Natural-language question for an independent Linkup synthesis with sources."`
	Depth                  string   `json:"depth,omitempty" jsonschema:"description=fast, standard, or deep. Defaults to standard."`
	MaxResults             int      `json:"max_results,omitempty" jsonschema:"description=Maximum number of supporting results."`
	IncludeDomains         []string `json:"include_domains,omitempty" jsonschema:"description=Only use these domains; Linkup accepts up to 100."`
	ExcludeDomains         []string `json:"exclude_domains,omitempty" jsonschema:"description=Exclude these domains."`
	FromDate               string   `json:"from_date,omitempty" jsonschema:"description=ISO date lower bound."`
	ToDate                 string   `json:"to_date,omitempty" jsonschema:"description=ISO date upper bound."`
	IncludeInlineCitations *bool    `json:"include_inline_citations,omitempty" jsonschema:"description=Embed source references in the answer. Defaults to true."`
}

func (r *Runtime) appendLinkupTools(enabled map[string]bool, tools []tool.BaseTool) ([]tool.BaseTool, error) {
	if enabled[ToolNameWebSearch] {
		value, err := toolutils.InferTool(
			ToolNameWebSearch,
			"Discover web evidence through Linkup using fast, standard, or multi-iteration deep search, with domain/date controls, images, and source-bearing structured extraction. Use web_fetch on decisive URLs.",
			r.linkupWebSearch,
		)
		if err != nil {
			return nil, err
		}
		tools = append(tools, value)
	}
	if enabled[ToolNameWebFetch] {
		value, err := toolutils.InferTool(
			ToolNameWebFetch,
			"Retrieve an exact URL through Linkup as clean Markdown, optionally rendering JavaScript and returning raw content or extracted images.",
			r.linkupWebFetch,
		)
		if err != nil {
			return nil, err
		}
		tools = append(tools, value)
	}
	if enabled[ToolNameWebAnswer] {
		value, err := toolutils.InferTool(
			ToolNameWebAnswer,
			"Ask Linkup for an independent sourced answer using fast, standard, or multi-iteration deep search. Treat the synthesis as a cross-check and retrieve decisive sources before relying on material claims.",
			r.linkupWebAnswer,
		)
		if err != nil {
			return nil, err
		}
		tools = append(tools, value)
	}
	return tools, nil
}

func (r *Runtime) linkupWebSearch(ctx context.Context, input LinkupWebSearchInput) (WebResearchResult, error) {
	query := strings.TrimSpace(input.Query)
	if query == "" {
		return WebResearchResult{}, fmt.Errorf("query is required")
	}
	depth, err := validateLinkupDepth(input.Depth)
	if err != nil {
		return WebResearchResult{}, err
	}
	if err := validateLinkupFilters(input.MaxResults, input.IncludeDomains); err != nil {
		return WebResearchResult{}, err
	}
	outputType := "searchResults"
	includeSources := false
	if len(input.StructuredSchema) > 0 {
		if input.StructuredSchema["type"] != "object" {
			return WebResearchResult{}, fmt.Errorf("structured_schema must have object at its root")
		}
		outputType = "structured"
		includeSources = true
	}
	client := linkup.Client{ResolveCredential: r.options.ResolveLinkupCredential}
	response, err := client.Search(ctx, linkup.SearchRequest{
		Query: query, Depth: depth, OutputType: outputType,
		MaxResults: input.MaxResults, IncludeImages: input.IncludeImages,
		IncludeDomains: cleanStrings(input.IncludeDomains), ExcludeDomains: cleanStrings(input.ExcludeDomains),
		FromDate: strings.TrimSpace(input.FromDate), ToDate: strings.TrimSpace(input.ToDate),
		IncludeSources: includeSources, StructuredOutputSchema: input.StructuredSchema,
	})
	if err != nil {
		return WebResearchResult{}, err
	}
	return WebResearchResult{
		Provider: "linkup", Mode: "search",
		Evidence: "Linkup search results and structured output are discovery evidence. Inspect the returned sources and retrieve decisive URLs with web_fetch before relying on material claims.",
		Response: response,
	}, nil
}

func (r *Runtime) linkupWebFetch(ctx context.Context, input LinkupWebFetchInput) (WebResearchResult, error) {
	url := strings.TrimSpace(input.URL)
	if url == "" {
		return WebResearchResult{}, fmt.Errorf("URL is required")
	}
	client := linkup.Client{ResolveCredential: r.options.ResolveLinkupCredential}
	response, err := client.Fetch(ctx, linkup.FetchRequest{
		URL: url, RenderJS: input.RenderJS,
		IncludeRawContent: input.IncludeRawContent, ExtractImages: input.ExtractImages,
	})
	if err != nil {
		return WebResearchResult{}, err
	}
	return WebResearchResult{
		Provider: "linkup", Mode: "fetch",
		Evidence: "Linkup returned content for the exact URL. Check the returned Markdown or raw content itself; an empty extraction supports no claim.",
		Response: response,
	}, nil
}

func (r *Runtime) linkupWebAnswer(ctx context.Context, input LinkupWebAnswerInput) (WebResearchResult, error) {
	query := strings.TrimSpace(input.Query)
	if query == "" {
		return WebResearchResult{}, fmt.Errorf("query is required")
	}
	depth, err := validateLinkupDepth(input.Depth)
	if err != nil {
		return WebResearchResult{}, err
	}
	if err := validateLinkupFilters(input.MaxResults, input.IncludeDomains); err != nil {
		return WebResearchResult{}, err
	}
	inline := true
	if input.IncludeInlineCitations != nil {
		inline = *input.IncludeInlineCitations
	}
	client := linkup.Client{ResolveCredential: r.options.ResolveLinkupCredential}
	response, err := client.Search(ctx, linkup.SearchRequest{
		Query: query, Depth: depth, OutputType: "sourcedAnswer",
		MaxResults: input.MaxResults, IncludeInlineCitations: inline,
		IncludeDomains: cleanStrings(input.IncludeDomains), ExcludeDomains: cleanStrings(input.ExcludeDomains),
		FromDate: strings.TrimSpace(input.FromDate), ToDate: strings.TrimSpace(input.ToDate),
	})
	if err != nil {
		return WebResearchResult{}, err
	}
	return WebResearchResult{
		Provider: "linkup", Mode: "answer",
		Evidence: "This is an independent Linkup synthesis, not ground truth. Validate material claims against its listed sources or retrieve them with web_fetch.",
		Response: response,
	}, nil
}

func validateLinkupDepth(raw string) (string, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return "standard", nil
	}
	switch value {
	case "fast", "standard", "deep":
		return value, nil
	default:
		return "", fmt.Errorf("depth must be fast, standard, or deep")
	}
}

func validateLinkupFilters(maxResults int, includeDomains []string) error {
	if maxResults < 0 {
		return fmt.Errorf("max_results cannot be negative")
	}
	if len(cleanStrings(includeDomains)) > 100 {
		return fmt.Errorf("include_domains accepts at most 100 domains")
	}
	return nil
}
