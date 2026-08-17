package tooling

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"altv1/internal/provider"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/components/tool"
	toolutils "github.com/cloudwego/eino/components/tool/utils"
)

const (
	ToolNameExecCommand   = "exec_command"
	ToolNameWriteStdin    = "write_stdin"
	ToolNameApplyPatch    = "apply_patch"
	ToolNameWebSearch     = "web_search"
	ToolNameWebFetch      = "web_fetch"
	ToolNameWebAnswer     = "web_answer"
	ToolNameContextSearch = "context_search"
	ToolNameContextBrowse = "context_browse"
	ToolNameContextOpen   = "context_open"
)

var allRuntimeTools = append(
	append([]string(nil), filesystemTools...),
	ToolNameExecCommand,
	ToolNameWriteStdin,
	ToolNameApplyPatch,
	ToolNameWebSearch,
	ToolNameWebFetch,
	ToolNameWebAnswer,
	ToolNameContextSearch,
	ToolNameContextBrowse,
	ToolNameContextOpen,
)

type ContextSearchInput struct {
	Query string `json:"query" jsonschema:"description=Terms to find in exact durable evidence available to this assignment."`
	Limit int    `json:"limit,omitempty" jsonschema:"description=Maximum matches to return; defaults to 8 and cannot exceed 25."`
}

type ContextSearchMatch struct {
	Reference      string `json:"reference"`
	SessionID      string `json:"session_id,omitempty"`
	SourceSequence int64  `json:"source_sequence"`
	Kind           string `json:"kind"`
	Actor          string `json:"actor,omitempty"`
	CorrelationID  string `json:"correlation_id,omitempty"`
	Preview        string `json:"preview"`
}

type ContextSearchResult struct {
	Matches []ContextSearchMatch `json:"matches"`
}

type ContextBrowseInput struct {
	Cursor string `json:"cursor,omitempty" jsonschema:"description=Opaque next_cursor returned by the previous context_browse call; omit for newest evidence."`
	Limit  int    `json:"limit,omitempty" jsonschema:"description=Maximum records to return; defaults to 20 and cannot exceed 50."`
}

type ContextBrowseResult struct {
	Records    []ContextSearchMatch `json:"records"`
	NextCursor string               `json:"next_cursor,omitempty"`
}

type ContextOpenInput struct {
	Reference  string `json:"reference" jsonschema:"description=An exact alt://context/records/... or alt-tool-output://... reference returned by ALT."`
	ByteOffset int    `json:"byte_offset,omitempty" jsonschema:"description=Zero-based byte offset from which to read; use next_byte_offset to continue an earlier response."`
	MaxBytes   int    `json:"max_bytes,omitempty" jsonschema:"description=Optional caller-requested byte ceiling. ALT may return a smaller page derived from the selected model's live context envelope."`
}

type ContextOpenResult struct {
	Reference      string `json:"reference"`
	SessionID      string `json:"session_id,omitempty"`
	SourceSequence int64  `json:"source_sequence"`
	OccurredAt     string `json:"occurred_at,omitempty"`
	Kind           string `json:"kind"`
	Actor          string `json:"actor,omitempty"`
	CorrelationID  string `json:"correlation_id,omitempty"`
	CausationID    string `json:"causation_id,omitempty"`
	Digest         string `json:"sha256"`
	ChunkDigest    string `json:"chunk_sha256"`
	ByteCount      int    `json:"byte_count"`
	ByteStart      int    `json:"byte_start"`
	ByteEnd        int    `json:"byte_end_exclusive"`
	HasMore        bool   `json:"has_more"`
	NextByteOffset int    `json:"next_byte_offset,omitempty"`
	Encoding       string `json:"encoding"`
	Content        string `json:"content"`
}

func ToolNames() []string {
	return append([]string(nil), allRuntimeTools...)
}

// Handlers gives every assignment one stable runtime tool catalogue. The
// catalogue is intentionally small enough to expose directly: interposing a
// model-driven discovery call would add a round trip before ordinary work and
// mutate the cache-bearing tool header between model calls. Process sessions
// remain scoped to owner, so concurrent assignments cannot write to or poll
// each other's PTYs.
func (r *Runtime) Handlers(
	ctx context.Context,
	owner string,
) ([]adk.ChatModelAgentMiddleware, error) {
	return r.handlers(ctx, owner, nil, provider.ModelLimits{})
}

// HandlersWithCompaction adds Eino's intra-agent summarization after ALT's
// exact tool-result reduction. Before any summary replaces working messages,
// ALT archives the exact permitted transcript and attaches its address.
func (r *Runtime) HandlersWithCompaction(
	ctx context.Context,
	owner string,
	summarizer model.BaseChatModel,
	limits ...provider.ModelLimits,
) ([]adk.ChatModelAgentMiddleware, error) {
	var resolved provider.ModelLimits
	if len(limits) > 0 {
		resolved = limits[0]
	}
	return r.handlers(ctx, owner, summarizer, resolved)
}

// CompactionHandlers installs the same provider-derived context recovery used
// by tool-capable agents without advertising any tools. Models whose
// authenticated catalog explicitly rejects tool calling still need overflow
// recovery and lifecycle accounting; tool capability and context capacity are
// independent facts.
func (r *Runtime) CompactionHandlers(
	ctx context.Context,
	owner string,
	summarizer model.BaseChatModel,
	limits provider.ModelLimits,
) ([]adk.ChatModelAgentMiddleware, error) {
	if r == nil {
		return nil, fmt.Errorf("tool runtime is required")
	}
	owner = strings.TrimSpace(owner)
	if owner == "" {
		return nil, fmt.Errorf("context owner is required")
	}
	budget := newContextBudget(limits)
	r.setContextBudget(owner, budget)
	compactor, err := r.contextCompactionHandler(ctx, owner, summarizer, budget)
	if err != nil {
		return nil, fmt.Errorf("create Eino context compaction: %w", err)
	}
	if compactor == nil {
		return nil, nil
	}
	return []adk.ChatModelAgentMiddleware{compactor}, nil
}

func (r *Runtime) handlers(
	ctx context.Context,
	owner string,
	summarizer model.BaseChatModel,
	limits provider.ModelLimits,
) ([]adk.ChatModelAgentMiddleware, error) {
	if r == nil {
		return nil, fmt.Errorf("tool runtime is required")
	}
	owner = strings.TrimSpace(owner)
	if owner == "" {
		return nil, fmt.Errorf("tool owner is required")
	}
	enabled := make(map[string]bool, len(allRuntimeTools))
	for _, name := range allRuntimeTools {
		enabled[name] = true
	}

	budget := newContextBudget(limits)
	r.setContextBudget(owner, budget)
	filesystemTools, filesystemInstruction, reducer, err := r.filesystemTools(ctx, owner, budget)
	if err != nil {
		return nil, err
	}
	nativeTools, err := r.nativeTools(ctx, owner, enabled)
	if err != nil {
		return nil, err
	}
	runtimeTools := append([]tool.BaseTool(nil), filesystemTools...)
	runtimeTools = append(runtimeTools, nativeTools...)
	handlers := []adk.ChatModelAgentMiddleware{
		&toolMiddleware{
			BaseChatModelAgentMiddleware: &adk.BaseChatModelAgentMiddleware{},
			instruction:                  filesystemInstruction,
			tools:                        runtimeTools,
		},
		reducer,
	}
	compactor, err := r.contextCompactionHandler(ctx, owner, summarizer, budget)
	if err != nil {
		return nil, fmt.Errorf("create Eino context compaction: %w", err)
	}
	if compactor != nil {
		handlers = append(handlers, compactor)
	}
	if len(nativeTools) > 0 {
		executionPolicy := "exec_command runs inside ALT's fail-closed Linux sandbox: Bubblewrap isolates filesystem mounts, processes, IPC, UTS, and direct networking; no_new_privs and Landlock restrict privilege transitions and writes. It may read the host filesystem, but may write only the session workspace and ALT's private temporary directory."
		if r.options.DangerouslyBypassApprovalsAndSandbox {
			executionPolicy = "DANGEROUS MODE: exec_command bypasses ALT's approval and sandbox boundary and executes directly on the host. Treat every command as having the user's ambient operating-system authority."
		}
		handlers[0] = &toolMiddleware{
			BaseChatModelAgentMiddleware: &adk.BaseChatModelAgentMiddleware{},
			instruction:                  filesystemInstruction + "\n" + executionPolicy + " Running commands return a numeric session_id for write_stdin; copy that exact ID when polling or sending input. An unknown-process lookup does not prove that the process ended: if the ID was transcribed incorrectly, retry with the exact session_id from the latest running result. apply_patch accepts strict Git or standard unified text patches and validates every path against the workspace before changing files. Runtime tools are directly available; call the one required by the work without a discovery step. web_search discovers sources, web_fetch retrieves exact source content, and web_answer supplies an independent cited synthesis. Search output and generated answers are not substitutes for checking decisive claims against fetched primary evidence. Credentials are never exposed to shell commands.",
			tools:                        runtimeTools,
		}
	}
	return handlers, nil
}

func (r *Runtime) nativeTools(ctx context.Context, owner string, enabled map[string]bool) ([]tool.BaseTool, error) {
	var tools []tool.BaseTool
	if enabled[ToolNameExecCommand] {
		value, err := toolutils.InferTool(
			ToolNameExecCommand,
			"Execute a shell command in the session workspace. A session_id is returned while the process is running or when completed output has_more pages; drain exact remaining pages with write_stdin.",
			func(ctx context.Context, input ExecCommandInput) (ProcessResult, error) {
				return r.processes.start(ctx, owner, input)
			},
		)
		if err != nil {
			return nil, err
		}
		tools = append(tools, value)
	}
	if enabled[ToolNameWriteStdin] {
		value, err := toolutils.InferTool(
			ToolNameWriteStdin,
			"Write exact characters to, poll, or drain has_more output pages from an exec_command session owned by this assignment.",
			func(ctx context.Context, input WriteStdinInput) (ProcessResult, error) {
				return r.processes.write(ctx, owner, input)
			},
		)
		if err != nil {
			return nil, err
		}
		tools = append(tools, value)
	}
	if enabled[ToolNameApplyPatch] {
		applier := &patchApplier{root: r.root}
		value, err := toolutils.InferTool(
			ToolNameApplyPatch,
			"Apply a strict Git or standard unified text patch transactionally inside the session workspace. The complete patch must contain file headers and hunks.",
			applier.apply,
		)
		if err != nil {
			return nil, err
		}
		tools = append(tools, value)
	}
	if enabled[ToolNameContextSearch] {
		value, err := toolutils.InferTool(
			ToolNameContextSearch,
			"Search ALT's exact durable context archive within this assignment's authority. Use it when a bounded working view points to older evidence or a prior detail becomes relevant.",
			func(ctx context.Context, input ContextSearchInput) (ContextSearchResult, error) {
				if r.options.SearchContext == nil {
					return ContextSearchResult{}, fmt.Errorf("durable context search is unavailable")
				}
				if input.Limit == 0 {
					input.Limit = 8
				}
				if input.Limit < 1 || input.Limit > 25 {
					return ContextSearchResult{}, fmt.Errorf("context search limit must be within [1,25]")
				}
				return r.options.SearchContext(ctx, owner, input)
			},
		)
		if err != nil {
			return nil, err
		}
		tools = append(tools, value)
	}
	if enabled[ToolNameContextBrowse] {
		value, err := toolutils.InferTool(
			ToolNameContextBrowse,
			"Browse the newest durable context occurrences available to this assignment without guessing search terms. Follow next_cursor to move backward, then use context_open for exact bytes.",
			func(ctx context.Context, input ContextBrowseInput) (ContextBrowseResult, error) {
				if r.options.BrowseContext == nil {
					return ContextBrowseResult{}, fmt.Errorf("durable context browsing is unavailable")
				}
				if input.Limit == 0 {
					input.Limit = 20
				}
				if input.Limit < 1 || input.Limit > 50 {
					return ContextBrowseResult{}, fmt.Errorf("context browse limit must be within [1,50]")
				}
				return r.options.BrowseContext(ctx, owner, input)
			},
		)
		if err != nil {
			return nil, err
		}
		tools = append(tools, value)
	}
	if enabled[ToolNameContextOpen] {
		value, err := toolutils.InferTool(
			ToolNameContextOpen,
			"Read a bounded exact byte range from one immutable ALT context occurrence. Continue with next_byte_offset when has_more is true; sha256 identifies the whole occurrence and chunk_sha256 verifies the returned range.",
			func(ctx context.Context, input ContextOpenInput) (ContextOpenResult, error) {
				if r.options.OpenContext == nil {
					return ContextOpenResult{}, fmt.Errorf("durable context recall is unavailable")
				}
				if input.ByteOffset < 0 {
					return ContextOpenResult{}, fmt.Errorf("context open byte_offset cannot be negative")
				}
				if input.MaxBytes < 0 {
					return ContextOpenResult{}, fmt.Errorf("context open max_bytes cannot be negative")
				}
				return r.openContextForModel(ctx, owner, input)
			},
		)
		if err != nil {
			return nil, err
		}
		tools = append(tools, value)
	}
	researchProvider, researchErr := r.researchProvider(ctx)
	if researchErr != nil && (enabled[ToolNameWebSearch] || enabled[ToolNameWebFetch] || enabled[ToolNameWebAnswer]) {
		return r.nativeToolsWithUnavailableResearch(owner, enabled, tools, researchErr)
	}
	if enabled[ToolNameWebSearch] && researchProvider == "exa" {
		value, err := toolutils.InferTool(
			ToolNameWebSearch,
			"Discover web sources through Exa. Supports fast through deep-reasoning search, domain/date/category controls, alternate deep queries, current content extraction, subpage discovery, and grounded synthesized output. Use web_fetch to verify decisive sources.",
			r.exaWebSearch,
		)
		if err != nil {
			return nil, err
		}
		tools = append(tools, value)
	}
	if enabled[ToolNameWebFetch] && researchProvider == "exa" {
		value, err := toolutils.InferTool(
			ToolNameWebFetch,
			"Retrieve exact URLs or Exa document IDs with configurable text, highlights, structured summaries, freshness, semantic sections, subpages, and outgoing links. Use returned statuses and page content as evidence.",
			r.exaWebFetch,
		)
		if err != nil {
			return nil, err
		}
		tools = append(tools, value)
	}
	if enabled[ToolNameWebAnswer] && researchProvider == "exa" {
		value, err := toolutils.InferTool(
			ToolNameWebAnswer,
			"Ask Exa for an independent answer with citations, optionally structured by JSON Schema. Treat it as a cross-check and inspect or fetch the cited sources before relying on material claims.",
			r.exaWebAnswer,
		)
		if err != nil {
			return nil, err
		}
		tools = append(tools, value)
	}
	if researchProvider == "linkup" {
		return r.appendLinkupTools(enabled, tools)
	}
	return tools, nil
}

func (r *Runtime) openContextForModel(
	ctx context.Context,
	owner string,
	input ContextOpenInput,
) (ContextOpenResult, error) {
	available, known := r.modelVisibleResultBytes(owner)
	requested := input.MaxBytes
	if known && (requested == 0 || requested > available) {
		requested = available
	}
	if known && requested <= 0 {
		return ContextOpenResult{}, fmt.Errorf("the current model envelope has no safe room for an exact context page; allow ALT to compact the working trajectory and retry")
	}
	input.MaxBytes = requested
	result, err := r.options.OpenContext(ctx, owner, input)
	if err != nil || !known {
		return result, err
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		return ContextOpenResult{}, fmt.Errorf("measure exact context page: %w", err)
	}
	if len(encoded) <= available {
		return result, nil
	}

	// Metadata and base64 expansion vary by occurrence. Binary-search the
	// actual serialized response rather than subtracting a guessed header or
	// assuming a chars-per-token ratio.
	low, high := 1, requested
	var best ContextOpenResult
	for low <= high {
		candidateBytes := low + (high-low)/2
		candidateInput := input
		candidateInput.MaxBytes = candidateBytes
		candidate, candidateErr := r.options.OpenContext(ctx, owner, candidateInput)
		if candidateErr != nil {
			return ContextOpenResult{}, candidateErr
		}
		wire, marshalErr := json.Marshal(candidate)
		if marshalErr != nil {
			return ContextOpenResult{}, fmt.Errorf("measure exact context page: %w", marshalErr)
		}
		if len(wire) <= available && candidate.ByteEnd > candidate.ByteStart {
			best = candidate
			low = candidateBytes + 1
		} else {
			high = candidateBytes - 1
		}
	}
	if best.ByteEnd <= best.ByteStart {
		return ContextOpenResult{}, fmt.Errorf("the current model envelope cannot hold even the smallest exact context page and its provenance")
	}
	return best, nil
}

type unavailableResearchInput struct {
	Query string   `json:"query,omitempty" jsonschema:"description=Research query, retained so the setup error can explain how to enable web research."`
	URLs  []string `json:"urls,omitempty" jsonschema:"description=Exact URLs intended for retrieval."`
}

func (r *Runtime) researchProvider(ctx context.Context) (string, error) {
	if r.options.ResolveResearchProvider == nil {
		if r.options.ResolveExaCredential != nil {
			return "exa", nil
		}
		return "", fmt.Errorf("web research is not configured; choose a provider with /research")
	}
	provider, err := r.options.ResolveResearchProvider(ctx)
	if err != nil {
		return "", err
	}
	switch provider {
	case "exa", "linkup":
		return provider, nil
	default:
		return "", fmt.Errorf("unsupported selected research provider %q", provider)
	}
}

// ProviderForTool pins the provider used by a provider-backed tool into the
// durable call event. It is deliberately empty for local tools and when setup
// is incomplete; execution still returns the authoritative setup error.
func (r *Runtime) ProviderForTool(ctx context.Context, name string) string {
	switch name {
	case ToolNameWebSearch, ToolNameWebFetch, ToolNameWebAnswer:
		provider, err := r.researchProvider(ctx)
		if err == nil {
			return provider
		}
	}
	return ""
}

func (r *Runtime) nativeToolsWithUnavailableResearch(
	owner string,
	enabled map[string]bool,
	tools []tool.BaseTool,
	researchErr error,
) ([]tool.BaseTool, error) {
	_ = owner
	for _, specification := range []struct {
		name        string
		description string
	}{
		{ToolNameWebSearch, "Web source discovery is unavailable until the user configures and selects a research provider."},
		{ToolNameWebFetch, "Exact web retrieval is unavailable until the user configures and selects a research provider."},
		{ToolNameWebAnswer, "Independent cited web synthesis is unavailable until the user configures and selects a research provider."},
	} {
		if !enabled[specification.name] {
			continue
		}
		value, err := toolutils.InferTool(
			specification.name,
			specification.description,
			func(context.Context, unavailableResearchInput) (WebResearchResult, error) {
				return WebResearchResult{}, researchErr
			},
		)
		if err != nil {
			return nil, err
		}
		tools = append(tools, value)
	}
	return tools, nil
}

type toolMiddleware struct {
	*adk.BaseChatModelAgentMiddleware
	instruction string
	tools       []tool.BaseTool
}

func (m *toolMiddleware) BeforeAgent(
	ctx context.Context,
	runCtx *adk.ChatModelAgentContext,
) (context.Context, *adk.ChatModelAgentContext, error) {
	if runCtx == nil {
		return ctx, nil, nil
	}
	copy := *runCtx
	if m.instruction != "" {
		copy.Instruction += "\n" + m.instruction
	}
	copy.Tools = append(copy.Tools, m.tools...)
	return ctx, &copy, nil
}
