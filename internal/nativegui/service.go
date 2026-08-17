package nativegui

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"altv1/internal/application"
	"altv1/internal/event"
	"altv1/internal/profile"
	"altv1/internal/provider"
	"altv1/internal/store"
	"altv1/internal/thinking"
)

type Launch struct {
	Mode      Mode
	ProfileID string
	Revision  int
	SessionID string
}

type Host struct {
	mu          sync.Mutex
	ctx         context.Context
	app         *application.Application
	launch      Launch
	catalog     []provider.CatalogModel
	draft       *TeamDraft
	thinking    *thinking.Projection
	published   *Published
	initError   string
	streamError string
	teamView    TeamView
	profiles    []store.ProfileSummary

	preparedRequest  []byte
	preparedResponse []byte
}

func (h *Host) loadCatalog(gateway string) error {
	gateway = strings.ToLower(strings.TrimSpace(gateway))
	if _, err := h.app.Providers.Descriptor(gateway); err != nil {
		return err
	}
	items, err := h.app.Providers.Catalog(h.ctx, gateway)
	if err != nil {
		return err
	}
	h.catalog = items
	return nil
}

func NewHost(ctx context.Context, app *application.Application, launch Launch) (*Host, error) {
	host := &Host{ctx: ctx, app: app, launch: launch}
	switch launch.Mode {
	case ModeTeam:
		profiles, err := app.Store.ListProfiles(ctx)
		if err != nil {
			return nil, err
		}
		host.profiles = profiles
		if launch.ProfileID == "" {
			draft := NewDraft()
			host.draft = &draft
			host.teamView = TeamViewNew
		} else {
			document, err := app.Store.Profile(ctx, launch.ProfileID, launch.Revision)
			if err != nil {
				return nil, err
			}
			if err := host.loadCatalog(document.Profile.Gateway); err != nil {
				host.initError = err.Error()
			}
			draft := DraftFromProfile(document.Profile, host.catalog)
			host.draft = &draft
			host.teamView = TeamViewInspect
		}
	case ModeThinking:
		if launch.SessionID == "" {
			return nil, fmt.Errorf("thinking graph requires a session")
		}
		turns, err := app.Store.ConversationSessions(ctx, launch.SessionID)
		if err != nil {
			return nil, err
		}
		if len(turns) == 0 {
			return nil, fmt.Errorf("thinking graph requires a non-empty session")
		}
		document, err := app.Store.Profile(ctx, turns[0].ProfileID, turns[0].ProfileRevision)
		if err != nil {
			return nil, err
		}
		projection := thinking.New(turns[0].ConversationID, document.Profile)
		for _, turn := range turns {
			if err := projection.AddTurn(turn); err != nil {
				return nil, err
			}
			events, err := app.Store.Events(ctx, turn.ID, 0)
			if err != nil {
				return nil, err
			}
			for _, item := range events {
				if err := projection.Apply(item); err != nil {
					return nil, err
				}
			}
		}
		host.thinking = projection
	default:
		return nil, fmt.Errorf("unsupported native GUI mode %q", launch.Mode)
	}
	return host, nil
}

func (h *Host) Exchange(source []byte) []byte {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.exchangeBytes(source)
}

// ExchangeSized implements a two-phase native boundary without a fixed
// response ceiling. A zero capacity prepares exactly one response. The next
// call with the same request returns that frozen response, so a state-changing
// operation such as publication is never executed twice merely to size a
// buffer.
func (h *Host) ExchangeSized(source []byte, capacity int) []byte {
	h.mu.Lock()
	defer h.mu.Unlock()
	if capacity == 0 {
		h.preparedRequest = append(h.preparedRequest[:0], source...)
		h.preparedResponse = append(h.preparedResponse[:0], h.exchangeBytes(source)...)
		return h.preparedResponse
	}
	if bytes.Equal(source, h.preparedRequest) && h.preparedResponse != nil {
		response := h.preparedResponse
		if len(response) <= capacity {
			h.preparedRequest = nil
			h.preparedResponse = nil
		}
		return response
	}
	return h.exchangeBytes(source)
}

func (h *Host) exchangeBytes(source []byte) []byte {
	var request Request
	if err := json.Unmarshal(source, &request); err != nil {
		return mustJSON(Response{OK: false, Error: "decode GUI request: " + err.Error()})
	}
	response := h.exchange(request)
	return mustJSON(response)
}

func (h *Host) exchange(request Request) Response {
	switch request.Operation {
	case "init":
		initial := &InitialState{
			Mode:     h.launch.Mode,
			View:     h.teamView,
			Catalog:  append([]provider.CatalogModel(nil), h.catalog...),
			Gateways: append([]provider.GatewayDescriptor(nil), h.app.Providers.Descriptors()...),
			Profiles: append([]store.ProfileSummary(nil), h.profiles...),
			Runtime: RuntimeCapabilities{
				DangerouslyBypassApprovalsAndSandbox: h.app.RuntimePolicy.DangerouslyBypassApprovalsAndSandbox,
				FilesystemConfinement:                h.app.RuntimePolicy.FilesystemConfinement,
				DirectTerminalNetwork:                h.app.RuntimePolicy.DirectTerminalNetwork,
				ExaConfigured:                        h.app.RuntimePolicy.ExaConfigured,
				LinkupConfigured:                     h.app.RuntimePolicy.LinkupConfigured,
				ResearchProvider:                     h.app.RuntimePolicy.ResearchProvider,
				Tools:                                append([]string(nil), h.app.RuntimePolicy.Tools...),
			},
		}
		if h.draft != nil {
			copy := *h.draft
			initial.Draft = &copy
			initial.Diagnostics = DiagnosticsForDraft(copy, h.catalog)
		}
		if h.thinking != nil {
			initial.Thinking = h.thinking
		}
		return Response{OK: true, Error: h.initError, Initial: initial}
	case "team.open":
		if h.launch.Mode != ModeTeam {
			return Response{OK: false, Error: "team surface is not active"}
		}
		view := request.View
		if view == TeamViewNew {
			draft := NewDraft()
			h.draft = &draft
			h.catalog = nil
			h.initError = ""
			h.teamView = view
			return h.exchange(Request{Operation: "init"})
		}
		if view != TeamViewEdit && view != TeamViewInspect {
			return Response{OK: false, Error: "unknown Team view " + string(view)}
		}
		if request.ProfileID == "" {
			return Response{OK: false, Error: "choose a Team revision first"}
		}
		document, err := h.app.Store.Profile(h.ctx, request.ProfileID, request.Revision)
		if err != nil {
			return Response{OK: false, Error: err.Error()}
		}
		if err := h.loadCatalog(document.Profile.Gateway); err != nil {
			h.initError = err.Error()
		} else {
			h.initError = ""
		}
		draft := DraftFromProfile(document.Profile, h.catalog)
		h.draft = &draft
		h.teamView = view
		return h.exchange(Request{Operation: "init"})
	case "team.gateway":
		if h.launch.Mode != ModeTeam || h.teamView == TeamViewInspect {
			return Response{OK: false, Error: "team inspection is read-only"}
		}
		gateway := strings.ToLower(strings.TrimSpace(request.Gateway))
		if gateway == "" {
			return Response{OK: false, Error: "choose a gateway account first"}
		}
		if err := h.loadCatalog(gateway); err != nil {
			return Response{OK: false, Error: err.Error()}
		}
		if request.Draft != nil {
			copy := *request.Draft
			h.draft = &copy
		} else if h.draft == nil {
			draft := NewDraft()
			h.draft = &draft
		}
		if h.draft.Gateway != gateway {
			h.draft.Gateway = gateway
			h.draft.Primary.Model = ModelChoice{}
			for index := range h.draft.Peers {
				h.draft.Peers[index].Model = ModelChoice{}
			}
			for index := range h.draft.Specialists {
				h.draft.Specialists[index].Model = ModelChoice{}
			}
		}
		h.initError = ""
		return h.exchange(Request{Operation: "init"})
	case "team.validate":
		if h.launch.Mode != ModeTeam || h.teamView == TeamViewInspect {
			return Response{OK: false, Error: "team inspection is read-only"}
		}
		if request.Draft == nil {
			return Response{OK: false, Error: "team draft is required"}
		}
		diagnostics := DiagnosticsForDraft(*request.Draft, h.catalog)
		return Response{OK: !hasErrors(diagnostics), Diagnostics: diagnostics}
	case "team.publish":
		if h.launch.Mode != ModeTeam || h.teamView == TeamViewInspect {
			return Response{OK: false, Error: "team inspection is read-only"}
		}
		if request.Draft == nil {
			return Response{OK: false, Error: "team draft is required"}
		}
		diagnostics := DiagnosticsForDraft(*request.Draft, h.catalog)
		if hasErrors(diagnostics) {
			return Response{OK: false, Error: "team has deterministic validation errors", Diagnostics: diagnostics}
		}
		document, err := h.app.Store.PublishProfile(
			h.ctx,
			request.Draft.Profile(),
			request.Draft.BaseRevision,
		)
		if err != nil {
			return Response{OK: false, Error: err.Error(), Diagnostics: diagnostics}
		}
		h.draft = request.Draft
		h.draft.BaseRevision = document.Profile.Revision
		h.published = &Published{
			ID: document.Profile.ID, Revision: document.Profile.Revision, Digest: document.Digest,
		}
		return Response{OK: true, Published: h.published, Diagnostics: diagnostics}
	case "thinking.snapshot":
		if h.thinking == nil {
			return Response{OK: false, Error: "thinking graph is not active"}
		}
		return Response{OK: h.streamError == "", Error: h.streamError, Thinking: h.thinking}
	default:
		return Response{OK: false, Error: "unknown GUI operation " + request.Operation}
	}
}

// PushEvent is the live transport used by the TUI process. Sequence handling
// remains in the projection, so a later durable reconciliation safely
// deduplicates anything already delivered through the pipe.
func (h *Host) PushEvent(item event.Event) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.thinking == nil {
		return fmt.Errorf("thinking graph is not active")
	}
	if !h.thinking.HasTurn(item.SessionID) {
		record, err := h.app.Store.Session(h.ctx, item.SessionID)
		if err != nil {
			return err
		}
		if err := h.thinking.AddTurn(*record); err != nil {
			return err
		}
	}
	current := h.thinking.TurnSequence(item.SessionID)
	if item.Sequence > current+1 {
		// Do not advance past a hole. Replay the durable range first, including
		// the pushed event if its transaction is already visible.
		if err := h.reconcileLocked(); err != nil {
			return err
		}
	}
	return h.thinking.Apply(item)
}

// Reconcile repairs a missed pipe event from the durable ledger. It is not the
// live update mechanism; callers wake the renderer only when this changes the
// projection.
func (h *Host) Reconcile() (bool, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	before := h.thinking.Revision
	if err := h.reconcileLocked(); err != nil {
		h.streamError = err.Error()
		return false, err
	}
	h.streamError = ""
	return h.thinking.Revision != before, nil
}

func (h *Host) SetStreamError(err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if err == nil {
		h.streamError = ""
		return
	}
	h.streamError = err.Error()
}

func (h *Host) reconcileLocked() error {
	turns, err := h.app.Store.ConversationSessions(h.ctx, h.launch.SessionID)
	if err != nil {
		return err
	}
	for _, turn := range turns {
		if !h.thinking.HasTurn(turn.ID) {
			if err := h.thinking.AddTurn(turn); err != nil {
				return err
			}
		}
		events, err := h.app.Store.Events(h.ctx, turn.ID, h.thinking.TurnSequence(turn.ID))
		if err != nil {
			return err
		}
		for _, item := range events {
			if err := h.thinking.Apply(item); err != nil {
				return err
			}
		}
	}
	return nil
}

func (h *Host) Published() *Published {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.published == nil {
		return nil
	}
	copy := *h.published
	return &copy
}

func mustJSON(value any) []byte {
	source, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return source
}

// ProfileDocument is used by the TUI when the GUI child reports a publish.
func ProfileDocument(ctx context.Context, app *application.Application, result Published) (*profile.Document, error) {
	return app.Store.Profile(ctx, result.ID, result.Revision)
}
