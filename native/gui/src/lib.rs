mod flow_router;
mod layout_engine;

use eframe::egui;
use egui_graph::node::EdgeEvent;
use egui_graph::{route_edges, EdgeRoutes, LayoutNode, LayoutParams, NodeId, SocketKind};
use serde::{Deserialize, Serialize};
use std::collections::{BTreeMap, HashMap, HashSet};
use std::hash::{Hash, Hasher};
use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::{Arc, LazyLock, Mutex};
use std::time::Duration;
use std::time::{SystemTime, UNIX_EPOCH};

type EdgeEndpoint = (NodeId, usize);
type EdgeEndpointPair = (EdgeEndpoint, EdgeEndpoint);

#[derive(Clone, Debug)]
struct PortEdge {
    key: String,
    from: NodeId,
    to: NodeId,
}

#[derive(Clone, Debug, Default)]
struct PortLayout {
    endpoints: HashMap<String, EdgeEndpointPair>,
    inputs: HashMap<NodeId, usize>,
    outputs: HashMap<NodeId, usize>,
}

/// Assign each incident edge its own boundary port. For a top-down graph,
/// ports are ordered along the node's horizontal boundary by the opposite
/// endpoint's horizontal position. This is the local two-layer crossing
/// minimum: reversing either order creates a crossing immediately outside
/// the node. Stable keys resolve geometrically coincident endpoints.
fn assign_edge_ports(edges: &[PortEdge], centers: &HashMap<NodeId, egui::Pos2>) -> PortLayout {
    let mut result = PortLayout::default();
    let mut outgoing: BTreeMap<NodeId, Vec<usize>> = BTreeMap::new();
    let mut incoming: BTreeMap<NodeId, Vec<usize>> = BTreeMap::new();

    for (index, edge) in edges.iter().enumerate() {
        result
            .endpoints
            .insert(edge.key.clone(), ((edge.from, 0), (edge.to, 0)));
        outgoing.entry(edge.from).or_default().push(index);
        incoming.entry(edge.to).or_default().push(index);
    }

    let center = |id: NodeId| centers.get(&id).copied().unwrap_or(egui::Pos2::ZERO);
    for (node, indices) in &mut outgoing {
        indices.sort_by(|&a, &b| {
            let a_edge = &edges[a];
            let b_edge = &edges[b];
            let a_center = center(a_edge.to);
            let b_center = center(b_edge.to);
            a_center
                .x
                .total_cmp(&b_center.x)
                .then_with(|| a_center.y.total_cmp(&b_center.y))
                .then_with(|| a_edge.to.cmp(&b_edge.to))
                .then_with(|| a_edge.key.cmp(&b_edge.key))
        });
        result.outputs.insert(*node, indices.len());
        for (port, edge_index) in indices.iter().copied().enumerate() {
            result
                .endpoints
                .get_mut(&edges[edge_index].key)
                .expect("edge endpoint initialized")
                .0
                 .1 = port;
        }
    }
    for (node, indices) in &mut incoming {
        indices.sort_by(|&a, &b| {
            let a_edge = &edges[a];
            let b_edge = &edges[b];
            let a_center = center(a_edge.from);
            let b_center = center(b_edge.from);
            a_center
                .x
                .total_cmp(&b_center.x)
                .then_with(|| a_center.y.total_cmp(&b_center.y))
                .then_with(|| a_edge.from.cmp(&b_edge.from))
                .then_with(|| a_edge.key.cmp(&b_edge.key))
        });
        result.inputs.insert(*node, indices.len());
        for (port, edge_index) in indices.iter().copied().enumerate() {
            result
                .endpoints
                .get_mut(&edges[edge_index].key)
                .expect("edge endpoint initialized")
                .1
                 .1 = port;
        }
    }
    result
}

unsafe extern "C" {
    fn alt_gui_host_exchange(
        handle: u64,
        request: *const u8,
        request_length: usize,
        response: *mut u8,
        response_capacity: usize,
    ) -> i64;
}

struct LiveSignal {
    context: egui::Context,
    generation: Arc<AtomicU64>,
}

static LIVE_SIGNALS: LazyLock<Mutex<HashMap<u64, LiveSignal>>> =
    LazyLock::new(|| Mutex::new(HashMap::new()));

/// Notify an already-open native surface that its authoritative Go projection
/// changed. This is called on the event-delivery thread; egui's Context may be
/// cloned and request_repaint is thread-safe.
#[no_mangle]
pub extern "C" fn alt_native_gui_wake(handle: u64) -> u8 {
    let signals = LIVE_SIGNALS.lock().expect("live signal mutex poisoned");
    let Some(signal) = signals.get(&handle) else {
        return 0;
    };
    signal.generation.fetch_add(1, Ordering::Release);
    signal.context.request_repaint();
    1
}

#[cfg(test)]
static TEST_HOST_EXCHANGES: std::sync::atomic::AtomicUsize = std::sync::atomic::AtomicUsize::new(0);

#[cfg(test)]
#[export_name = "alt_gui_host_exchange"]
pub extern "C" fn test_alt_gui_host_exchange(
    _handle: u64,
    _request: *const u8,
    _request_length: usize,
    _response: *mut u8,
    _response_capacity: usize,
) -> i64 {
    TEST_HOST_EXCHANGES.fetch_add(1, std::sync::atomic::Ordering::SeqCst);
    0
}

#[derive(Serialize)]
struct HostRequest<'a> {
    operation: &'a str,
    #[serde(skip_serializing_if = "Option::is_none")]
    draft: Option<&'a TeamDraft>,
}

#[derive(Debug, Deserialize)]
struct HostResponse {
    ok: bool,
    #[serde(default)]
    error: String,
    initial: Option<InitialState>,
    #[serde(default)]
    diagnostics: Vec<Diagnostic>,
    published: Option<Published>,
    thinking: Option<ThinkingProjection>,
}

#[derive(Debug, Deserialize)]
struct InitialState {
    mode: String,
    #[serde(default)]
    runtime: RuntimeCapabilities,
    #[serde(default)]
    catalog: Vec<CatalogModel>,
    draft: Option<TeamDraft>,
    thinking: Option<ThinkingProjection>,
    #[serde(default)]
    diagnostics: Vec<Diagnostic>,
}

#[derive(Clone, Debug, Default, Deserialize)]
struct RuntimeCapabilities {
    dangerously_bypass_approvals_and_sandbox: bool,
    filesystem_confinement: bool,
    direct_terminal_network: bool,
    exa_configured: bool,
    #[serde(default)]
    tools: Vec<String>,
}

#[derive(Clone, Debug, Deserialize)]
struct CatalogModel {
    gateway: String,
    route: String,
    id: String,
}

#[derive(Clone, Debug, Default, Deserialize, Serialize)]
struct TeamDraft {
    id: String,
    name: String,
    base_revision: i32,
    router: DraftAssignment,
    #[serde(default)]
    members: Vec<DraftMember>,
    #[serde(default)]
    call_edges: Vec<CallEdge>,
}

#[derive(Clone, Debug, Default, Deserialize, Serialize)]
struct DraftAssignment {
    model: ModelChoice,
    definition: String,
}

#[derive(Clone, Debug, Default, Deserialize, Serialize)]
struct DraftMember {
    id: String,
    model: ModelChoice,
    definition: String,
    lead: bool,
}

#[derive(Clone, Debug, Default, Deserialize, Serialize)]
struct CallEdge {
    lead_id: String,
    member_id: String,
}

#[derive(Clone, Debug, Default, Deserialize, Serialize)]
struct ModelChoice {
    gateway: String,
    route: String,
    id: String,
}

#[derive(Clone, Debug, Deserialize)]
struct Diagnostic {
    severity: String,
    path: String,
    message: String,
}

#[derive(Debug, Deserialize)]
struct Published {
    id: String,
    revision: i32,
    digest: String,
}

#[derive(Clone, Debug, Default, Deserialize)]
struct ThinkingProjection {
    session_id: String,
    active_turn_id: String,
    revision: u64,
    #[serde(default)]
    turns: Vec<ThinkingTurnSummary>,
    #[serde(default)]
    active: ThinkingTurn,
}

#[derive(Clone, Debug, Default, Deserialize)]
struct ThinkingTurnSummary {
    id: String,
    ordinal: usize,
    task: String,
    status: String,
    sequence: i64,
}

#[derive(Clone, Debug, Default, Deserialize)]
struct ThinkingTurn {
    id: String,
    ordinal: usize,
    task: String,
    status: String,
    sequence: i64,
    #[serde(default)]
    nodes: BTreeMap<String, ThinkingNode>,
    #[serde(default)]
    edges: BTreeMap<String, ThinkingEdge>,
}

#[derive(Clone, Debug, Deserialize)]
struct ThinkingNode {
    id: String,
    kind: String,
    label: String,
    status: String,
    #[serde(default)]
    actor: String,
    #[serde(default)]
    metadata: BTreeMap<String, String>,
}

#[derive(Clone, Debug, Deserialize)]
struct ThinkingEdge {
    id: String,
    from: String,
    to: String,
    kind: String,
    direction: String,
    status: String,
    #[serde(default)]
    count: usize,
    #[serde(default)]
    active: usize,
    #[serde(default)]
    started_at_ms: i64,
    #[serde(default)]
    #[allow(dead_code)]
    // Retained for future edge inspection without changing the wire schema.
    metadata: BTreeMap<String, String>,
}

fn host_exchange(
    handle: u64,
    operation: &str,
    draft: Option<&TeamDraft>,
) -> Result<HostResponse, String> {
    let request = serde_json::to_vec(&HostRequest { operation, draft })
        .map_err(|error| format!("encode host request: {error}"))?;
    let required = unsafe {
        alt_gui_host_exchange(
            handle,
            request.as_ptr(),
            request.len(),
            std::ptr::null_mut(),
            0,
        )
    };
    if required >= 0 {
        return Err(format!(
            "host sizing probe returned {required}; expected the exact required byte count"
        ));
    }
    let mut response = vec![0_u8; (-required) as usize];
    let written = unsafe {
        alt_gui_host_exchange(
            handle,
            request.as_ptr(),
            request.len(),
            response.as_mut_ptr(),
            response.len(),
        )
    };
    if written < 0 {
        return Err(format!(
            "host response size changed after preparation ({} bytes now required)",
            -written
        ));
    }
    if written as usize != response.len() {
        return Err(format!(
            "host wrote {written} bytes after preparing {}",
            response.len()
        ));
    }
    response.truncate(written as usize);
    serde_json::from_slice(&response).map_err(|error| format!("decode host response: {error}"))
}

#[no_mangle]
pub extern "C" fn alt_native_gui_run(handle: u64) -> i32 {
    let initial = match host_exchange(handle, "init", None) {
        Ok(response) => response,
        Err(error) => return run_error_window(error),
    };
    let Some(state) = initial.initial else {
        return run_error_window(if initial.error.is_empty() {
            "ALT did not provide an initial GUI state".to_owned()
        } else {
            initial.error
        });
    };
    let title = match state.mode.as_str() {
        "thinking" => "ALT — Thinking",
        "team-inspect" => "ALT — Team",
        "team-edit" => "ALT — Edit Team",
        _ => "ALT — New Team",
    };
    let options = eframe::NativeOptions {
        viewport: egui::ViewportBuilder::default()
            .with_inner_size([1320.0, 820.0])
            .with_min_inner_size([920.0, 620.0])
            .with_active(true),
        ..Default::default()
    };
    let run = eframe::run_native(
        title,
        options,
        Box::new(move |cc| {
            configure_style(&cc.egui_ctx);
            let app = match state.mode.as_str() {
                "thinking" => AltApp::Thinking(ThinkingApp::new(
                    handle,
                    state.thinking.unwrap_or_default(),
                    state.runtime,
                    initial.error,
                    &cc.egui_ctx,
                )),
                "team-inspect" | "team-edit" | "team-new" => AltApp::Team(TeamApp::new(
                    handle,
                    state.draft.unwrap_or_default(),
                    state.catalog,
                    state.diagnostics,
                    initial.error,
                    state.mode == "team-inspect",
                    state.runtime,
                )),
                _ => return Err(format!("unsupported native GUI mode {}", state.mode).into()),
            };
            Ok(Box::new(app))
        }),
    );
    if run.is_ok() {
        0
    } else {
        1
    }
}

fn run_error_window(error: String) -> i32 {
    let options = eframe::NativeOptions {
        viewport: egui::ViewportBuilder::default()
            .with_inner_size([680.0, 260.0])
            .with_min_inner_size([520.0, 220.0]),
        ..Default::default()
    };
    let run = eframe::run_native(
        "ALT — Native GUI Error",
        options,
        Box::new(move |_cc| Ok(Box::new(ErrorApp { error }))),
    );
    if run.is_ok() {
        0
    } else {
        1
    }
}

fn configure_style(ctx: &egui::Context) {
    let mut visuals = egui::Visuals::dark();
    visuals.panel_fill = egui::Color32::from_rgb(18, 20, 23);
    visuals.window_fill = egui::Color32::from_rgb(24, 27, 31);
    visuals.extreme_bg_color = egui::Color32::from_rgb(12, 14, 16);
    visuals.selection.bg_fill = egui::Color32::from_rgb(31, 115, 140);
    visuals.hyperlink_color = egui::Color32::from_rgb(83, 202, 230);
    ctx.set_visuals(visuals);
    let mut style = (*ctx.global_style()).clone();
    style.spacing.item_spacing = egui::vec2(8.0, 7.0);
    style.spacing.button_padding = egui::vec2(12.0, 7.0);
    ctx.set_global_style(style);
}

enum AltApp {
    Team(TeamApp),
    Thinking(ThinkingApp),
}

impl eframe::App for AltApp {
    fn ui(&mut self, ui: &mut egui::Ui, frame: &mut eframe::Frame) {
        match self {
            Self::Team(app) => app.update(ui),
            Self::Thinking(app) => app.update(ui, frame),
        }
    }
}

struct ErrorApp {
    error: String,
}

impl eframe::App for ErrorApp {
    fn ui(&mut self, ui: &mut egui::Ui, _frame: &mut eframe::Frame) {
        let ctx = ui.ctx().clone();
        egui::CentralPanel::default().show_inside(ui, |ui| {
            ui.heading("ALT could not open the native surface");
            ui.add_space(12.0);
            ui.colored_label(egui::Color32::from_rgb(255, 112, 112), &self.error);
            ui.add_space(18.0);
            if ui.button("Close").clicked() {
                ctx.send_viewport_cmd(egui::ViewportCommand::Close);
            }
        });
    }
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
enum Selection {
    Router,
    Member(u64),
}

struct TeamApp {
    handle: u64,
    read_only: bool,
    draft: TeamDraft,
    catalog: Vec<CatalogModel>,
    member_uids: Vec<u64>,
    next_uid: u64,
    selection: Selection,
    edge_in_progress: Option<(NodeId, SocketKind, usize)>,
    selected_edges: HashSet<String>,
    show_all_edges: bool,
    view: egui_graph::View,
    routes: EdgeRoutes,
    layout_frames: u8,
    model_filter: String,
    diagnostics: Vec<Diagnostic>,
    status: String,
    runtime: RuntimeCapabilities,
}

impl TeamApp {
    fn new(
        handle: u64,
        draft: TeamDraft,
        catalog: Vec<CatalogModel>,
        diagnostics: Vec<Diagnostic>,
        init_error: String,
        read_only: bool,
        runtime: RuntimeCapabilities,
    ) -> Self {
        let member_uids: Vec<u64> = (0..draft.members.len())
            .map(|index| 100 + index as u64)
            .collect();
        let next_uid = 100 + member_uids.len() as u64;
        Self {
            handle,
            read_only,
            draft,
            catalog,
            member_uids,
            next_uid,
            selection: Selection::Router,
            edge_in_progress: None,
            selected_edges: HashSet::new(),
            show_all_edges: false,
            view: Default::default(),
            routes: Default::default(),
            layout_frames: 3,
            model_filter: String::new(),
            diagnostics,
            status: init_error,
            runtime,
        }
    }

    fn update(&mut self, root: &mut egui::Ui) {
        let ctx = root.ctx().clone();
        egui::Panel::top("team-toolbar").show_inside(root, |ui| {
            ui.horizontal(|ui| {
                ui.heading(if self.read_only {
                    if self.draft.name.is_empty() {
                        "Team".to_owned()
                    } else {
                        self.draft.name.clone()
                    }
                } else if self.draft.base_revision > 0 {
                    format!("Edit Team · revision {}", self.draft.base_revision)
                } else {
                    "New Team".to_owned()
                });
                ui.separator();
                if self.read_only {
                    ui.monospace(format!("{}@{}", self.draft.id, self.draft.base_revision));
                    ui.separator();
                    ui.weak("READ ONLY");
                    ui.separator();
                    let edge_label = if self.show_all_edges {
                        "Focus selection"
                    } else {
                        "Show all edges"
                    };
                    if ui.button(edge_label).clicked() {
                        self.show_all_edges = !self.show_all_edges;
                    }
                } else {
                    if ui.button("+ Lead").clicked() {
                        self.add_member(true);
                    }
                    if ui.button("+ Member").clicked() {
                        self.add_member(false);
                    }
                }
                runtime_policy_badge(ui, &self.runtime);
                if ui.button("Auto layout").clicked() {
                    self.layout_frames = 3;
                }
                if !self.read_only {
                    ui.separator();
                    if ui.button("Validate").clicked() {
                        self.validate();
                    }
                    let publish = egui::Button::new("Publish revision")
                        .fill(egui::Color32::from_rgb(25, 110, 137));
                    if ui.add(publish).clicked() {
                        self.publish(&ctx);
                    }
                }
            });
            if !self.read_only {
                ui.horizontal(|ui| {
                    ui.label("Name");
                    ui.add(egui::TextEdit::singleline(&mut self.draft.name).desired_width(360.0));
                    if !self.status.is_empty() {
                        ui.separator();
                        ui.label(&self.status);
                    }
                });
            }
        });

        if !self.read_only {
            egui::Panel::bottom("team-diagnostics")
                .resizable(true)
                .default_size(128.0)
                .min_size(58.0)
                .show_inside(root, |ui| {
                    let errors = self
                        .diagnostics
                        .iter()
                        .filter(|item| item.severity == "error")
                        .count();
                    let warnings = self.diagnostics.len().saturating_sub(errors);
                    ui.horizontal(|ui| {
                        ui.strong("Deterministic validation");
                        ui.label(format!("{errors} errors · {warnings} warnings"));
                        ui.separator();
                        ui.weak("Definitions are stored verbatim; warnings do not rewrite them.");
                    });
                    egui::ScrollArea::vertical().show(ui, |ui| {
                        for item in &self.diagnostics {
                            let color = if item.severity == "error" {
                                egui::Color32::from_rgb(255, 108, 108)
                            } else {
                                egui::Color32::from_rgb(236, 190, 74)
                            };
                            ui.horizontal_wrapped(|ui| {
                                ui.colored_label(color, item.severity.to_uppercase());
                                ui.monospace(&item.path);
                                ui.label(&item.message);
                            });
                        }
                    });
                });
        }

        egui::Panel::right("team-inspector")
            .resizable(true)
            .default_size(420.0)
            .min_size(320.0)
            .show_inside(root, |ui| {
                if self.read_only {
                    self.read_only_inspector(ui);
                } else {
                    self.scrollable_editor_inspector(ui);
                }
            });

        egui::CentralPanel::default()
            .frame(egui::Frame::default().fill(egui::Color32::from_rgb(13, 15, 18)))
            .show_inside(root, |ui| self.graph(ui));
    }

    fn add_member(&mut self, lead: bool) {
        let prefix = if lead { "lead" } else { "member" };
        let mut ordinal = self.draft.members.len() + 1;
        let id = loop {
            let candidate = format!("{prefix}-{ordinal}");
            if !self
                .draft
                .members
                .iter()
                .any(|member| member.id == candidate)
            {
                break candidate;
            }
            ordinal += 1;
        };
        let uid = self.next_uid;
        self.next_uid += 1;
        self.member_uids.push(uid);
        self.draft.members.push(DraftMember {
            id,
            lead,
            ..Default::default()
        });
        self.selection = Selection::Member(uid);
        self.layout_frames = 3;
        self.status.clear();
    }

    fn read_only_inspector(&self, ui: &mut egui::Ui) {
        egui::ScrollArea::vertical().show(ui, |ui| {
            match self.selection {
                Selection::Router => {
                    ui.heading("Router");
                    ui.weak("Selects the Lead assignment responsible for each request.");
                    ui.add_space(10.0);
                    read_only_model(ui, &self.draft.router.model);
                    read_only_definition(ui, &self.draft.router.definition);
                    ui.add_space(12.0);
                    ui.strong("Routes to");
                    for member in self.draft.members.iter().filter(|member| member.lead) {
                        ui.monospace(&member.id);
                    }
                }
                Selection::Member(uid) => {
                    let Some(index) = self
                        .member_uids
                        .iter()
                        .position(|candidate| *candidate == uid)
                    else {
                        return;
                    };
                    let member = &self.draft.members[index];
                    ui.heading(&member.id);
                    ui.label(if member.lead { "Lead" } else { "Member" });
                    ui.add_space(10.0);
                    read_only_model(ui, &member.model);
                    read_only_definition(ui, &member.definition);

                    if member.lead {
                        ui.add_space(12.0);
                        ui.strong("Can call");
                        let mut any = false;
                        for edge in self
                            .draft
                            .call_edges
                            .iter()
                            .filter(|edge| edge.lead_id == member.id)
                        {
                            any = true;
                            ui.monospace(&edge.member_id);
                        }
                        if !any {
                            ui.weak("No callable members assigned.");
                        }
                    }
                    ui.add_space(12.0);
                    ui.strong("Callable by");
                    let mut any = false;
                    for edge in self
                        .draft
                        .call_edges
                        .iter()
                        .filter(|edge| edge.member_id == member.id)
                    {
                        any = true;
                        ui.monospace(&edge.lead_id);
                    }
                    if !any {
                        ui.weak("Not callable by a Lead.");
                    }
                }
            }
            ui.add_space(18.0);
            ui.separator();
            runtime_capabilities(ui, &self.runtime);
        });
    }

    fn inspector(&mut self, ui: &mut egui::Ui) {
        match self.selection {
            Selection::Router => {
                ui.heading("Router");
                ui.weak("The Router is mandatory and explicitly model-backed.");
                ui.add_space(8.0);
                model_editor(
                    ui,
                    &self.catalog,
                    &mut self.model_filter,
                    &mut self.draft.router.model,
                );
                definition_editor(ui, &mut self.draft.router.definition);
            }
            Selection::Member(uid) => {
                let Some(index) = self
                    .member_uids
                    .iter()
                    .position(|candidate| *candidate == uid)
                else {
                    self.selection = Selection::Router;
                    return;
                };
                let old_id = self.draft.members[index].id.clone();
                let identity_changed;
                let mut roles_changed = false;
                let delete;
                {
                    let member = &mut self.draft.members[index];
                    ui.heading(if member.id.is_empty() {
                        "Team member"
                    } else {
                        &member.id
                    });
                    ui.label("Identity");
                    identity_changed = ui
                        .add(
                            egui::TextEdit::singleline(&mut member.id).desired_width(f32::INFINITY),
                        )
                        .changed();
                    ui.horizontal(|ui| {
                        ui.label("Authority");
                        roles_changed |= ui.checkbox(&mut member.lead, "Eligible Lead").changed();
                    });
                    ui.weak("Every node is a Team member. Lead eligibility permits Router selection and outgoing calls.");
                    ui.add_space(8.0);
                    model_editor(ui, &self.catalog, &mut self.model_filter, &mut member.model);
                    definition_editor(ui, &mut member.definition);
                    ui.add_space(12.0);
                    delete = ui
                        .add(
                            egui::Button::new("Delete member")
                                .fill(egui::Color32::from_rgb(92, 39, 42)),
                        )
                        .clicked();
                }
                if identity_changed {
                    let new_id = self.draft.members[index].id.clone();
                    for edge in &mut self.draft.call_edges {
                        if edge.lead_id == old_id {
                            edge.lead_id.clone_from(&new_id);
                        }
                        if edge.member_id == old_id {
                            edge.member_id.clone_from(&new_id);
                        }
                    }
                    self.layout_frames = 2;
                }
                if roles_changed {
                    self.prune_invalid_role_edges();
                    self.layout_frames = 3;
                }
                if delete {
                    self.delete_member(uid);
                }
            }
        }
    }

    fn scrollable_editor_inspector(
        &mut self,
        ui: &mut egui::Ui,
    ) -> egui::scroll_area::ScrollAreaOutput<()> {
        egui::ScrollArea::vertical()
            .id_salt("team-editor-inspector-scroll")
            .auto_shrink([false, false])
            .show(ui, |ui| self.inspector(ui))
    }

    fn graph(&mut self, ui: &mut egui::Ui) {
        let center_view = self.layout_frames == 1;
        if self.layout_frames > 0 {
            self.relayout(ui.ctx());
            self.layout_frames -= 1;
        }
        let selected_nodes = HashSet::from([match self.selection {
            Selection::Router => router_node_id(),
            Selection::Member(uid) => NodeId::from_u64(uid),
        }]);
        let sizes = egui_graph::with_graph_memory(ui.ctx(), team_graph_id(), |memory| {
            memory.node_sizes().clone()
        });
        let ports = self.port_layout(&self.view.layout, &sizes);
        let mut view = std::mem::take(&mut self.view);
        let edge_layout = view.layout.clone();
        let mut graph = egui_graph::Graph::from_id(team_graph_id())
            .center_view(center_view)
            .dot_grid(true)
            .wheel_zoom(true)
            .zoom_range(pixel_perfect_scene_zoom_range())
            .selected_nodes(selected_nodes);
        if self.read_only {
            graph = graph
                .drag_pan_buttons(
                    egui::containers::DragPanButtons::PRIMARY
                        | egui::containers::DragPanButtons::MIDDLE,
                )
                .marquee_selection(false)
                .immutable(true)
                .align(false);
        } else {
            graph = graph.align(true);
        }
        let response = graph.show(&mut view, ui, |ui, show| {
            show.nodes(ui, |nctx, ui| self.render_nodes(nctx, ui, &ports))
                .edges(ui, |ectx, ui| {
                    self.render_edges(ectx, ui, &edge_layout, &ports)
                });
        });
        self.view = view;
        if !self.read_only && self.view.layout != edge_layout {
            self.reroute(ui.ctx());
            ui.ctx().request_repaint();
        }
        if let Some(selected) = response.selection_changed {
            if selected.contains(&router_node_id()) {
                self.selection = Selection::Router;
            } else if let Some(id) = selected.iter().next() {
                self.selection = Selection::Member(id.0);
            }
        }
        if !self.read_only {
            response.response.context_menu(|ui| {
                ui.strong("Create Team member");
                if ui.button("Lead").clicked() {
                    self.add_member(true);
                    ui.close();
                }
                if ui.button("Member").clicked() {
                    self.add_member(false);
                    ui.close();
                }
            });
        }
    }

    fn render_nodes(
        &mut self,
        nctx: &mut egui_graph::NodesCtx,
        ui: &mut egui::Ui,
        ports: &PortLayout,
    ) {
        let mut edge_events = Vec::new();
        let router_outputs = ports
            .outputs
            .get(&router_node_id())
            .copied()
            .unwrap_or_default();
        let router_response = egui_graph::node::Node::from_id(router_node_id())
            .outputs(if self.read_only {
                0
            } else {
                router_outputs.max(1)
            })
            .flow(egui::Direction::TopDown)
            .animation_time(if self.read_only { 0.0 } else { 0.1 })
            .show(nctx, ui, |node_ctx| {
                node_ctx.framed(|ui, _| {
                    if self.read_only {
                        ui.visuals_mut().disabled_alpha = 1.0;
                    }
                    ui.strong("ROUTER");
                    model_badge(ui, &self.draft.router.model);
                })
            });
        if let Some(event) = router_response.edge_event() {
            edge_events.push((router_node_id(), event));
        }

        let mut deletions = Vec::new();
        for (index, member) in self.draft.members.iter().enumerate() {
            let uid = self.member_uids[index];
            let node_id = NodeId::from_u64(uid);
            let roles = if member.lead { "LEAD" } else { "MEMBER" };
            let inputs = ports.inputs.get(&node_id).copied().unwrap_or_default();
            let outputs = ports.outputs.get(&node_id).copied().unwrap_or_default();
            let response = egui_graph::node::Node::from_id(node_id)
                .inputs(if self.read_only { 0 } else { inputs.max(1) })
                .outputs(if self.read_only || !member.lead {
                    0
                } else {
                    outputs.max(1)
                })
                .flow(egui::Direction::TopDown)
                .animation_time(if self.read_only { 0.0 } else { 0.1 })
                .show(nctx, ui, |node_ctx| {
                    node_ctx.framed(|ui, _| {
                        if self.read_only {
                            ui.visuals_mut().disabled_alpha = 1.0;
                        }
                        ui.strong(if member.id.is_empty() {
                            "unnamed"
                        } else {
                            &member.id
                        });
                        ui.small(roles);
                        model_badge(ui, &member.model);
                    })
                });
            if let Some(event) = response.edge_event() {
                edge_events.push((node_id, event));
            }
            if response.removed() {
                deletions.push(uid);
            }
        }
        for (node_id, event) in edge_events {
            self.apply_edge_event(node_id, event);
        }
        for uid in deletions {
            self.delete_member(uid);
        }
    }

    fn render_edges(
        &mut self,
        ectx: &mut egui_graph::EdgesCtx,
        ui: &mut egui::Ui,
        layout: &egui_graph::Layout,
        ports: &PortLayout,
    ) {
        if self.read_only {
            self.render_read_only_edges(ui, layout);
            return;
        }
        let edges = self.visible_graph_edges();
        let mut deleted = Vec::new();
        for edge in edges {
            let pair = ports
                .endpoints
                .get(&edge.key)
                .copied()
                .unwrap_or(((edge.from, 0), (edge.to, 0)));
            let waypoints = self.routes.route(pair.0, pair.1, 0).unwrap_or(&[]);
            let mut selected = !self.read_only && self.selected_edges.contains(&edge.key);
            let mut widget =
                egui_graph::edge::Edge::new(pair.0, pair.1, &mut selected).waypoints(waypoints);
            if self.read_only {
                let color = match edge.kind {
                    GraphEdgeKind::Router => egui::Color32::from_rgb(112, 130, 148),
                    GraphEdgeKind::Call if self.show_all_edges => {
                        egui::Color32::from_rgba_unmultiplied(69, 184, 218, 88)
                    }
                    GraphEdgeKind::Call => egui::Color32::from_rgb(69, 184, 218),
                };
                widget = widget.stroke(egui::Stroke::new(1.5, color));
            }
            let response = widget.show(ectx, ui);
            if !self.read_only {
                if response.deleted() {
                    deleted.push(edge);
                } else if response.changed() {
                    if selected {
                        self.selected_edges.insert(edge.key);
                    } else {
                        self.selected_edges.remove(&edge.key);
                    }
                }
            }
        }
        for edge in deleted {
            self.remove_graph_edge(edge);
        }
        if let Some(edge) = ectx.in_progress(ui) {
            edge.show(ui, 0.5);
        }
    }

    fn render_read_only_edges(&self, ui: &mut egui::Ui, layout: &egui_graph::Layout) {
        let sizes = egui_graph::with_graph_memory(ui.ctx(), team_graph_id(), |memory| {
            memory.node_sizes().clone()
        });
        let rect_for = |id: NodeId| {
            let position = layout.get(&id).copied()?;
            let size = sizes.get(&id).copied().unwrap_or_else(|| {
                if id == router_node_id() {
                    egui::vec2(180.0, 72.0)
                } else {
                    egui::vec2(220.0, 92.0)
                }
            });
            Some(egui::Rect::from_min_size(position, size))
        };
        let node_rects: Vec<_> = std::iter::once(router_node_id())
            .chain(self.member_uids.iter().copied().map(NodeId::from_u64))
            .filter_map(|id| Some((id, rect_for(id)?)))
            .collect();

        for edge in self.visible_graph_edges() {
            let (Some(from_rect), Some(to_rect)) = (rect_for(edge.from), rect_for(edge.to)) else {
                continue;
            };
            let points = route_between_rects(edge.from, from_rect, edge.to, to_rect, &node_rects);
            if points.len() < 2 {
                continue;
            }
            let color = match edge.kind {
                GraphEdgeKind::Router => egui::Color32::from_rgb(112, 130, 148),
                GraphEdgeKind::Call if self.show_all_edges => {
                    egui::Color32::from_rgba_unmultiplied(69, 184, 218, 72)
                }
                GraphEdgeKind::Call => egui::Color32::from_rgb(69, 184, 218),
            };
            let stroke = egui::Stroke::new(
                if matches!(edge.kind, GraphEdgeKind::Router) {
                    1.5
                } else {
                    1.75
                },
                color,
            );
            ui.painter().add(egui::Shape::line(points.clone(), stroke));
            if let Some(head) = arrow_head(&points, 8.0) {
                ui.painter().add(egui::Shape::convex_polygon(
                    head.to_vec(),
                    color,
                    egui::Stroke::NONE,
                ));
            }
        }
    }

    fn apply_edge_event(&mut self, node: NodeId, event: EdgeEvent) {
        match event {
            EdgeEvent::Started { kind, index } => {
                self.edge_in_progress = Some((node, kind, index));
            }
            EdgeEvent::Ended { kind, .. } => {
                let Some((started, _, _)) = self.edge_in_progress.take() else {
                    return;
                };
                let (from, to) = match kind {
                    SocketKind::Input => (started, node),
                    SocketKind::Output => (node, started),
                };
                self.create_graph_edge(from, to);
            }
            EdgeEvent::Cancelled => self.edge_in_progress = None,
        }
    }

    fn create_graph_edge(&mut self, from: NodeId, to: NodeId) {
        let Some(target) = self.member_index_for_node(to) else {
            self.status = "Edges must end at a Team member.".to_owned();
            return;
        };
        if from == router_node_id() {
            self.draft.members[target].lead = true;
            self.status = "Lead role added.".to_owned();
            self.layout_frames = 3;
            return;
        }
        let Some(source) = self.member_index_for_node(from) else {
            return;
        };
        if source == target {
            self.status = "A Lead cannot call itself.".to_owned();
            return;
        }
        if !self.draft.members[source].lead {
            self.status = "Only the Router or a Lead can own an outgoing edge.".to_owned();
            return;
        }
        let edge = CallEdge {
            lead_id: self.draft.members[source].id.clone(),
            member_id: self.draft.members[target].id.clone(),
        };
        if !self.draft.call_edges.iter().any(|candidate| {
            candidate.lead_id == edge.lead_id && candidate.member_id == edge.member_id
        }) {
            self.draft.call_edges.push(edge);
        }
        self.status = "Call edge added.".to_owned();
        self.layout_frames = 3;
    }

    fn remove_graph_edge(&mut self, edge: GraphEdge) {
        match edge.kind {
            GraphEdgeKind::Router => {
                if let Some(index) = self.member_index_for_node(edge.to) {
                    self.draft.members[index].lead = false;
                    self.prune_invalid_role_edges();
                }
            }
            GraphEdgeKind::Call => {
                self.draft
                    .call_edges
                    .retain(|item| format!("call:{}:{}", item.lead_id, item.member_id) != edge.key);
            }
        }
        self.selected_edges.remove(&edge.key);
        self.layout_frames = 3;
    }

    fn prune_invalid_role_edges(&mut self) {
        let roles: HashMap<String, bool> = self
            .draft
            .members
            .iter()
            .map(|member| (member.id.clone(), member.lead))
            .collect();
        self.draft.call_edges.retain(|edge| {
            edge.lead_id != edge.member_id
                && roles.get(&edge.lead_id).is_some_and(|lead| *lead)
                && roles.contains_key(&edge.member_id)
        });
    }

    fn delete_member(&mut self, uid: u64) {
        let Some(index) = self
            .member_uids
            .iter()
            .position(|candidate| *candidate == uid)
        else {
            return;
        };
        let id = self.draft.members[index].id.clone();
        self.draft.members.remove(index);
        self.member_uids.remove(index);
        self.draft
            .call_edges
            .retain(|edge| edge.lead_id != id && edge.member_id != id);
        self.selection = Selection::Router;
        self.layout_frames = 3;
    }

    fn member_index_for_node(&self, id: NodeId) -> Option<usize> {
        self.member_uids.iter().position(|uid| *uid == id.0)
    }

    fn graph_edges(&self) -> Vec<GraphEdge> {
        let mut result = Vec::new();
        for (index, member) in self.draft.members.iter().enumerate() {
            if member.lead {
                result.push(GraphEdge {
                    key: format!("router:{}", member.id),
                    from: router_node_id(),
                    to: NodeId::from_u64(self.member_uids[index]),
                    kind: GraphEdgeKind::Router,
                });
            }
        }
        for call in &self.draft.call_edges {
            let Some(source) = self
                .draft
                .members
                .iter()
                .position(|member| member.id == call.lead_id)
            else {
                continue;
            };
            let Some(target) = self
                .draft
                .members
                .iter()
                .position(|member| member.id == call.member_id)
            else {
                continue;
            };
            result.push(GraphEdge {
                key: format!("call:{}:{}", call.lead_id, call.member_id),
                from: NodeId::from_u64(self.member_uids[source]),
                to: NodeId::from_u64(self.member_uids[target]),
                kind: GraphEdgeKind::Call,
            });
        }
        result
    }

    fn visible_graph_edges(&self) -> Vec<GraphEdge> {
        let edges = self.graph_edges();
        if !self.read_only || self.show_all_edges {
            return edges;
        }
        let Selection::Member(uid) = self.selection else {
            return edges
                .into_iter()
                .filter(|edge| matches!(edge.kind, GraphEdgeKind::Router))
                .collect();
        };
        let selected = NodeId::from_u64(uid);
        let selected_is_lead = self
            .member_index_for_node(selected)
            .is_some_and(|index| self.draft.members[index].lead);
        edges
            .into_iter()
            .filter(|edge| {
                matches!(edge.kind, GraphEdgeKind::Router) && edge.to == selected
                    || matches!(edge.kind, GraphEdgeKind::Call)
                        && if selected_is_lead {
                            edge.from == selected
                        } else {
                            edge.to == selected
                        }
            })
            .collect()
    }

    fn relayout(&mut self, ctx: &egui::Context) {
        let sizes = egui_graph::with_graph_memory(ctx, team_graph_id(), |memory| {
            memory.node_sizes().clone()
        });
        let router_size = sizes
            .get(&router_node_id())
            .copied()
            .unwrap_or_else(|| egui::vec2(180.0, 72.0));
        let member_sizes: Vec<egui::Vec2> = self
            .member_uids
            .iter()
            .map(|uid| {
                sizes
                    .get(&NodeId::from_u64(*uid))
                    .copied()
                    .unwrap_or_else(|| egui::vec2(220.0, 92.0))
            })
            .collect();
        let member_index: BTreeMap<_, _> = self
            .draft
            .members
            .iter()
            .enumerate()
            .map(|(index, member)| (member.id.as_str(), index))
            .collect();
        let callable_members: HashSet<_> = self
            .draft
            .call_edges
            .iter()
            .map(|edge| edge.member_id.as_str())
            .collect();
        let team = layout_engine::Team {
            router_key: router_node_id().value(),
            router_size: layout_engine::Size {
                width: router_size.x,
                height: router_size.y,
            },
            members: self
                .draft
                .members
                .iter()
                .enumerate()
                .map(|(index, member)| layout_engine::Member {
                    key: self.member_uids[index],
                    size: layout_engine::Size {
                        width: member_sizes[index].x,
                        height: member_sizes[index].y,
                    },
                    roles: layout_engine::Roles {
                        lead: member.lead,
                        callable: callable_members.contains(member.id.as_str()),
                    },
                })
                .collect(),
            call_edges: self
                .draft
                .call_edges
                .iter()
                .filter_map(|edge| {
                    Some(layout_engine::CallEdge {
                        from: self.member_uids[*member_index.get(edge.lead_id.as_str())?],
                        to: self.member_uids[*member_index.get(edge.member_id.as_str())?],
                    })
                })
                .collect(),
        };
        let layout = layout_engine::layout_team(&team);
        self.view.layout = layout
            .positions
            .into_iter()
            .map(|(key, point)| (NodeId::from_u64(key), egui::pos2(point.x, point.y)))
            .collect();
        if self.read_only {
            self.routes = EdgeRoutes::default();
        } else {
            self.reroute(ctx);
        }
    }

    fn reroute(&mut self, ctx: &egui::Context) {
        let sizes = egui_graph::with_graph_memory(ctx, team_graph_id(), |memory| {
            memory.node_sizes().clone()
        });
        let ports = self.port_layout(&self.view.layout, &sizes);
        let socket_padding = egui_graph::socket_padding(ctx.global_style().as_ref());
        let router_size = sizes
            .get(&router_node_id())
            .copied()
            .unwrap_or_else(|| egui::vec2(180.0, 72.0));
        let router = self
            .view
            .layout
            .get(&router_node_id())
            .copied()
            .map(|position| {
                (
                    router_node_id(),
                    position,
                    LayoutNode::new(router_size)
                        .socket_padding(socket_padding)
                        .outputs(
                            ports
                                .outputs
                                .get(&router_node_id())
                                .copied()
                                .unwrap_or_default()
                                .max(1),
                        ),
                )
            });
        let members = self
            .draft
            .members
            .iter()
            .enumerate()
            .filter_map(|(index, member)| {
                let id = NodeId::from_u64(self.member_uids[index]);
                let position = self.view.layout.get(&id).copied()?;
                let size = sizes
                    .get(&id)
                    .copied()
                    .unwrap_or_else(|| egui::vec2(220.0, 92.0));
                Some((
                    id,
                    position,
                    LayoutNode::new(size)
                        .socket_padding(socket_padding)
                        .inputs(ports.inputs.get(&id).copied().unwrap_or_default().max(1))
                        .outputs(if member.lead {
                            ports.outputs.get(&id).copied().unwrap_or_default().max(1)
                        } else {
                            0
                        }),
                ))
            });
        let edges = self
            .graph_edges()
            .into_iter()
            .filter_map(|edge| ports.endpoints.get(&edge.key).copied());
        self.routes = route_edges(
            router.into_iter().chain(members),
            edges,
            LayoutParams::new(egui::Direction::TopDown).node_gap(32.0),
        );
    }

    fn port_layout(
        &self,
        positions: &HashMap<NodeId, egui::Pos2>,
        sizes: &HashMap<NodeId, egui::Vec2>,
    ) -> PortLayout {
        let mut centers = HashMap::new();
        if let Some(position) = positions.get(&router_node_id()).copied() {
            let size = sizes
                .get(&router_node_id())
                .copied()
                .unwrap_or_else(|| egui::vec2(180.0, 72.0));
            centers.insert(router_node_id(), position + size * 0.5);
        }
        for (index, uid) in self.member_uids.iter().copied().enumerate() {
            let id = NodeId::from_u64(uid);
            let Some(position) = positions.get(&id).copied() else {
                continue;
            };
            let size = sizes
                .get(&id)
                .copied()
                .unwrap_or_else(|| egui::vec2(220.0, 92.0));
            centers.insert(id, position + size * 0.5);
            debug_assert!(index < self.draft.members.len());
        }
        let edges: Vec<_> = self
            .graph_edges()
            .into_iter()
            .map(|edge| PortEdge {
                key: edge.key,
                from: edge.from,
                to: edge.to,
            })
            .collect();
        assign_edge_ports(&edges, &centers)
    }

    fn validate(&mut self) {
        match host_exchange(self.handle, "team.validate", Some(&self.draft)) {
            Ok(response) => {
                self.diagnostics = response.diagnostics;
                self.status = if response.ok {
                    "Structure is publishable.".to_owned()
                } else if response.error.is_empty() {
                    "Resolve the validation errors before publishing.".to_owned()
                } else {
                    response.error
                };
            }
            Err(error) => self.status = error,
        }
    }

    fn publish(&mut self, ctx: &egui::Context) {
        match host_exchange(self.handle, "team.publish", Some(&self.draft)) {
            Ok(response) => {
                self.diagnostics = response.diagnostics;
                if let Some(published) = response.published {
                    self.status = format!(
                        "Published {}@{} ({})",
                        published.id,
                        published.revision,
                        &published.digest[..published.digest.len().min(8)]
                    );
                    ctx.send_viewport_cmd(egui::ViewportCommand::Close);
                } else {
                    self.status = if response.error.is_empty() {
                        "Publish was rejected.".to_owned()
                    } else {
                        response.error
                    };
                }
            }
            Err(error) => self.status = error,
        }
    }
}

#[derive(Clone)]
struct GraphEdge {
    key: String,
    from: NodeId,
    to: NodeId,
    kind: GraphEdgeKind,
}

#[derive(Clone, Copy)]
enum GraphEdgeKind {
    Router,
    Call,
}

fn router_node_id() -> NodeId {
    NodeId::from_u64(1)
}

fn team_graph_id() -> egui::Id {
    egui_graph::id("ALT Team Builder")
}

fn read_only_model(ui: &mut egui::Ui, model: &ModelChoice) {
    ui.strong("Exact model");
    model_badge(ui, model);
    ui.horizontal_wrapped(|ui| {
        ui.weak("Gateway");
        ui.monospace(&model.gateway);
        ui.separator();
        ui.weak("Route");
        ui.monospace(&model.route);
    });
}

fn read_only_definition(ui: &mut egui::Ui, definition: &str) {
    ui.add_space(12.0);
    ui.strong("Assignment definition");
    ui.weak("Stored verbatim. Soft-wrapped for display.");
    ui.add_space(4.0);
    if definition.trim().is_empty() {
        ui.weak("No definition.");
    } else {
        ui.add(
            egui::Label::new(reflow_definition_for_display(definition))
                .selectable(true)
                .wrap(),
        );
    }
}

fn reflow_definition_for_display(definition: &str) -> String {
    definition
        .replace("\r\n", "\n")
        .split("\n\n")
        .map(|paragraph| {
            let lines: Vec<&str> = paragraph.lines().collect();
            if lines.iter().copied().any(is_structured_definition_line) {
                lines
                    .into_iter()
                    .map(str::trim_end)
                    .collect::<Vec<_>>()
                    .join("\n")
            } else {
                lines
                    .into_iter()
                    .flat_map(str::split_whitespace)
                    .collect::<Vec<_>>()
                    .join(" ")
            }
        })
        .collect::<Vec<_>>()
        .join("\n\n")
}

fn is_structured_definition_line(line: &str) -> bool {
    let trimmed = line.trim_start();
    let indented = trimmed.len() != line.len();
    let list = trimmed.starts_with("- ")
        || trimmed.starts_with("* ")
        || trimmed.starts_with("+ ")
        || trimmed.starts_with("• ")
        || trimmed
            .split_once(". ")
            .is_some_and(|(prefix, _)| prefix.chars().all(|ch| ch.is_ascii_digit()));
    indented
        || list
        || trimmed.starts_with('#')
        || trimmed.starts_with('>')
        || trimmed.starts_with("```")
}

fn model_editor(
    ui: &mut egui::Ui,
    catalog: &[CatalogModel],
    filter: &mut String,
    selected: &mut ModelChoice,
) {
    ui.label("Exact model");
    ui.horizontal(|ui| {
        model_badge(ui, selected);
        if !selected.route.is_empty() {
            ui.weak(format!("{} · {}", selected.gateway, selected.route));
        }
    });
    ui.add(
        egui::TextEdit::singleline(filter)
            .hint_text("Filter models")
            .desired_width(f32::INFINITY),
    );
    let needle = filter.trim().to_lowercase();
    egui::ScrollArea::vertical()
        .id_salt("model-catalog")
        .max_height(170.0)
        .show(ui, |ui| {
            for item in catalog {
                if !needle.is_empty()
                    && !item.id.to_lowercase().contains(&needle)
                    && !item.route.to_lowercase().contains(&needle)
                    && !item.gateway.to_lowercase().contains(&needle)
                {
                    continue;
                }
                let active = selected.gateway == item.gateway
                    && selected.id == item.id
                    && selected.route == item.route;
                let label = format!("{}  ·  {} / {}", item.id, item.gateway, item.route);
                if ui.selectable_label(active, label).clicked() {
                    *selected = ModelChoice {
                        gateway: item.gateway.clone(),
                        route: item.route.clone(),
                        id: item.id.clone(),
                    };
                }
            }
        });
}

fn definition_editor(ui: &mut egui::Ui, definition: &mut String) {
    ui.add_space(10.0);
    ui.label("Assignment definition");
    ui.weak("Stored verbatim. The Router and this model receive this exact text.");
    ui.add(
        egui::TextEdit::multiline(definition)
            .hint_text("Write the model's ownership, responsibilities, boundaries, and delegation expectations.")
            .desired_rows(16)
            .desired_width(f32::INFINITY),
    );
}

fn model_badge(ui: &mut egui::Ui, model: &ModelChoice) {
    if model.id.is_empty() {
        ui.colored_label(egui::Color32::from_rgb(255, 125, 125), "model not selected");
    } else {
        ui.monospace(&model.id);
    }
}

struct LiveRegistration {
    handle: u64,
    generation: Arc<AtomicU64>,
}

impl LiveRegistration {
    fn new(handle: u64, context: &egui::Context) -> Self {
        let generation = Arc::new(AtomicU64::new(0));
        LIVE_SIGNALS
            .lock()
            .expect("live signal mutex poisoned")
            .insert(
                handle,
                LiveSignal {
                    context: context.clone(),
                    generation: generation.clone(),
                },
            );
        Self { handle, generation }
    }
}

impl Drop for LiveRegistration {
    fn drop(&mut self) {
        LIVE_SIGNALS
            .lock()
            .expect("live signal mutex poisoned")
            .remove(&self.handle);
    }
}

struct ThinkingApp {
    handle: u64,
    state: ThinkingProjection,
    view: egui_graph::View,
    routes: flow_router::FlowRoutes,
    selected: Option<String>,
    live: LiveRegistration,
    observed_generation: u64,
    structure_fingerprint: u64,
    routing_fingerprint: u64,
    layout_frames: u8,
    route_frames: u8,
    error: String,
    expanded_metadata: HashSet<String>,
    runtime: RuntimeCapabilities,
}

impl ThinkingApp {
    fn new(
        handle: u64,
        state: ThinkingProjection,
        runtime: RuntimeCapabilities,
        error: String,
        context: &egui::Context,
    ) -> Self {
        Self {
            handle,
            state,
            view: Default::default(),
            routes: Default::default(),
            selected: None,
            live: LiveRegistration::new(handle, context),
            observed_generation: u64::MAX,
            structure_fingerprint: 0,
            routing_fingerprint: 0,
            layout_frames: 3,
            route_frames: 0,
            error,
            expanded_metadata: HashSet::new(),
            runtime,
        }
    }

    fn update(&mut self, root: &mut egui::Ui, _frame: &mut eframe::Frame) {
        let ctx = root.ctx().clone();
        let generation = self.live.generation.load(Ordering::Acquire);
        if generation != self.observed_generation {
            self.refresh();
            self.observed_generation = generation;
        }
        if self.state.active.status == "running" {
            // Frames while work is open animate elapsed activity. State itself
            // is refreshed only by alt_native_gui_wake, never by this timer.
            ctx.request_repaint_after(Duration::from_millis(16));
        }
        egui::Panel::top("thinking-toolbar").show_inside(root, |ui| {
            ui.horizontal_wrapped(|ui| {
                ui.heading("Thinking");
                ui.separator();
                ui.monospace(format!(
                    "turn {}/{} · sequence {}",
                    self.state.active.ordinal,
                    self.state.turns.len(),
                    self.state.active.sequence
                ));
                ui.separator();
                ui.weak(short_id(&self.state.session_id))
                    .on_hover_text(format!(
                        "session {}\nactive turn {}\nprojection revision {}\n\n{}",
                        self.state.session_id,
                        self.state.active.id,
                        self.state.revision,
                        self.state.active.task
                    ));
                if ui.button("Auto layout").clicked() {
                    self.layout_frames = 3;
                }
                runtime_policy_badge(ui, &self.runtime);
                if !self.error.is_empty() {
                    ui.separator();
                    ui.colored_label(egui::Color32::from_rgb(255, 112, 112), &self.error);
                }
            });
            if self.state.turns.len() > 1 {
                ui.horizontal_wrapped(|ui| {
                    for turn in &self.state.turns {
                        let current = turn.id == self.state.active_turn_id;
                        let label =
                            format!("{} · {} · seq {}", turn.ordinal, turn.status, turn.sequence);
                        if current {
                            ui.strong(label).on_hover_text(&turn.task);
                        } else {
                            ui.weak(label).on_hover_text(&turn.task);
                        }
                    }
                });
            }
        });
        egui::Panel::right("thinking-inspector")
            .resizable(true)
            .default_size(340.0)
            .min_size(280.0)
            .show_inside(root, |ui| {
                egui::ScrollArea::vertical()
                    .id_salt("thinking-inspector-scroll")
                    .show(ui, |ui| self.thinking_inspector(ui));
            });
        egui::CentralPanel::default()
            .frame(egui::Frame::default().fill(egui::Color32::from_rgb(13, 15, 18)))
            .show_inside(root, |ui| self.thinking_graph(ui));
    }

    fn refresh(&mut self) {
        match host_exchange(self.handle, "thinking.snapshot", None) {
            Ok(response) => {
                if let Some(state) = response.thinking {
                    let structure_fingerprint = thinking_fingerprint(&state);
                    let routing_fingerprint = thinking_routing_fingerprint(&state);
                    if structure_fingerprint != self.structure_fingerprint {
                        self.layout_frames = 3;
                        self.structure_fingerprint = structure_fingerprint;
                        self.route_frames = 0;
                    } else if routing_fingerprint != self.routing_fingerprint {
                        // Execution traffic changes independently of the stable
                        // Team architecture. Re-route it without moving or
                        // re-centering nodes the user may be inspecting.
                        self.route_frames = 1;
                    }
                    self.routing_fingerprint = routing_fingerprint;
                    self.state = state;
                    self.error = response.error;
                } else if !response.error.is_empty() {
                    self.error = response.error;
                }
            }
            Err(error) => self.error = error,
        }
    }

    fn thinking_graph(&mut self, ui: &mut egui::Ui) {
        let center_view = self.layout_frames == 1;
        if self.layout_frames > 0 {
            self.relayout(ui.ctx());
            self.layout_frames -= 1;
        } else if self.route_frames > 0 {
            self.reroute(ui.ctx());
            self.route_frames -= 1;
        }
        let selected_nodes: HashSet<NodeId> = self
            .selected
            .as_ref()
            .map(|id| HashSet::from([thinking_node_id(id)]))
            .unwrap_or_default();
        let mut view = std::mem::take(&mut self.view);
        let response = egui_graph::Graph::from_id(thinking_graph_id())
            .center_view(center_view)
            .dot_grid(true)
            .wheel_zoom(true)
            .drag_pan_buttons(
                egui::containers::DragPanButtons::PRIMARY
                    | egui::containers::DragPanButtons::MIDDLE,
            )
            .marquee_selection(false)
            .zoom_range(pixel_perfect_scene_zoom_range())
            .immutable(true)
            .selected_nodes(selected_nodes)
            .show(&mut view, ui, |ui, show| {
                show.nodes(ui, |nctx, ui| {
                    for node in self.state.active.nodes.values() {
                        let id = thinking_node_id(&node.id);
                        egui_graph::node::Node::from_id(id)
                            // The live graph uses geometry-derived free ports.
                            // Widget sockets would reintroduce a false global
                            // TopDown axis and are therefore intentionally absent.
                            .inputs(0)
                            .outputs(0)
                            .flow(egui::Direction::TopDown)
                            .show(nctx, ui, |node_ctx| {
                                node_ctx.framed(|ui, _| {
                                    ui.visuals_mut().disabled_alpha = 1.0;
                                    ui.horizontal(|ui| {
                                        ui.colored_label(status_color(&node.status), "●");
                                        ui.strong(&node.label);
                                    });
                                    ui.small(format!("{} · {}", node.kind, node.status));
                                })
                            });
                    }
                })
                .edges(ui, |_ectx, ui| {
                    for edge in self
                        .state
                        .active
                        .edges
                        .values()
                        .filter(|edge| edge.kind != "allowed")
                    {
                        let Some(points) = self.routes.path(&edge.id) else {
                            continue;
                        };
                        let color = if edge.status == "failed" {
                            egui::Color32::from_rgb(255, 103, 112)
                        } else if edge.direction == "inward" {
                            egui::Color32::from_rgb(82, 211, 158)
                        } else {
                            thinking_edge_color(&edge.kind)
                        };
                        let width = if edge.active > 0 {
                            3.0
                        } else if edge.kind == "allowed" {
                            0.75
                        } else {
                            1.7
                        };
                        let stroke = egui::Stroke::new(width, color);
                        ui.painter().add(egui::Shape::line(points.to_vec(), stroke));
                        if let Some(head) = arrow_head(points, 8.0) {
                            ui.painter().add(egui::Shape::convex_polygon(
                                head.to_vec(),
                                color,
                                egui::Stroke::NONE,
                            ));
                        }
                        if edge.active > 0 {
                            let offset = (thinking_node_id(&edge.id).value() % 997) as f64 / 997.0;
                            let phase =
                                (ui.input(|input| input.time) * 0.42 + offset).fract() as f32;
                            if let Some(point) = point_along_polyline(points, phase) {
                                ui.painter().circle_filled(point, 4.2, color);
                            }
                            if edge.started_at_ms > 0 {
                                let now_ms = SystemTime::now()
                                    .duration_since(UNIX_EPOCH)
                                    .unwrap_or_default()
                                    .as_millis()
                                    as i64;
                                let elapsed =
                                    (now_ms.saturating_sub(edge.started_at_ms)) as f64 / 1000.0;
                                if let Some(annotation) = tangent_annotation(points, 0.68, 10.0) {
                                    let galley = ui.painter().layout_no_wrap(
                                        format!("{elapsed:.1}s"),
                                        egui::FontId::monospace(9.0),
                                        color,
                                    );
                                    ui.painter().add(anchored_rotated_text(
                                        annotation.anchor,
                                        galley,
                                        color,
                                        annotation.angle,
                                        egui::Align2::CENTER_CENTER,
                                    ));
                                }
                            }
                        }
                        if edge.count > 1 {
                            if let Some(point) = point_along_polyline(points, 0.5) {
                                ui.painter().text(
                                    point,
                                    egui::Align2::CENTER_CENTER,
                                    format!("×{}", edge.count),
                                    egui::FontId::monospace(10.0),
                                    color,
                                );
                            }
                        }
                    }
                });
            });
        self.view = view;
        if let Some(selected) = response.selection_changed {
            self.selected = self
                .state
                .active
                .nodes
                .keys()
                .find(|id| selected.contains(&thinking_node_id(id)))
                .cloned();
        }
    }

    fn thinking_inspector(&mut self, ui: &mut egui::Ui) {
        let Some(id) = &self.selected else {
            ui.heading("Execution provenance");
            ui.label("Select a node to inspect the durable event projection.");
            ui.add_space(8.0);
            ui.weak("This surface displays recorded routing, delegation, tool, and completion events. It does not fabricate hidden model thought.");
            ui.add_space(18.0);
            ui.separator();
            runtime_capabilities(ui, &self.runtime);
            return;
        };
        let Some(node) = self.state.active.nodes.get(id).cloned() else {
            return;
        };
        ui.heading(&node.label);
        ui.monospace(&node.id);
        ui.horizontal(|ui| {
            ui.colored_label(status_color(&node.status), "●");
            ui.label(format!("{} · {}", node.kind, node.status));
        });
        if !node.actor.is_empty() {
            ui.label(format!("Actor: {}", node.actor));
        }
        ui.separator();
        let field_count = node.metadata.len();
        for (index, (key, value)) in node.metadata.iter().enumerate() {
            ui.strong(key);
            let expansion_key = format!("{}\0{}", node.id, key);
            let expanded = self.expanded_metadata.contains(&expansion_key);
            let wrap_width = ui.available_width().max(1.0);
            let font = egui::TextStyle::Body.resolve(ui.style());
            let color = ui.visuals().text_color();
            let galley = ui
                .painter()
                .layout(value.clone(), font.clone(), color, wrap_width);
            let row_height = ui.text_style_height(&egui::TextStyle::Body);
            let fields_remaining = (field_count - index).max(1);
            let fair_height =
                metadata_height_budget(ui.available_height(), fields_remaining, row_height);
            if expanded || galley.size().y <= fair_height {
                ui.add(egui::Label::new(value).selectable(true).wrap());
                if expanded && ui.small_button("Collapse").clicked() {
                    self.expanded_metadata.remove(&expansion_key);
                }
            } else {
                let (rect, response) = ui
                    .allocate_exact_size(egui::vec2(wrap_width, fair_height), egui::Sense::hover());
                ui.painter()
                    .with_clip_rect(rect)
                    .galley(rect.min, galley, color);
                response.on_hover_text("The complete value is retained.");
                if ui.small_button("Show complete value").clicked() {
                    self.expanded_metadata.insert(expansion_key);
                }
            }
            ui.add_space(6.0);
        }
    }

    fn relayout(&mut self, ctx: &egui::Context) {
        let sizes = egui_graph::with_graph_memory(ctx, thinking_graph_id(), |memory| {
            memory.node_sizes().clone()
        });
        let size_for = |id: &str, fallback: egui::Vec2| {
            sizes
                .get(&thinking_node_id(id))
                .copied()
                .unwrap_or(fallback)
        };
        let router_size = size_for("router", egui::vec2(180.0, 72.0));
        let members: Vec<_> = self
            .state
            .active
            .nodes
            .values()
            .filter(|node| node.kind == "member")
            .collect();
        let member_keys: BTreeMap<_, _> = members
            .iter()
            .map(|node| (node.id.as_str(), thinking_node_id(&node.id).value()))
            .collect();
        let callable_members: HashSet<_> = self
            .state
            .active
            .edges
            .values()
            .filter(|edge| {
                edge.kind == "allowed"
                    && edge.from.starts_with("member:")
                    && edge.to.starts_with("member:")
            })
            .map(|edge| edge.to.as_str())
            .collect();
        let team = layout_engine::Team {
            router_key: thinking_node_id("router").value(),
            router_size: layout_engine::Size {
                width: router_size.x,
                height: router_size.y,
            },
            members: members
                .iter()
                .map(|node| {
                    let size = size_for(&node.id, egui::vec2(210.0, 82.0));
                    layout_engine::Member {
                        key: member_keys[node.id.as_str()],
                        size: layout_engine::Size {
                            width: size.x,
                            height: size.y,
                        },
                        roles: layout_engine::Roles {
                            lead: node
                                .metadata
                                .get("lead")
                                .is_some_and(|value| value == "true"),
                            callable: callable_members.contains(node.id.as_str()),
                        },
                    }
                })
                .collect(),
            call_edges: self
                .state
                .active
                .edges
                .values()
                .filter(|edge| {
                    edge.kind == "allowed"
                        && edge.from.starts_with("member:")
                        && edge.to.starts_with("member:")
                })
                .filter_map(|edge| {
                    Some(layout_engine::CallEdge {
                        from: *member_keys.get(edge.from.as_str())?,
                        to: *member_keys.get(edge.to.as_str())?,
                    })
                })
                .collect(),
        };
        let team_layout = layout_engine::layout_team(&team);
        let user_size = size_for("user", egui::vec2(180.0, 72.0));
        let user_position = layout_engine::place_boundary_terminal(
            &team,
            &team_layout,
            layout_engine::Size {
                width: user_size.x,
                height: user_size.y,
            },
        );
        let mut positions: HashMap<NodeId, egui::Pos2> = team_layout
            .positions
            .into_iter()
            .map(|(key, point)| (NodeId::from_u64(key), egui::pos2(point.x, point.y)))
            .collect();
        positions.insert(
            thinking_node_id("user"),
            egui::pos2(user_position.x, user_position.y),
        );

        let tools: Vec<_> = self
            .state
            .active
            .nodes
            .values()
            .filter(|node| node.kind == "tool")
            .collect();
        if !tools.is_empty() {
            let bottom = positions
                .iter()
                .map(|(id, position)| {
                    position.y
                        + sizes
                            .get(id)
                            .copied()
                            .unwrap_or_else(|| egui::vec2(190.0, 76.0))
                            .y
                })
                .fold(0.0_f32, f32::max)
                + 120.0;
            let widths: Vec<_> = tools
                .iter()
                .map(|node| size_for(&node.id, egui::vec2(190.0, 76.0)).x)
                .collect();
            let total = widths.iter().sum::<f32>() + 48.0 * tools.len().saturating_sub(1) as f32;
            let mut x = -total * 0.5;
            for (node, width) in tools.iter().zip(widths) {
                positions.insert(thinking_node_id(&node.id), egui::pos2(x, bottom));
                x += width + 48.0;
            }
        }
        self.view.layout = positions;
        self.reroute(ctx);
    }

    fn reroute(&mut self, ctx: &egui::Context) {
        let sizes = egui_graph::with_graph_memory(ctx, thinking_graph_id(), |memory| {
            memory.node_sizes().clone()
        });
        self.routes = thinking_routes(&self.state, &self.view.layout, &sizes);
    }
}

fn metadata_height_budget(available_height: f32, fields_remaining: usize, row_height: f32) -> f32 {
    (available_height / fields_remaining.max(1) as f32).max(row_height)
}

fn runtime_policy_badge(ui: &mut egui::Ui, runtime: &RuntimeCapabilities) {
    ui.separator();
    if runtime.dangerously_bypass_approvals_and_sandbox {
        ui.colored_label(
            egui::Color32::from_rgb(255, 105, 105),
            "DANGEROUS · HOST ACCESS",
        )
        .on_hover_text(
            "Terminal commands bypass ALT's approval and sandbox boundary. Credential redaction and process ownership remain active.",
        );
    } else {
        ui.colored_label(
            egui::Color32::from_rgb(82, 211, 158),
            "SANDBOXED · NETWORK ISOLATED",
        )
        .on_hover_text(
            "Bubblewrap namespaces, no_new_privs, and Landlock confine terminal commands. Web research uses the separate Exa connection.",
        );
    }
}

fn runtime_capabilities(ui: &mut egui::Ui, runtime: &RuntimeCapabilities) {
    ui.heading("Runtime capabilities");
    ui.weak("Product-owned authority; Team definitions do not rewrite it.");
    ui.add_space(8.0);
    ui.horizontal_wrapped(|ui| {
        ui.strong("Terminal");
        if runtime.filesystem_confinement {
            ui.label("workspace writes");
        } else {
            ui.colored_label(egui::Color32::from_rgb(255, 105, 105), "host access");
        }
        ui.separator();
        ui.strong("Direct network");
        ui.label(if runtime.direct_terminal_network {
            "enabled"
        } else {
            "isolated"
        });
    });
    ui.horizontal_wrapped(|ui| {
        ui.strong("Exa web research");
        if runtime.exa_configured {
            ui.colored_label(egui::Color32::from_rgb(82, 211, 158), "configured");
        } else {
            ui.colored_label(egui::Color32::from_rgb(236, 190, 74), "credential required")
                .on_hover_text("Run `alt auth set exa`.");
        }
    });
    ui.add_space(8.0);
    ui.strong("Model-callable tools");
    ui.horizontal_wrapped(|ui| {
        for tool in &runtime.tools {
            ui.monospace(tool);
        }
    });
}

fn thinking_graph_id() -> egui::Id {
    egui_graph::id("ALT Thinking Graph")
}

/// egui's Scene scales already-rasterized text when zooming above 1:1, which
/// visibly softens glyphs. Keep both graph surfaces at or below native scale:
/// users can still zoom out arbitrarily far and return to a crisp 1:1 view.
fn pixel_perfect_scene_zoom_range() -> egui::Rangef {
    egui::Rangef::new(0.08, 1.0)
}

fn thinking_node_id(value: &str) -> NodeId {
    let mut hasher = std::collections::hash_map::DefaultHasher::new();
    value.hash(&mut hasher);
    NodeId::from_u64(hasher.finish())
}

#[derive(Clone, Copy, Debug)]
struct PolylineSample {
    point: egui::Pos2,
    tangent: egui::Vec2,
}

fn sample_polyline(points: &[egui::Pos2], phase: f32) -> Option<PolylineSample> {
    let total: f32 = points
        .windows(2)
        .map(|pair| pair[0].distance(pair[1]))
        .sum();
    if total <= f32::EPSILON {
        return None;
    }
    let mut remaining = phase.clamp(0.0, 1.0) * total;
    let mut last_nonzero = None;
    for pair in points.windows(2) {
        let delta = pair[1] - pair[0];
        let length = delta.length();
        if length <= f32::EPSILON {
            continue;
        }
        let tangent = delta / length;
        last_nonzero = Some(PolylineSample {
            point: pair[1],
            tangent,
        });
        if remaining <= length {
            return Some(PolylineSample {
                point: pair[0].lerp(pair[1], remaining / length),
                tangent,
            });
        }
        remaining -= length;
    }
    last_nonzero
}

fn point_along_polyline(points: &[egui::Pos2], phase: f32) -> Option<egui::Pos2> {
    sample_polyline(points, phase)
        .map(|sample| sample.point)
        .or_else(|| points.first().copied())
}

#[derive(Clone, Copy, Debug)]
struct TangentAnnotation {
    anchor: egui::Pos2,
    angle: f32,
}

fn tangent_annotation(
    points: &[egui::Pos2],
    phase: f32,
    normal_offset: f32,
) -> Option<TangentAnnotation> {
    let sample = sample_polyline(points, phase)?;
    let mut angle = sample.tangent.y.atan2(sample.tangent.x);
    if angle > std::f32::consts::FRAC_PI_2 {
        angle -= std::f32::consts::PI;
    } else if angle < -std::f32::consts::FRAC_PI_2 {
        angle += std::f32::consts::PI;
    }

    // Choose one deterministic side of the curve: visually above it in
    // screen coordinates, falling back to the right for a vertical tangent.
    let mut normal = egui::vec2(-sample.tangent.y, sample.tangent.x);
    if normal.y > 0.0 || (normal.y.abs() <= f32::EPSILON && normal.x < 0.0) {
        normal = -normal;
    }
    Some(TangentAnnotation {
        anchor: sample.point + normal * normal_offset,
        angle,
    })
}

fn anchored_rotated_text(
    anchor: egui::Pos2,
    galley: std::sync::Arc<egui::Galley>,
    color: egui::Color32,
    angle: f32,
    alignment: egui::Align2,
) -> egui::epaint::TextShape {
    let local_anchor = alignment.pos_in_rect(&galley.rect);
    egui::epaint::TextShape::new(anchor - local_anchor.to_vec2(), galley, color)
        .with_angle_and_anchor(angle, alignment)
}

fn thinking_fingerprint(state: &ThinkingProjection) -> u64 {
    let mut hasher = std::collections::hash_map::DefaultHasher::new();
    state.active_turn_id.hash(&mut hasher);
    for id in state.active.nodes.keys() {
        id.hash(&mut hasher);
    }
    for edge in state
        .active
        .edges
        .values()
        .filter(|edge| edge.kind == "allowed" || edge.kind == "tool")
    {
        edge.id.hash(&mut hasher);
        edge.from.hash(&mut hasher);
        edge.to.hash(&mut hasher);
        edge.kind.hash(&mut hasher);
    }
    hasher.finish()
}

fn thinking_routing_fingerprint(state: &ThinkingProjection) -> u64 {
    let mut hasher = std::collections::hash_map::DefaultHasher::new();
    state.active_turn_id.hash(&mut hasher);
    for edge in state
        .active
        .edges
        .values()
        .filter(|edge| edge.kind != "allowed")
    {
        edge.id.hash(&mut hasher);
        edge.from.hash(&mut hasher);
        edge.to.hash(&mut hasher);
        edge.kind.hash(&mut hasher);
    }
    hasher.finish()
}

fn thinking_node_fallback_size(node: &ThinkingNode) -> egui::Vec2 {
    match node.kind.as_str() {
        "member" => egui::vec2(210.0, 82.0),
        "tool" => egui::vec2(190.0, 76.0),
        _ => egui::vec2(180.0, 72.0),
    }
}

fn thinking_routes(
    state: &ThinkingProjection,
    positions: &HashMap<NodeId, egui::Pos2>,
    measured_sizes: &HashMap<NodeId, egui::Vec2>,
) -> flow_router::FlowRoutes {
    let nodes: Vec<_> = state
        .active
        .nodes
        .values()
        .filter_map(|node| {
            let id = thinking_node_id(&node.id);
            let position = positions.get(&id).copied()?;
            let size = measured_sizes
                .get(&id)
                .copied()
                .unwrap_or_else(|| thinking_node_fallback_size(node));
            Some(flow_router::FlowNode {
                id,
                rect: egui::Rect::from_min_size(position, size),
            })
        })
        .collect();
    let edges: Vec<_> = state
        .active
        .edges
        .values()
        .filter(|edge| edge.kind != "allowed")
        .map(|edge| flow_router::FlowEdge {
            key: edge.id.clone(),
            from: thinking_node_id(&edge.from),
            to: thinking_node_id(&edge.to),
        })
        .collect();
    flow_router::route_flow_edges(&nodes, &edges, 16.0)
}

fn status_color(status: &str) -> egui::Color32 {
    match status {
        "running" => egui::Color32::from_rgb(80, 202, 231),
        "completed" => egui::Color32::from_rgb(80, 210, 144),
        "failed" => egui::Color32::from_rgb(255, 103, 112),
        "cancelled" => egui::Color32::from_rgb(158, 164, 174),
        _ => egui::Color32::from_rgb(235, 188, 72),
    }
}

fn thinking_edge_color(kind: &str) -> egui::Color32 {
    match kind {
        "result" | "failure" | "tool-result" | "answer" => egui::Color32::from_rgb(82, 211, 158),
        "enables" => egui::Color32::from_rgb(239, 190, 75),
        "delegation" | "route" | "request" => egui::Color32::from_rgb(76, 185, 224),
        "tool" => egui::Color32::from_rgb(152, 162, 175),
        "allowed" => egui::Color32::from_rgb(54, 61, 69),
        _ => egui::Color32::from_rgb(116, 137, 159),
    }
}

/// Route a read-only edge between the nearest points on two node rectangles.
///
/// The first pass is the straight, shortest visible segment. If it intersects
/// another node, the segment is deterministically replaced by the shortest
/// valid walk around one side of that node's expanded rectangle. Repeating the
/// operation handles multiple blockers without relying on vertical sockets,
/// which would impose a false flow axis on radial layouts.
fn route_between_rects(
    from_id: NodeId,
    from: egui::Rect,
    to_id: NodeId,
    to: egui::Rect,
    nodes: &[(NodeId, egui::Rect)],
) -> Vec<egui::Pos2> {
    let start = rect_ray_anchor(from, to.center());
    let end = rect_ray_anchor(to, from.center());
    let mut points = vec![start, end];

    for _ in 0..8 {
        let obstruction = points.windows(2).enumerate().find_map(|(segment, pair)| {
            nodes
                .iter()
                .filter(|(id, _)| *id != from_id && *id != to_id)
                .filter_map(|(id, rect)| {
                    let expanded = rect.expand(16.0);
                    segment_rect_entry(pair[0], pair[1], expanded)
                        .map(|entry| (entry, *id, expanded))
                })
                .min_by(|a, b| a.0.total_cmp(&b.0).then_with(|| a.1.cmp(&b.1)))
                .map(|(_, _, rect)| (segment, rect))
        });
        let Some((segment, blocker)) = obstruction else {
            break;
        };
        let a = points[segment];
        let b = points[segment + 1];
        let lt = blocker.left_top();
        let rt = blocker.right_top();
        let rb = blocker.right_bottom();
        let lb = blocker.left_bottom();
        let sides = [(lt, rt), (rt, rb), (rb, lb), (lb, lt)];
        let interior = blocker.shrink(0.5);
        let mut candidates = Vec::new();
        for (left, right) in sides {
            for (first, second) in [(left, right), (right, left)] {
                if segment_rect_entry(a, first, interior).is_none()
                    && segment_rect_entry(first, second, interior).is_none()
                    && segment_rect_entry(second, b, interior).is_none()
                {
                    candidates.push((
                        a.distance(first) + first.distance(second) + second.distance(b),
                        first,
                        second,
                    ));
                }
            }
        }
        let Some((_, first, second)) = candidates.into_iter().min_by(|a, b| a.0.total_cmp(&b.0))
        else {
            break;
        };
        points.splice(segment + 1..segment + 1, [first, second]);
        points.dedup();
    }
    points
}

fn rect_ray_anchor(rect: egui::Rect, toward: egui::Pos2) -> egui::Pos2 {
    let center = rect.center();
    let direction = toward - center;
    if direction.length_sq() <= f32::EPSILON {
        return center;
    }
    let half = rect.size() * 0.5;
    let x_scale = if direction.x.abs() <= f32::EPSILON {
        f32::INFINITY
    } else {
        half.x / direction.x.abs()
    };
    let y_scale = if direction.y.abs() <= f32::EPSILON {
        f32::INFINITY
    } else {
        half.y / direction.y.abs()
    };
    center + direction * x_scale.min(y_scale)
}

fn segment_rect_entry(p: egui::Pos2, q: egui::Pos2, rect: egui::Rect) -> Option<f32> {
    let direction = q - p;
    let mut entry = 0.0_f32;
    let mut exit = 1.0_f32;
    for (origin, delta, min, max) in [
        (p.x, direction.x, rect.min.x, rect.max.x),
        (p.y, direction.y, rect.min.y, rect.max.y),
    ] {
        if delta.abs() <= f32::EPSILON {
            if origin < min || origin > max {
                return None;
            }
        } else {
            let a = (min - origin) / delta;
            let b = (max - origin) / delta;
            entry = entry.max(a.min(b));
            exit = exit.min(a.max(b));
            if entry > exit {
                return None;
            }
        }
    }
    (entry < 1.0 - 1e-4 && exit > 1e-4).then_some(entry)
}

/// Construct a directional triangular arrow head from the final non-degenerate
/// polyline segment. The tip is the path endpoint, so direction is encoded by
/// geometry rather than inferred from layout.
fn arrow_head(points: &[egui::Pos2], size: f32) -> Option<[egui::Pos2; 3]> {
    if points.len() < 2 || !size.is_finite() || size <= 0.0 {
        return None;
    }
    let tip = *points.last()?;
    let previous = points[..points.len() - 1]
        .iter()
        .rev()
        .copied()
        .find(|point| (*point - tip).length_sq() > f32::EPSILON)?;
    let direction = (tip - previous).normalized();
    let normal = egui::vec2(-direction.y, direction.x);
    let base = tip - direction * size;
    Some([
        tip,
        base + normal * size * 0.55,
        base - normal * size * 0.55,
    ])
}

fn short_id(value: &str) -> &str {
    &value[..value.len().min(12)]
}

#[cfg(test)]
mod tests {
    use super::{
        alt_native_gui_wake, arrow_head, assign_edge_ports, metadata_height_budget,
        pixel_perfect_scene_zoom_range, point_along_polyline, rect_ray_anchor,
        reflow_definition_for_display, route_between_rects, sample_polyline, segment_rect_entry,
        tangent_annotation, thinking_fingerprint, thinking_node_id, thinking_routes,
        thinking_routing_fingerprint, CallEdge, DraftAssignment, DraftMember, LiveRegistration,
        ModelChoice, PortEdge, RuntimeCapabilities, Selection, TeamApp, TeamDraft, ThinkingEdge,
        ThinkingNode, ThinkingProjection, ThinkingTurn,
    };
    use egui_graph::NodeId;
    use std::collections::{BTreeMap, HashMap};

    fn collect_shape_text(shape: &eframe::egui::Shape, output: &mut String) {
        match shape {
            eframe::egui::Shape::Text(text) => {
                output.push_str(&text.galley.job.text);
                output.push('\n');
            }
            eframe::egui::Shape::Vec(shapes) => {
                for shape in shapes {
                    collect_shape_text(shape, output);
                }
            }
            _ => {}
        }
    }

    #[test]
    fn definition_display_reflows_soft_prose_but_preserves_structure() {
        let prose = "Preserve\ntechnical accuracy while adapting to the intended\nreader.";
        assert_eq!(
            reflow_definition_for_display(prose),
            "Preserve technical accuracy while adapting to the intended reader."
        );

        let structured = "Requirements:\n\n- first item\n- second item\n\n```\nmake test\n```";
        assert_eq!(reflow_definition_for_display(structured), structured);
    }

    #[test]
    fn radial_edge_anchor_is_the_exact_ray_rectangle_intersection() {
        let rect = eframe::egui::Rect::from_min_size(
            eframe::egui::pos2(-100.0, -50.0),
            eframe::egui::vec2(200.0, 100.0),
        );
        assert_eq!(
            rect_ray_anchor(rect, eframe::egui::pos2(400.0, 0.0)),
            eframe::egui::pos2(100.0, 0.0)
        );
        assert_eq!(
            rect_ray_anchor(rect, eframe::egui::pos2(0.0, -400.0)),
            eframe::egui::pos2(0.0, -50.0)
        );
        let diagonal = rect_ray_anchor(rect, eframe::egui::pos2(400.0, 400.0));
        assert_eq!(diagonal, eframe::egui::pos2(50.0, 50.0));
    }

    #[test]
    fn read_only_route_deterministically_avoids_a_blocking_node() {
        let from_id = egui_graph::NodeId::from_u64(10);
        let blocker_id = egui_graph::NodeId::from_u64(20);
        let to_id = egui_graph::NodeId::from_u64(30);
        let from = eframe::egui::Rect::from_min_size(
            eframe::egui::pos2(-300.0, -40.0),
            eframe::egui::vec2(100.0, 80.0),
        );
        let blocker = eframe::egui::Rect::from_min_size(
            eframe::egui::pos2(-50.0, -50.0),
            eframe::egui::vec2(100.0, 100.0),
        );
        let to = eframe::egui::Rect::from_min_size(
            eframe::egui::pos2(200.0, -40.0),
            eframe::egui::vec2(100.0, 80.0),
        );
        let nodes = [(from_id, from), (blocker_id, blocker), (to_id, to)];
        let route = route_between_rects(from_id, from, to_id, to, &nodes);
        let again = route_between_rects(from_id, from, to_id, to, &nodes);
        assert_eq!(route, again);
        assert!(
            route.len() >= 4,
            "a blocker requires two boundary waypoints"
        );
        let forbidden = blocker.expand(15.0);
        for segment in route.windows(2) {
            assert!(
                segment_rect_entry(segment[0], segment[1], forbidden).is_none(),
                "routed segment enters the blocker's clearance rectangle: {segment:?}"
            );
        }
    }

    #[test]
    fn read_only_team_surface_renders_content_without_authoring_controls() {
        super::TEST_HOST_EXCHANGES.store(0, std::sync::atomic::Ordering::SeqCst);
        let model = |id: &str| ModelChoice {
            gateway: "opencode".to_owned(),
            route: "zen".to_owned(),
            id: id.to_owned(),
        };
        let draft = TeamDraft {
            id: "inspection-team".to_owned(),
            name: "Inspection Team".to_owned(),
            base_revision: 4,
            router: DraftAssignment {
                model: model("inspect-router"),
                definition: "Route using the assignment definitions.".to_owned(),
            },
            members: vec![
                DraftMember {
                    id: "engineering".to_owned(),
                    model: model("inspect-engineer"),
                    definition: "Own implementation.".to_owned(),
                    lead: true,
                },
                DraftMember {
                    id: "research".to_owned(),
                    model: model("inspect-researcher"),
                    definition: "Establish external evidence.".to_owned(),
                    lead: true,
                },
            ],
            call_edges: vec![
                CallEdge {
                    lead_id: "engineering".to_owned(),
                    member_id: "research".to_owned(),
                },
                CallEdge {
                    lead_id: "research".to_owned(),
                    member_id: "engineering".to_owned(),
                },
            ],
        };
        let mut app = TeamApp::new(
            0,
            draft,
            vec![],
            vec![],
            String::new(),
            true,
            RuntimeCapabilities::default(),
        );
        assert_eq!(
            app.visible_graph_edges().len(),
            2,
            "overview should project only Router edges"
        );
        app.selection = Selection::Member(100);
        let focused = app.visible_graph_edges();
        assert_eq!(
            focused.len(),
            2,
            "Lead focus should contain its Router edge and its outgoing pool"
        );
        assert!(focused.iter().any(|edge| edge.key == "router:engineering"));
        assert!(!focused.iter().any(|edge| edge.key == "router:research"));
        assert!(focused
            .iter()
            .any(|edge| edge.key == "call:engineering:research"));
        assert!(!focused
            .iter()
            .any(|edge| edge.key == "call:research:engineering"));
        app.show_all_edges = true;
        assert_eq!(
            app.visible_graph_edges().len(),
            4,
            "explicit exhaustive mode should expose every stored edge"
        );
        app.selection = Selection::Router;
        app.show_all_edges = false;
        let context = eframe::egui::Context::default();
        let output = context.run_ui(
            eframe::egui::RawInput {
                screen_rect: Some(eframe::egui::Rect::from_min_size(
                    eframe::egui::Pos2::ZERO,
                    eframe::egui::vec2(1320.0, 820.0),
                )),
                ..Default::default()
            },
            |ui| app.update(ui),
        );
        let mut text = String::new();
        for shape in &output.shapes {
            collect_shape_text(&shape.shape, &mut text);
        }
        for expected in [
            "Inspection Team",
            "inspection-team@4",
            "READ ONLY",
            "Show all edges",
            "Router",
            "inspect-router",
            "Route using the assignment definitions.",
            "engineering",
            "research",
        ] {
            assert!(
                text.contains(expected),
                "missing {expected:?} from:\n{text}"
            );
        }
        for forbidden in ["Publish revision", "Validate", "+ Lead", "+ Member"] {
            assert!(
                !text.contains(forbidden),
                "read-only surface exposed {forbidden:?}:\n{text}"
            );
        }
        assert_eq!(
            super::TEST_HOST_EXCHANGES.load(std::sync::atomic::Ordering::SeqCst),
            0,
            "rendering the read-only surface contacted the backend"
        );
    }

    #[test]
    fn editable_inspector_consumes_mouse_wheel_and_reaches_lower_fields() {
        let model = ModelChoice {
            gateway: "opencode".to_owned(),
            route: "zen".to_owned(),
            id: "model".to_owned(),
        };
        let draft = TeamDraft {
            id: "team-generated-id".to_owned(),
            name: "Scrollable Team".to_owned(),
            base_revision: 0,
            router: DraftAssignment {
                model: model.clone(),
                definition: String::new(),
            },
            members: vec![DraftMember {
                id: "engineering".to_owned(),
                model,
                definition: "A long editable definition.\n".repeat(30),
                lead: true,
            }],
            call_edges: vec![],
        };
        let mut app = TeamApp::new(
            0,
            draft,
            vec![],
            vec![],
            String::new(),
            false,
            RuntimeCapabilities::default(),
        );
        app.selection = Selection::Member(100);
        let context = eframe::egui::Context::default();
        let screen = eframe::egui::Rect::from_min_size(
            eframe::egui::Pos2::ZERO,
            eframe::egui::vec2(420.0, 220.0),
        );
        let mut content_height = 0.0;
        let mut viewport_height = 0.0;
        let _ = context.run_ui(
            eframe::egui::RawInput {
                screen_rect: Some(screen),
                ..Default::default()
            },
            |ui| {
                let output = app.scrollable_editor_inspector(ui);
                content_height = output.content_size.y;
                viewport_height = output.inner_rect.height();
            },
        );
        assert!(
            content_height > viewport_height,
            "inspector content ({content_height}) did not exceed viewport ({viewport_height})"
        );

        let mut offset = 0.0;
        let _ = context.run_ui(
            eframe::egui::RawInput {
                screen_rect: Some(screen),
                events: vec![
                    eframe::egui::Event::PointerMoved(eframe::egui::pos2(200.0, 110.0)),
                    eframe::egui::Event::MouseWheel {
                        unit: eframe::egui::MouseWheelUnit::Point,
                        delta: eframe::egui::vec2(0.0, -600.0),
                        phase: eframe::egui::TouchPhase::Move,
                        modifiers: eframe::egui::Modifiers::NONE,
                    },
                ],
                ..Default::default()
            },
            |ui| {
                offset = app.scrollable_editor_inspector(ui).state.offset.y;
            },
        );
        assert!(
            offset > 0.0,
            "mouse wheel did not move inspector offset: {offset}"
        );
    }

    #[test]
    fn editable_and_read_only_team_modes_share_exact_layout_engine_result() {
        let model = |id: &str| ModelChoice {
            gateway: "opencode".to_owned(),
            route: "zen".to_owned(),
            id: id.to_owned(),
        };
        let draft = TeamDraft {
            id: "free".to_owned(),
            name: "Free".to_owned(),
            base_revision: 1,
            router: DraftAssignment {
                model: model("big-pickle"),
                definition: "Route between the two Leads.".to_owned(),
            },
            members: vec![
                DraftMember {
                    id: "engineering".to_owned(),
                    model: model("deepseek-v4-flash-free"),
                    definition: "Own engineering.".to_owned(),
                    lead: true,
                },
                DraftMember {
                    id: "research".to_owned(),
                    model: model("laguna-s-2.1-free"),
                    definition: "Own research.".to_owned(),
                    lead: true,
                },
            ],
            call_edges: vec![
                CallEdge {
                    lead_id: "engineering".to_owned(),
                    member_id: "research".to_owned(),
                },
                CallEdge {
                    lead_id: "research".to_owned(),
                    member_id: "engineering".to_owned(),
                },
            ],
        };
        let mut editor = TeamApp::new(
            0,
            draft.clone(),
            vec![],
            vec![],
            String::new(),
            false,
            RuntimeCapabilities::default(),
        );
        let mut inspector = TeamApp::new(
            0,
            draft,
            vec![],
            vec![],
            String::new(),
            true,
            RuntimeCapabilities::default(),
        );
        let context = eframe::egui::Context::default();
        editor.relayout(&context);
        inspector.relayout(&context);
        assert_eq!(
            editor.view.layout, inspector.view.layout,
            "edit mode diverged from the shared Team-layout mathematics"
        );

        editor.view.layout.insert(
            super::router_node_id(),
            eframe::egui::pos2(19_000.0, -8_000.0),
        );
        editor.relayout(&context);
        assert_eq!(
            editor.view.layout, inspector.view.layout,
            "Auto layout did not restore the canonical shared result"
        );
    }

    #[test]
    fn graph_scene_never_magnifies_rasterized_text() {
        let range = pixel_perfect_scene_zoom_range();
        assert_eq!(range.max, 1.0);
        assert!(range.min > 0.0);
        assert!(range.min < range.max);
    }

    #[test]
    fn native_wake_advances_generation_and_requests_the_registered_surface() {
        let context = eframe::egui::Context::default();
        let registration = LiveRegistration::new(91_337, &context);
        assert_eq!(
            registration
                .generation
                .load(std::sync::atomic::Ordering::Acquire),
            0
        );
        assert_eq!(alt_native_gui_wake(91_337), 1);
        assert_eq!(
            registration
                .generation
                .load(std::sync::atomic::Ordering::Acquire),
            1
        );
        drop(registration);
        assert_eq!(alt_native_gui_wake(91_337), 0);
    }

    #[test]
    fn activity_marker_position_is_exact_polyline_arclength() {
        let points = [
            eframe::egui::pos2(0.0, 0.0),
            eframe::egui::pos2(3.0, 0.0),
            eframe::egui::pos2(3.0, 4.0),
        ];
        assert_eq!(
            point_along_polyline(&points, 0.5),
            Some(eframe::egui::pos2(3.0, 0.5))
        );
        assert_eq!(point_along_polyline(&points, 1.0), points.last().copied());
    }

    #[test]
    fn incident_edges_receive_unique_ports_in_geometric_order() {
        let source = NodeId::from_u64(1);
        let left = NodeId::from_u64(2);
        let middle = NodeId::from_u64(3);
        let right = NodeId::from_u64(4);
        let edges = vec![
            PortEdge {
                key: "right".to_owned(),
                from: source,
                to: right,
            },
            PortEdge {
                key: "left".to_owned(),
                from: source,
                to: left,
            },
            PortEdge {
                key: "middle".to_owned(),
                from: source,
                to: middle,
            },
        ];
        let centers = HashMap::from([
            (source, eframe::egui::pos2(0.0, 0.0)),
            (left, eframe::egui::pos2(-100.0, 100.0)),
            (middle, eframe::egui::pos2(0.0, 100.0)),
            (right, eframe::egui::pos2(100.0, 100.0)),
        ]);
        let assigned = assign_edge_ports(&edges, &centers);
        assert_eq!(assigned.outputs.get(&source), Some(&3));
        assert_eq!(assigned.endpoints["left"].0, (source, 0));
        assert_eq!(assigned.endpoints["middle"].0, (source, 1));
        assert_eq!(assigned.endpoints["right"].0, (source, 2));

        let mut reversed = edges.clone();
        reversed.reverse();
        assert_eq!(
            assign_edge_ports(&reversed, &centers).endpoints,
            assigned.endpoints,
            "port assignment depended on event iteration order"
        );
    }

    #[test]
    fn elapsed_annotation_is_parallel_to_and_clear_of_local_curve_segment() {
        let points = [eframe::egui::pos2(0.0, 0.0), eframe::egui::pos2(10.0, 10.0)];
        let sample = sample_polyline(&points, 0.5).expect("sample");
        let annotation = tangent_annotation(&points, 0.5, 10.0).expect("annotation");
        let displacement = annotation.anchor - sample.point;
        assert!(
            displacement.dot(sample.tangent).abs() < 0.0001,
            "annotation offset was not normal to the curve"
        );
        assert!((displacement.length() - 10.0).abs() < 0.0001);
        let label_direction = eframe::egui::vec2(annotation.angle.cos(), annotation.angle.sin());
        let cross = label_direction.x * sample.tangent.y - label_direction.y * sample.tangent.x;
        assert!(
            cross.abs() < 0.0001,
            "label baseline was not parallel to the curve tangent"
        );

        let reversed = [points[1], points[0]];
        let reversed_annotation =
            tangent_annotation(&reversed, 0.5, 10.0).expect("reversed annotation");
        assert!(
            reversed_annotation.angle.abs() <= std::f32::consts::FRAC_PI_2,
            "text was allowed to render upside down"
        );
    }

    #[test]
    fn return_traffic_cannot_move_the_stable_team_layout() {
        let mut state = ThinkingProjection {
            session_id: "session".to_owned(),
            active_turn_id: "turn".to_owned(),
            revision: 1,
            turns: vec![],
            active: ThinkingTurn {
                id: "turn".to_owned(),
                ordinal: 1,
                task: "task".to_owned(),
                status: "running".to_owned(),
                sequence: 4,
                nodes: BTreeMap::from([
                    (
                        "router".to_owned(),
                        ThinkingNode {
                            id: "router".to_owned(),
                            kind: "router".to_owned(),
                            label: "Router".to_owned(),
                            status: "completed".to_owned(),
                            actor: String::new(),
                            metadata: BTreeMap::new(),
                        },
                    ),
                    (
                        "member:lead".to_owned(),
                        ThinkingNode {
                            id: "member:lead".to_owned(),
                            kind: "member".to_owned(),
                            label: "lead".to_owned(),
                            status: "running".to_owned(),
                            actor: String::new(),
                            metadata: BTreeMap::new(),
                        },
                    ),
                ]),
                edges: BTreeMap::from([(
                    "allowed:router:lead".to_owned(),
                    ThinkingEdge {
                        id: "allowed:router:lead".to_owned(),
                        from: "router".to_owned(),
                        to: "member:lead".to_owned(),
                        kind: "allowed".to_owned(),
                        direction: "outward".to_owned(),
                        status: "idle".to_owned(),
                        count: 0,
                        active: 0,
                        started_at_ms: 0,
                        metadata: BTreeMap::new(),
                    },
                )]),
            },
        };
        let architectural = thinking_fingerprint(&state);
        let routing = thinking_routing_fingerprint(&state);
        state.active.edges.insert(
            "flow:result".to_owned(),
            ThinkingEdge {
                id: "flow:result".to_owned(),
                from: "member:lead".to_owned(),
                to: "router".to_owned(),
                kind: "result".to_owned(),
                direction: "inward".to_owned(),
                status: "completed".to_owned(),
                count: 1,
                active: 0,
                started_at_ms: 0,
                metadata: BTreeMap::new(),
            },
        );
        state.revision += 1;
        assert_eq!(
            thinking_fingerprint(&state),
            architectural,
            "a return event changed the architectural layout fingerprint"
        );
        assert_ne!(
            thinking_routing_fingerprint(&state),
            routing,
            "a return event did not invalidate its execution corridor"
        );
    }

    #[test]
    fn thinking_request_route_avoids_an_orbit_member_between_user_and_router() {
        let node = |id: &str, kind: &str| ThinkingNode {
            id: id.to_owned(),
            kind: kind.to_owned(),
            label: id.to_owned(),
            status: "idle".to_owned(),
            actor: String::new(),
            metadata: BTreeMap::new(),
        };
        let state = ThinkingProjection {
            session_id: "session".to_owned(),
            active_turn_id: "turn".to_owned(),
            revision: 1,
            turns: vec![],
            active: ThinkingTurn {
                id: "turn".to_owned(),
                ordinal: 1,
                task: "task".to_owned(),
                status: "running".to_owned(),
                sequence: 4,
                nodes: BTreeMap::from([
                    ("user".to_owned(), node("user", "user")),
                    ("router".to_owned(), node("router", "router")),
                    (
                        "member:engineering".to_owned(),
                        node("member:engineering", "member"),
                    ),
                    (
                        "member:research".to_owned(),
                        node("member:research", "member"),
                    ),
                ]),
                edges: BTreeMap::from([(
                    "flow:request".to_owned(),
                    ThinkingEdge {
                        id: "flow:request".to_owned(),
                        from: "user".to_owned(),
                        to: "router".to_owned(),
                        kind: "request".to_owned(),
                        direction: "outward".to_owned(),
                        status: "running".to_owned(),
                        count: 1,
                        active: 1,
                        started_at_ms: 1,
                        metadata: BTreeMap::new(),
                    },
                )]),
            },
        };
        // This is the topology in the reported sequence-4 screenshot: the
        // stable Router orbit places engineering directly between the User
        // and Router centres.
        let positions = HashMap::from([
            (thinking_node_id("user"), eframe::egui::pos2(0.0, -320.0)),
            (
                thinking_node_id("member:engineering"),
                eframe::egui::pos2(-15.0, -160.0),
            ),
            (thinking_node_id("router"), eframe::egui::pos2(0.0, 40.0)),
            (
                thinking_node_id("member:research"),
                eframe::egui::pos2(-15.0, 240.0),
            ),
        ]);
        let sizes = HashMap::from([
            (thinking_node_id("user"), eframe::egui::vec2(180.0, 72.0)),
            (
                thinking_node_id("member:engineering"),
                eframe::egui::vec2(210.0, 82.0),
            ),
            (thinking_node_id("router"), eframe::egui::vec2(180.0, 72.0)),
            (
                thinking_node_id("member:research"),
                eframe::egui::vec2(210.0, 82.0),
            ),
        ]);
        let routes = thinking_routes(&state, &positions, &sizes);
        let path = routes
            .path("flow:request")
            .expect("the obstructed request must receive a corridor");
        let blocker = eframe::egui::Rect::from_min_size(
            eframe::egui::pos2(-15.0, -160.0),
            eframe::egui::vec2(210.0, 82.0),
        );
        assert!(
            path.windows(2)
                .all(|segment| segment_rect_entry(segment[0], segment[1], blocker).is_none()),
            "the routed request still crosses the engineering node: {path:?}"
        );
    }

    #[test]
    fn arrow_head_points_in_final_segment_direction() {
        let points = [
            eframe::egui::pos2(0.0, 0.0),
            eframe::egui::pos2(10.0, 0.0),
            eframe::egui::pos2(10.0, 20.0),
        ];
        let triangle = arrow_head(&points, 8.0).expect("arrow");
        assert_eq!(triangle[0], points[2]);
        assert!(triangle[1].y < triangle[0].y);
        assert!(triangle[2].y < triangle[0].y);
        assert!((triangle[1].x + triangle[2].x - 20.0).abs() < 0.001);
    }

    #[test]
    fn arrow_head_ignores_duplicate_terminal_points() {
        let points = [
            eframe::egui::pos2(0.0, 0.0),
            eframe::egui::pos2(10.0, 0.0),
            eframe::egui::pos2(10.0, 0.0),
        ];
        assert!(arrow_head(&points, 6.0).is_some());
    }

    #[test]
    fn metadata_budget_is_derived_from_live_geometry() {
        assert_eq!(metadata_height_budget(600.0, 3, 18.0), 200.0);
        assert_eq!(
            metadata_height_budget(12.0, 4, 18.0),
            18.0,
            "at least one measured text row must remain visible"
        );
        assert_eq!(
            metadata_height_budget(600.0, 0, 18.0),
            600.0,
            "an empty remaining-field count is handled without division by zero"
        );
    }
}
