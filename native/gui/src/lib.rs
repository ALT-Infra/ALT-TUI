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

static CLIPBOARD_IMAGE: LazyLock<Mutex<Option<Vec<u8>>>> = LazyLock::new(|| Mutex::new(None));

fn clipboard_image_as_png() -> Option<Vec<u8>> {
    let mut clipboard = arboard::Clipboard::new().ok()?;
    let dynamic = clipboard
        .get()
        .file_list()
        .ok()
        .unwrap_or_default()
        .into_iter()
        .find_map(|path| image::open(path).ok())
        .or_else(|| {
            let value = clipboard.get_image().ok()?;
            let rgba = image::RgbaImage::from_raw(
                value.width as u32,
                value.height as u32,
                value.bytes.into_owned(),
            )?;
            Some(image::DynamicImage::ImageRgba8(rgba))
        })?;
    let mut encoded = Vec::new();
    dynamic
        .write_to(
            &mut std::io::Cursor::new(&mut encoded),
            image::ImageFormat::Png,
        )
        .ok()?;
    Some(encoded)
}

/// Read an image from the desktop clipboard and normalize it to PNG. The
/// first call uses a null buffer and returns the required length negatively;
/// the second copies the exact snapshot captured by that first call.
#[no_mangle]
pub extern "C" fn alt_native_gui_clipboard_image(buffer: *mut u8, capacity: usize) -> i64 {
    let mut pending = match CLIPBOARD_IMAGE.lock() {
        Ok(value) => value,
        Err(_) => return 0,
    };
    if buffer.is_null() || capacity == 0 {
        *pending = clipboard_image_as_png();
        return pending
            .as_ref()
            .map(|bytes| -(bytes.len() as i64))
            .unwrap_or(0);
    }
    let Some(bytes) = pending.as_ref() else {
        return 0;
    };
    if capacity < bytes.len() {
        return -(bytes.len() as i64);
    }
    // SAFETY: the Go caller supplies a writable buffer of capacity bytes.
    unsafe { std::ptr::copy_nonoverlapping(bytes.as_ptr(), buffer, bytes.len()) };
    let length = bytes.len() as i64;
    *pending = None;
    length
}

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
    #[serde(skip_serializing_if = "Option::is_none")]
    view: Option<&'a str>,
    #[serde(skip_serializing_if = "Option::is_none")]
    profile_id: Option<&'a str>,
    #[serde(skip_serializing_if = "Option::is_none")]
    revision: Option<i32>,
    #[serde(skip_serializing_if = "Option::is_none")]
    gateway: Option<&'a str>,
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
    view: String,
    #[serde(default)]
    runtime: RuntimeCapabilities,
    #[serde(default)]
    catalog: Vec<CatalogModel>,
    #[serde(default)]
    gateways: Vec<GatewayDescriptor>,
    #[serde(default)]
    profiles: Vec<ProfileSummary>,
    draft: Option<TeamDraft>,
    thinking: Option<ThinkingProjection>,
    #[serde(default)]
    diagnostics: Vec<Diagnostic>,
}

#[derive(Clone, Debug, Deserialize)]
struct ProfileSummary {
    #[serde(rename = "ID")]
    id: String,
    #[serde(rename = "Revision")]
    revision: i32,
    #[serde(rename = "Name")]
    name: String,
}

#[derive(Clone, Debug, Default, Deserialize)]
struct RuntimeCapabilities {
    dangerously_bypass_approvals_and_sandbox: bool,
    filesystem_confinement: bool,
    direct_terminal_network: bool,
    exa_configured: bool,
    #[serde(default)]
    linkup_configured: bool,
    #[serde(default)]
    research_provider: String,
}

#[derive(Clone, Debug, Deserialize)]
struct CatalogModel {
    route: String,
    id: String,
}

#[derive(Clone, Debug, Deserialize)]
struct GatewayDescriptor {
    id: String,
    name: String,
}

#[derive(Clone, Debug, Default)]
struct TeamDraft {
    id: String,
    name: String,
    base_revision: i32,
    gateway: String,
    primary_id: String,
    primary: DraftAssignment,
    members: Vec<DraftMember>,
    peer_ids: Vec<String>,
    call_edges: Vec<CallEdge>,
    peer_edges: Vec<PeerEdge>,
}

#[derive(Clone, Debug, Default, Deserialize, Serialize)]
struct WireTeamDraft {
    id: String,
    name: String,
    base_revision: i32,
    gateway: String,
    primary: DraftMember,
    #[serde(default)]
    peers: Vec<DraftMember>,
    #[serde(default)]
    specialists: Vec<DraftMember>,
    #[serde(default)]
    peer_edges: Vec<WirePeerEdge>,
    #[serde(default)]
    specialist_edges: Vec<WireSpecialistEdge>,
}

#[derive(Clone, Debug, Default, Deserialize, Serialize)]
struct WirePeerEdge {
    first_agent_id: String,
    second_agent_id: String,
}

#[derive(Clone, Debug, Default, Deserialize, Serialize)]
struct WireSpecialistEdge {
    agent_id: String,
    specialist_id: String,
}

impl<'de> Deserialize<'de> for TeamDraft {
    fn deserialize<D>(deserializer: D) -> Result<Self, D::Error>
    where
        D: serde::Deserializer<'de>,
    {
        let wire = WireTeamDraft::deserialize(deserializer)?;
        let primary_id = wire.primary.id.clone();
        let primary = DraftAssignment {
            model: wire.primary.model,
            definition: wire.primary.definition,
        };
        let peer_ids = wire.peers.iter().map(|peer| peer.id.clone()).collect();
        let mut members = wire.peers;
        members.extend(wire.specialists);
        Ok(Self {
            id: wire.id,
            name: wire.name,
            base_revision: wire.base_revision,
            gateway: wire.gateway,
            primary_id,
            primary,
            members,
            peer_ids,
            call_edges: wire
                .specialist_edges
                .into_iter()
                .map(|edge| CallEdge {
                    agent_id: edge.agent_id,
                    specialist_id: edge.specialist_id,
                })
                .collect(),
            peer_edges: wire
                .peer_edges
                .into_iter()
                .map(|edge| PeerEdge {
                    first_agent_id: edge.first_agent_id,
                    second_agent_id: edge.second_agent_id,
                })
                .collect(),
        })
    }
}

impl Serialize for TeamDraft {
    fn serialize<S>(&self, serializer: S) -> Result<S::Ok, S::Error>
    where
        S: serde::Serializer,
    {
        let agents: HashSet<&str> = self.peer_ids.iter().map(String::as_str).collect();
        let peers = self
            .members
            .iter()
            .filter(|member| agents.contains(member.id.as_str()))
            .cloned()
            .collect();
        let specialists = self
            .members
            .iter()
            .filter(|member| !agents.contains(member.id.as_str()))
            .cloned()
            .collect();
        WireTeamDraft {
            id: self.id.clone(),
            name: self.name.clone(),
            base_revision: self.base_revision,
            gateway: self.gateway.clone(),
            primary: DraftMember {
                id: self.primary_id.clone(),
                model: self.primary.model.clone(),
                definition: self.primary.definition.clone(),
            },
            peers,
            specialists,
            peer_edges: self
                .peer_edges
                .iter()
                .map(|edge| WirePeerEdge {
                    first_agent_id: edge.first_agent_id.clone(),
                    second_agent_id: edge.second_agent_id.clone(),
                })
                .collect(),
            specialist_edges: self
                .call_edges
                .iter()
                .map(|edge| WireSpecialistEdge {
                    agent_id: edge.agent_id.clone(),
                    specialist_id: edge.specialist_id.clone(),
                })
                .collect(),
        }
        .serialize(serializer)
    }
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
}

#[derive(Clone, Debug, Default, Deserialize, Serialize)]
struct CallEdge {
    agent_id: String,
    specialist_id: String,
}

#[derive(Clone, Debug, Default, Deserialize, Serialize)]
struct PeerEdge {
    first_agent_id: String,
    second_agent_id: String,
}

#[derive(Clone, Debug, Default, Deserialize, Serialize)]
struct ModelChoice {
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
    let request = serde_json::to_vec(&HostRequest {
        operation,
        draft,
        view: None,
        profile_id: None,
        revision: None,
        gateway: None,
    })
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

fn host_open_team(
    handle: u64,
    view: &str,
    profile_id: Option<&str>,
    revision: Option<i32>,
) -> Result<HostResponse, String> {
    let request = serde_json::to_vec(&HostRequest {
        operation: "team.open",
        draft: None,
        view: Some(view),
        profile_id,
        revision,
        gateway: None,
    })
    .map_err(|error| format!("encode host request: {error}"))?;
    host_exchange_bytes(handle, request)
}

fn host_select_gateway(
    handle: u64,
    gateway: &str,
    draft: &TeamDraft,
) -> Result<HostResponse, String> {
    let request = serde_json::to_vec(&HostRequest {
        operation: "team.gateway",
        draft: Some(draft),
        view: None,
        profile_id: None,
        revision: None,
        gateway: Some(gateway),
    })
    .map_err(|error| format!("encode host request: {error}"))?;
    host_exchange_bytes(handle, request)
}

fn host_exchange_bytes(handle: u64, request: Vec<u8>) -> Result<HostResponse, String> {
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
        "team" => "ALT — Team",
        _ => "ALT",
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
                "team" => AltApp::Team(TeamApp::new(
                    handle,
                    state.draft.unwrap_or_default(),
                    state.catalog,
                    state.gateways,
                    state.profiles,
                    state.diagnostics,
                    initial.error,
                    TeamView::from_wire(&state.view),
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
    Primary,
    Member(u64),
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
enum TeamView {
    New,
    Edit,
    Inspect,
}

impl TeamView {
    fn from_wire(value: &str) -> Self {
        match value {
            "edit" => Self::Edit,
            "inspect" => Self::Inspect,
            _ => Self::New,
        }
    }

    fn wire(self) -> &'static str {
        match self {
            Self::New => "new",
            Self::Edit => "edit",
            Self::Inspect => "inspect",
        }
    }

    fn label(self) -> &'static str {
        match self {
            Self::New => "New Team",
            Self::Edit => "Edit Team",
            Self::Inspect => "Inspect Team",
        }
    }
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
enum ConnectionKind {
    Call,
    Peer,
}

struct TeamApp {
    handle: u64,
    team_view: TeamView,
    draft: TeamDraft,
    catalog: Vec<CatalogModel>,
    gateways: Vec<GatewayDescriptor>,
    profiles: Vec<ProfileSummary>,
    member_uids: Vec<u64>,
    next_uid: u64,
    selection: Selection,
    edge_in_progress: Option<(NodeId, SocketKind, usize)>,
    selected_edges: HashSet<String>,
    show_all_edges: bool,
    view: egui_graph::View,
    graph_screen_rect: egui::Rect,
    routes: EdgeRoutes,
    layout_frames: u8,
    model_filter: String,
    diagnostics: Vec<Diagnostic>,
    status: String,
    runtime: RuntimeCapabilities,
    connection_kind: ConnectionKind,
    choose_target_for: Option<TeamView>,
    inspector_open: bool,
}

impl TeamApp {
    #[allow(clippy::too_many_arguments)] // Mirrors the typed native init payload.
    fn new(
        handle: u64,
        draft: TeamDraft,
        catalog: Vec<CatalogModel>,
        gateways: Vec<GatewayDescriptor>,
        profiles: Vec<ProfileSummary>,
        diagnostics: Vec<Diagnostic>,
        init_error: String,
        team_view: TeamView,
        runtime: RuntimeCapabilities,
    ) -> Self {
        let member_uids: Vec<u64> = (0..draft.members.len())
            .map(|index| 100 + index as u64)
            .collect();
        let next_uid = 100 + member_uids.len() as u64;
        Self {
            handle,
            team_view,
            draft,
            catalog,
            gateways,
            profiles,
            member_uids,
            next_uid,
            selection: Selection::Primary,
            edge_in_progress: None,
            selected_edges: HashSet::new(),
            show_all_edges: team_view == TeamView::Inspect,
            view: Default::default(),
            graph_screen_rect: egui::Rect::NOTHING,
            routes: Default::default(),
            layout_frames: 3,
            model_filter: String::new(),
            diagnostics,
            status: init_error,
            runtime,
            connection_kind: ConnectionKind::Peer,
            choose_target_for: None,
            inspector_open: false,
        }
    }

    fn read_only(&self) -> bool {
        self.team_view == TeamView::Inspect
    }

    fn is_agent(&self, id: &str) -> bool {
        if id == self.draft.primary_id {
            return true;
        }
        self.draft.peer_ids.iter().any(|candidate| candidate == id)
    }

    #[cfg(test)]
    fn graph_to_screen(&self, point: egui::Pos2) -> egui::Pos2 {
        let scene = self.view.scene_rect;
        let scale = pixel_perfect_scene_zoom_range()
            .clamp((self.graph_screen_rect.size() / scene.size()).min_elem());
        let translation =
            self.graph_screen_rect.center().to_vec2() - scale * scene.center().to_vec2();
        egui::Pos2::new(
            translation.x + scale * point.x,
            translation.y + scale * point.y,
        )
    }

    fn update(&mut self, root: &mut egui::Ui) {
        let ctx = root.ctx().clone();
        let mut requested_gateway = None;
        if ctx.input(|input| input.key_pressed(egui::Key::Escape)) {
            self.inspector_open = false;
        }
        egui::Panel::top("team-toolbar").show_inside(root, |ui| {
            ui.horizontal_wrapped(|ui| {
                ui.strong(if self.draft.name.is_empty() {
                    "Team"
                } else {
                    &self.draft.name
                });
                ui.separator();
                let previous = self.team_view;
                egui::ComboBox::from_id_salt("team-view-switcher")
                    .selected_text(self.team_view.label())
                    .show_ui(ui, |ui| {
                        ui.selectable_value(&mut self.team_view, TeamView::New, "New Team");
                        ui.selectable_value(&mut self.team_view, TeamView::Edit, "Edit Team");
                        ui.selectable_value(&mut self.team_view, TeamView::Inspect, "Inspect Team");
                    });
                if self.team_view != previous {
                    let requested = self.team_view;
                    self.team_view = previous;
                    self.request_view(requested);
                }
                ui.separator();
                if self.read_only() {
                    ui.weak("Gateway");
                    ui.monospace(gateway_label(&self.gateways, &self.draft.gateway));
                } else {
                    let current = self.draft.gateway.clone();
                    let mut selected = current.clone();
                    egui::ComboBox::from_id_salt("team-gateway-switcher")
                        .selected_text(if current.is_empty() {
                            "Choose gateway account".to_owned()
                        } else {
                            gateway_label(&self.gateways, &current)
                        })
                        .show_ui(ui, |ui| {
                            for gateway in &self.gateways {
                                ui.selectable_value(
                                    &mut selected,
                                    gateway.id.clone(),
                                    format!("{} · {}", gateway.name, gateway.id),
                                );
                            }
                        });
                    if selected != current {
                        requested_gateway = Some(selected);
                    }
                }
                if self.read_only() {
                    ui.separator();
                    ui.monospace(format!("{}@{}", self.draft.id, self.draft.base_revision));
                    ui.separator();
                    let edge_label = if self.show_all_edges {
                        "Focus selection"
                    } else {
                        "Show all edges"
                    };
                    if ui.button(edge_label).clicked() {
                        self.show_all_edges = !self.show_all_edges;
                    }
                }
                ui.separator();
                runtime_policy_badge(ui, &self.runtime);
                if ui.button("Auto layout").clicked() {
                    self.layout_frames = 3;
                }
                if !self.inspector_open && ui.button("Details").clicked() {
                    self.inspector_open = true;
                }
            });
            if !self.read_only() {
                ui.horizontal_wrapped(|ui| {
                    ui.label("Name");
                    ui.add(egui::TextEdit::singleline(&mut self.draft.name).desired_width(360.0));
                    ui.separator();
                    if ui.button("+ Peer").clicked() {
                        self.add_member(true);
                    }
                    if ui.button("+ Specialist").clicked() {
                        self.add_member(false);
                    }
                    ui.separator();
                    ui.weak("Draw");
                    ui.selectable_value(&mut self.connection_kind, ConnectionKind::Call, "Call");
                    ui.selectable_value(&mut self.connection_kind, ConnectionKind::Peer, "Peer");
                    ui.separator();
                    if ui.button("Validate").clicked() {
                        self.validate();
                    }
                    let publish = egui::Button::new("Publish revision")
                        .fill(egui::Color32::from_rgb(25, 110, 137));
                    if ui.add(publish).clicked() {
                        self.publish();
                    }
                    if !self.status.is_empty() {
                        ui.separator();
                        ui.label(&self.status);
                    }
                });
            }
        });

        if let Some(gateway) = requested_gateway {
            self.select_gateway(&gateway);
        }

        if !self.read_only() {
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

        egui::CentralPanel::default()
            .frame(egui::Frame::default().fill(egui::Color32::from_rgb(13, 15, 18)))
            .show_inside(root, |ui| self.graph(ui));

        if self.inspector_open {
            let screen = ctx.content_rect();
            let width = (screen.width() * 0.32).clamp(360.0, 520.0);
            let height = (screen.height() * 0.72).clamp(360.0, 760.0);
            let mut open = true;
            egui::Window::new("Details")
                .id(egui::Id::new("team-inspector"))
                .open(&mut open)
                .default_pos(egui::pos2(
                    (screen.right() - width - 18.0).max(screen.left() + 18.0),
                    screen.top() + 58.0,
                ))
                .default_size(egui::vec2(width, height))
                .min_width(320.0)
                .max_width(560.0)
                .max_height(screen.height() - 88.0)
                .resizable(true)
                .collapsible(false)
                .show(&ctx, |ui| {
                    if self.read_only() {
                        egui::ScrollArea::vertical()
                            .id_salt("team-read-only-inspector-scroll")
                            .auto_shrink([false, false])
                            .show(ui, |ui| self.read_only_inspector(ui));
                    } else {
                        self.scrollable_editor_inspector(ui);
                    }
                });
            self.inspector_open = open;
        }

        self.target_picker(&ctx);
    }

    fn add_member(&mut self, peer: bool) {
        let prefix = if peer { "peer" } else { "specialist" };
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
            id: id.clone(),
            ..Default::default()
        });
        if peer {
            self.draft.peer_ids.push(id);
        }
        self.selection = Selection::Member(uid);
        self.layout_frames = 3;
        self.status.clear();
    }

    fn request_view(&mut self, requested: TeamView) {
        if requested == TeamView::New {
            self.open_team_view(requested, None, None);
        } else if self.draft.base_revision > 0 {
            let id = self.draft.id.clone();
            self.open_team_view(requested, Some(&id), Some(self.draft.base_revision));
        } else {
            self.choose_target_for = Some(requested);
        }
    }

    fn select_gateway(&mut self, gateway: &str) {
        match host_select_gateway(self.handle, gateway, &self.draft) {
            Ok(response) => {
                if let Some(initial) = response.initial {
                    self.catalog = initial.catalog;
                    self.gateways = initial.gateways;
                    self.diagnostics = initial.diagnostics;
                    self.status = response.error;
                    if let Some(draft) = initial.draft {
                        self.draft = draft;
                    }
                    self.selection = Selection::Primary;
                    self.model_filter.clear();
                } else {
                    self.status = response.error;
                }
            }
            Err(error) => self.status = error,
        }
    }

    fn open_team_view(&mut self, view: TeamView, id: Option<&str>, revision: Option<i32>) {
        match host_open_team(self.handle, view.wire(), id, revision) {
            Ok(response) => {
                if let Some(initial) = response.initial {
                    self.team_view = TeamView::from_wire(&initial.view);
                    self.catalog = initial.catalog;
                    self.gateways = initial.gateways;
                    self.profiles = initial.profiles;
                    self.diagnostics = initial.diagnostics;
                    self.status = response.error;
                    if let Some(draft) = initial.draft {
                        self.draft = draft;
                        self.member_uids = (0..self.draft.members.len())
                            .map(|index| 100 + index as u64)
                            .collect();
                        self.next_uid = 100 + self.member_uids.len() as u64;
                        self.selection = Selection::Primary;
                        self.selected_edges.clear();
                        self.layout_frames = 3;
                    }
                    self.choose_target_for = None;
                } else {
                    self.status = response.error;
                }
            }
            Err(error) => self.status = error,
        }
    }

    fn target_picker(&mut self, ctx: &egui::Context) {
        let Some(view) = self.choose_target_for else {
            return;
        };
        let mut open = true;
        egui::Window::new(format!("{} — choose a revision", view.label()))
            .collapsible(false)
            .resizable(true)
            .open(&mut open)
            .show(ctx, |ui| {
                ui.label("Choose the immutable Team revision to open.");
                ui.add_space(6.0);
                let profiles = self.profiles.clone();
                egui::ScrollArea::vertical()
                    .max_height(360.0)
                    .show(ui, |ui| {
                        for item in profiles {
                            let label = format!("{}@{} · {}", item.id, item.revision, item.name);
                            if ui.button(label).clicked() {
                                self.open_team_view(view, Some(&item.id), Some(item.revision));
                            }
                        }
                    });
                if self.profiles.is_empty() {
                    ui.weak("No published Teams exist yet. Switch to New Team first.");
                }
            });
        if !open {
            self.choose_target_for = None;
        }
    }

    fn read_only_inspector(&self, ui: &mut egui::Ui) {
        egui::ScrollArea::vertical().show(ui, |ui| {
            match self.selection {
                Selection::Primary => {
                    ui.heading(&self.draft.primary_id);
                    ui.label("Primary · entry point for every user turn");
                    ui.add_space(10.0);
                    read_only_model(ui, &self.draft.primary.model);
                    read_only_definition(ui, &self.draft.primary.definition);
                    ui.add_space(12.0);
                    ui.strong("Peer relationships");
                    let mut any = false;
                    for edge in &self.draft.peer_edges {
                        if edge.first_agent_id == self.draft.primary_id {
                            any = true;
                            ui.monospace(&edge.second_agent_id);
                        } else if edge.second_agent_id == self.draft.primary_id {
                            any = true;
                            ui.monospace(&edge.first_agent_id);
                        }
                    }
                    if !any {
                        ui.weak("No peers assigned.");
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
                    let is_agent = self.is_agent(&member.id);
                    ui.label(if is_agent {
                        "Peer · may consult or hold leadership"
                    } else {
                        "Stateless specialist"
                    });
                    ui.add_space(10.0);
                    read_only_model(ui, &member.model);
                    read_only_definition(ui, &member.definition);

                    if is_agent {
                        ui.add_space(12.0);
                        ui.strong("Can call");
                        let mut any = false;
                        for edge in self
                            .draft
                            .call_edges
                            .iter()
                            .filter(|edge| edge.agent_id == member.id)
                        {
                            any = true;
                            ui.monospace(&edge.specialist_id);
                        }
                        if !any {
                            ui.weak("No callable members assigned.");
                        }
                        ui.add_space(8.0);
                        ui.strong("Can collaborate with peers");
                        let mut any_peer = false;
                        for edge in &self.draft.peer_edges {
                            if edge.first_agent_id == member.id {
                                any_peer = true;
                                ui.monospace(&edge.second_agent_id);
                            } else if edge.second_agent_id == member.id {
                                any_peer = true;
                                ui.monospace(&edge.first_agent_id);
                            }
                        }
                        if !any_peer {
                            ui.weak("No peer relationships assigned.");
                        }
                    }
                    ui.add_space(12.0);
                    ui.strong("Callable by");
                    let mut any = false;
                    for edge in self
                        .draft
                        .call_edges
                        .iter()
                        .filter(|edge| edge.specialist_id == member.id)
                    {
                        any = true;
                        ui.monospace(&edge.agent_id);
                    }
                    if !any {
                        ui.weak("Not callable by an agent.");
                    }
                    ui.add_space(8.0);
                    ui.strong("Peer to");
                    let mut any_peer = false;
                    for edge in self
                        .draft
                        .peer_edges
                        .iter()
                        .filter(|edge| edge.second_agent_id == member.id)
                    {
                        any_peer = true;
                        ui.monospace(&edge.first_agent_id);
                    }
                    if !any_peer {
                        ui.weak("Not available for stateful peer work.");
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
            Selection::Primary => {
                ui.heading("Primary");
                ui.weak("Mandatory entry point for every user turn; may answer, consult, call specialists, or hand leadership to a peer.");
                ui.label("Identity");
                let old_id = self.draft.primary_id.clone();
                if ui
                    .add(
                        egui::TextEdit::singleline(&mut self.draft.primary_id)
                            .desired_width(f32::INFINITY),
                    )
                    .changed()
                {
                    for edge in &mut self.draft.call_edges {
                        if edge.agent_id == old_id {
                            edge.agent_id.clone_from(&self.draft.primary_id);
                        }
                    }
                    for edge in &mut self.draft.peer_edges {
                        if edge.first_agent_id == old_id {
                            edge.first_agent_id.clone_from(&self.draft.primary_id);
                        }
                        if edge.second_agent_id == old_id {
                            edge.second_agent_id.clone_from(&self.draft.primary_id);
                        }
                    }
                }
                ui.add_space(8.0);
                model_editor(
                    ui,
                    &self.catalog,
                    &mut self.model_filter,
                    &mut self.draft.primary.model,
                );
                definition_editor(ui, &mut self.draft.primary.definition);
            }
            Selection::Member(uid) => {
                let Some(index) = self
                    .member_uids
                    .iter()
                    .position(|candidate| *candidate == uid)
                else {
                    self.selection = Selection::Primary;
                    return;
                };
                let old_id = self.draft.members[index].id.clone();
                let identity_changed;
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
                    let role = if self.draft.peer_ids.iter().any(|id| id == &member.id) {
                        "Peer — context-bearing and eligible to hold leadership"
                    } else {
                        "Specialist — thoroughly stateless on every call"
                    };
                    ui.weak(role);
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
                        if edge.agent_id == old_id {
                            edge.agent_id.clone_from(&new_id);
                        }
                        if edge.specialist_id == old_id {
                            edge.specialist_id.clone_from(&new_id);
                        }
                    }
                    for edge in &mut self.draft.peer_edges {
                        if edge.first_agent_id == old_id {
                            edge.first_agent_id.clone_from(&new_id);
                        }
                        if edge.second_agent_id == old_id {
                            edge.second_agent_id.clone_from(&new_id);
                        }
                    }
                    for id in &mut self.draft.peer_ids {
                        if *id == old_id {
                            id.clone_from(&new_id);
                        }
                    }
                    self.layout_frames = 2;
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
        self.graph_screen_rect = ui.available_rect_before_wrap();
        let center_view = self.layout_frames == 1;
        if self.layout_frames > 0 {
            self.relayout(ui.ctx());
            self.layout_frames -= 1;
        }
        let selected_nodes = HashSet::from([match self.selection {
            Selection::Primary => primary_node_id(),
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
            // The grid is a direct-manipulation affordance: it makes camera
            // movement and alignment legible while authoring. In Inspect it
            // competes with the stored topology without adding information.
            .dot_grid(!self.read_only())
            .wheel_zoom(true)
            .zoom_range(pixel_perfect_scene_zoom_range())
            .prevent_node_overlap(!self.read_only())
            .node_clearance(12.0)
            .marquee_selection(false)
            .selected_nodes(selected_nodes);
        if self.read_only() {
            graph = graph
                .drag_pan_buttons(
                    egui::containers::DragPanButtons::PRIMARY
                        | egui::containers::DragPanButtons::MIDDLE,
                )
                .immutable(true)
                .align(false);
        } else {
            // The fork's graph-aware gesture reserves primary drags that begin
            // on nodes and sockets for editing, and pans only when the gesture
            // begins on empty canvas. This removes marquee selection without
            // sacrificing node movement or connection drawing.
            graph = graph.primary_drag_pan_empty(true).align(true);
        }
        let response = graph.show(&mut view, ui, |ui, show| {
            if self.read_only() {
                self.render_read_only_scaffold(ui, &edge_layout);
            }
            show.nodes(ui, |nctx, ui| self.render_nodes(nctx, ui, &ports))
                .edges(ui, |ectx, ui| {
                    self.render_edges(ectx, ui, &edge_layout, &ports)
                });
        });
        self.view = view;
        if !self.read_only() && self.view.layout != edge_layout {
            self.reroute(ui.ctx());
            ui.ctx().request_repaint();
        }
        if let Some(selected) = response.selection_changed {
            if selected.contains(&primary_node_id()) {
                self.selection = Selection::Primary;
                self.inspector_open = true;
                if self.read_only() {
                    self.show_all_edges = false;
                }
            } else if let Some(id) = selected.iter().next() {
                self.selection = Selection::Member(id.0);
                self.inspector_open = true;
                if self.read_only() {
                    self.show_all_edges = false;
                }
            }
        }
        if !self.read_only() {
            response.response.context_menu(|ui| {
                ui.strong("Create Team role");
                if ui.button("Peer").clicked() {
                    self.add_member(true);
                    ui.close();
                }
                if ui.button("Specialist").clicked() {
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
        let primary_outputs = ports
            .outputs
            .get(&primary_node_id())
            .copied()
            .unwrap_or_default();
        let primary_response = egui_graph::node::Node::from_id(primary_node_id())
            .outputs(if self.read_only() {
                0
            } else {
                primary_outputs.max(1)
            })
            .socket_radius(6.0)
            .socket_color(egui::Color32::from_rgb(112, 180, 206))
            .flow(egui::Direction::TopDown)
            .animation_time(if self.read_only() { 0.0 } else { 0.1 })
            .show(nctx, ui, |node_ctx| {
                let focused = self.node_in_focus(primary_node_id());
                let selected = node_ctx.interaction().selected;
                let mut frame =
                    egui_graph::node::default_frame(node_ctx.style(), node_ctx.interaction());
                frame.fill = if focused {
                    egui::Color32::from_rgb(25, 22, 37)
                } else {
                    egui::Color32::from_rgb(17, 17, 22)
                };
                if !selected {
                    frame.stroke = egui::Stroke::new(
                        1.6,
                        egui::Color32::from_rgb(178, 132, 255).gamma_multiply(if focused {
                            1.0
                        } else {
                            0.22
                        }),
                    );
                }
                node_ctx.framed_with(frame, |ui, _| {
                    if self.read_only() {
                        ui.visuals_mut().disabled_alpha = 1.0;
                    }
                    ui.label(egui::RichText::new("PRIMARY").strong().color(
                        egui::Color32::from_rgb(211, 184, 255).gamma_multiply(if focused {
                            1.0
                        } else {
                            0.3
                        }),
                    ));
                    ui.strong(&self.draft.primary_id);
                    model_badge(ui, &self.draft.primary.model);
                })
            });
        if let Some(event) = primary_response.edge_event() {
            edge_events.push((primary_node_id(), event));
        }

        let mut deletions = Vec::new();
        for (index, member) in self.draft.members.iter().enumerate() {
            let uid = self.member_uids[index];
            let node_id = NodeId::from_u64(uid);
            let agent = self.is_agent(&member.id);
            let roles = if agent { "PEER" } else { "SPECIALIST" };
            let inputs = ports.inputs.get(&node_id).copied().unwrap_or_default();
            let outputs = ports.outputs.get(&node_id).copied().unwrap_or_default();
            let response = egui_graph::node::Node::from_id(node_id)
                .inputs(if self.read_only() { 0 } else { inputs.max(1) })
                .outputs(if self.read_only() || !agent {
                    0
                } else {
                    outputs.max(1)
                })
                .flow(egui::Direction::TopDown)
                .animation_time(if self.read_only() { 0.0 } else { 0.1 })
                .socket_radius(6.0)
                .socket_color(match self.connection_kind {
                    ConnectionKind::Call => egui::Color32::from_rgb(69, 184, 218),
                    ConnectionKind::Peer => egui::Color32::from_rgb(236, 190, 74),
                })
                .show(nctx, ui, |node_ctx| {
                    let focused = self.node_in_focus(node_id);
                    let selected = node_ctx.interaction().selected;
                    let accent = (if agent {
                        egui::Color32::from_rgb(99, 164, 255)
                    } else {
                        egui::Color32::from_rgb(82, 211, 158)
                    })
                    .gamma_multiply(if focused { 1.0 } else { 0.22 });
                    let mut frame =
                        egui_graph::node::default_frame(node_ctx.style(), node_ctx.interaction());
                    frame.fill = if !focused {
                        egui::Color32::from_rgb(16, 18, 20)
                    } else if agent {
                        egui::Color32::from_rgb(18, 28, 43)
                    } else {
                        egui::Color32::from_rgb(17, 31, 28)
                    };
                    if !selected {
                        frame.stroke = egui::Stroke::new(if agent { 1.5 } else { 1.15 }, accent);
                    }
                    node_ctx.framed_with(frame, |ui, _| {
                        if self.read_only() {
                            ui.visuals_mut().disabled_alpha = 1.0;
                        }
                        if !focused {
                            ui.visuals_mut().override_text_color =
                                Some(egui::Color32::from_rgb(73, 79, 84));
                        }
                        ui.label(
                            egui::RichText::new(if member.id.is_empty() {
                                "unnamed"
                            } else {
                                &member.id
                            })
                            .strong()
                            .color(accent),
                        );
                        ui.label(
                            egui::RichText::new(roles)
                                .small()
                                .color(accent.gamma_multiply(0.76)),
                        );
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
        if self.read_only() {
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
            let mut selected = !self.read_only() && self.selected_edges.contains(&edge.key);
            let widget =
                egui_graph::edge::Edge::new(pair.0, pair.1, &mut selected).waypoints(waypoints);
            let color = match edge.kind {
                GraphEdgeKind::Call => egui::Color32::from_rgb(69, 184, 218),
                GraphEdgeKind::Peer => egui::Color32::from_rgb(236, 190, 74),
            };
            let widget = widget
                .stroke(egui::Stroke::new(1.8, color))
                .hovered_stroke(egui::Stroke::new(2.6, color))
                .selected_stroke(egui::Stroke::new(3.0, color));
            let kind = edge.kind;
            let response = widget.show_with(ectx, ui, |ui, paint| {
                if matches!(kind, GraphEdgeKind::Peer) {
                    ui.painter().extend(egui::Shape::dashed_line(
                        paint.points,
                        paint.stroke,
                        8.0,
                        5.0,
                    ));
                } else {
                    ui.painter()
                        .add(egui::Shape::line(paint.points.to_vec(), paint.stroke));
                }
                if let Some(head) = arrow_head(paint.points, 8.0) {
                    ui.painter().add(egui::Shape::convex_polygon(
                        head.to_vec(),
                        paint.stroke.color,
                        egui::Stroke::NONE,
                    ));
                }
                if matches!(kind, GraphEdgeKind::Peer) {
                    let reversed: Vec<_> = paint.points.iter().rev().copied().collect();
                    if let Some(head) = arrow_head(&reversed, 8.0) {
                        ui.painter().add(egui::Shape::convex_polygon(
                            head.to_vec(),
                            paint.stroke.color,
                            egui::Stroke::NONE,
                        ));
                    }
                }
            });
            if !self.read_only() {
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
                if id == primary_node_id() {
                    egui::vec2(180.0, 72.0)
                } else {
                    egui::vec2(220.0, 92.0)
                }
            });
            Some(egui::Rect::from_min_size(position, size))
        };
        let node_rects: Vec<_> = std::iter::once(primary_node_id())
            .chain(self.member_uids.iter().copied().map(NodeId::from_u64))
            .filter_map(|id| Some((id, rect_for(id)?)))
            .collect();

        // Route the complete visible relation set together. Independent
        // center-ray routing makes dense graphs look tangled even when every
        // individual path avoids nodes: several relations leave a rectangle
        // through the same visual point. The live primary allocates distinct,
        // monotonically ordered free-boundary ports before obstacle routing,
        // which gives Inspect the same locally traceable connection grammar as
        // live execution without changing the stable Team placement.
        let edges = self.visible_graph_edges();
        let flow_nodes: Vec<_> = node_rects
            .iter()
            .map(|(id, rect)| flow_router::FlowNode {
                id: *id,
                rect: *rect,
            })
            .collect();
        let flow_edges: Vec<_> = edges
            .iter()
            .map(|edge| flow_router::FlowEdge {
                key: edge.key.clone(),
                from: edge.from,
                to: edge.to,
            })
            .collect();
        let routes = flow_router::route_flow_edges(&flow_nodes, &flow_edges, 14.0);

        for edge in edges {
            let Some(points) = routes.path(&edge.key).map(|path| path.to_vec()) else {
                continue;
            };
            if points.len() < 2 {
                continue;
            }
            let color = match edge.kind {
                GraphEdgeKind::Call if self.show_all_edges => {
                    egui::Color32::from_rgba_unmultiplied(69, 184, 218, 122)
                }
                GraphEdgeKind::Call => egui::Color32::from_rgb(69, 184, 218),
                GraphEdgeKind::Peer if self.show_all_edges => {
                    egui::Color32::from_rgba_unmultiplied(236, 190, 74, 138)
                }
                GraphEdgeKind::Peer => egui::Color32::from_rgb(236, 190, 74),
            };
            let stroke = egui::Stroke::new(1.75, color);
            if matches!(edge.kind, GraphEdgeKind::Peer) {
                ui.painter()
                    .extend(egui::Shape::dashed_line(&points, stroke, 8.0, 5.0));
            } else {
                ui.painter().add(egui::Shape::line(points.clone(), stroke));
            }
            if let Some(head) = arrow_head(&points, 8.0) {
                ui.painter().add(egui::Shape::convex_polygon(
                    head.to_vec(),
                    color,
                    egui::Stroke::NONE,
                ));
            }
            if matches!(edge.kind, GraphEdgeKind::Peer) {
                let reversed: Vec<_> = points.iter().rev().copied().collect();
                if let Some(head) = arrow_head(&reversed, 8.0) {
                    ui.painter().add(egui::Shape::convex_polygon(
                        head.to_vec(),
                        color,
                        egui::Stroke::NONE,
                    ));
                }
            }
        }
    }

    fn render_read_only_scaffold(&self, ui: &mut egui::Ui, layout: &egui_graph::Layout) {
        let mut all_x = Vec::new();
        let mut agent_y = Vec::new();
        let mut contributor_y = Vec::new();
        if let Some(position) = layout.get(&primary_node_id()) {
            all_x.push(position.x);
        }
        for (index, member) in self.draft.members.iter().enumerate() {
            let Some(position) = layout.get(&NodeId::from_u64(self.member_uids[index])) else {
                continue;
            };
            all_x.push(position.x);
            if self.is_agent(&member.id) {
                agent_y.push(position.y);
            } else {
                contributor_y.push(position.y);
            }
        }
        if all_x.is_empty() {
            return;
        }
        let left = all_x.iter().copied().fold(f32::INFINITY, f32::min) - 54.0;
        let right = all_x.iter().copied().fold(f32::NEG_INFINITY, f32::max) + 250.0;
        let painter = ui.painter();
        let color = egui::Color32::from_rgba_unmultiplied(126, 144, 164, 48);
        let label_color = egui::Color32::from_rgba_unmultiplied(159, 174, 191, 105);
        for (label, values) in [
            ("CONTEXT-BEARING PEERS", &agent_y),
            ("STATELESS SPECIALISTS", &contributor_y),
        ] {
            if values.is_empty() {
                continue;
            }
            let y = values.iter().sum::<f32>() / values.len() as f32 - 34.0;
            painter.line_segment(
                [egui::pos2(left, y), egui::pos2(right, y)],
                egui::Stroke::new(0.75, color),
            );
            painter.text(
                egui::pos2(left, y - 7.0),
                egui::Align2::LEFT_BOTTOM,
                label,
                egui::FontId::monospace(9.0),
                label_color,
            );
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
        let target_id = self.draft.members[target].id.clone();
        let source_id = if from == primary_node_id() {
            self.draft.primary_id.clone()
        } else if let Some(source) = self.member_index_for_node(from) {
            self.draft.members[source].id.clone()
        } else {
            return;
        };
        if source_id == target_id {
            self.status = "An agent cannot connect to itself.".to_owned();
            return;
        }
        if !self.is_agent(&source_id) {
            self.status = "Only the primary or a peer can own an outgoing edge.".to_owned();
            return;
        }
        match self.connection_kind {
            ConnectionKind::Call => {
                if self.is_agent(&target_id) {
                    self.status =
                        "A specialist call must end at a stateless specialist, not a peer."
                            .to_owned();
                    return;
                }
                let edge = CallEdge {
                    agent_id: source_id,
                    specialist_id: target_id,
                };
                if !self.draft.call_edges.iter().any(|candidate| {
                    candidate.agent_id == edge.agent_id
                        && candidate.specialist_id == edge.specialist_id
                }) {
                    self.draft.call_edges.push(edge);
                }
                self.status = "Stateless call edge added.".to_owned();
            }
            ConnectionKind::Peer => {
                if !self.is_agent(&target_id) {
                    let already_called = self
                        .draft
                        .call_edges
                        .iter()
                        .any(|edge| edge.specialist_id == target_id);
                    if already_called {
                        self.status =
                            "Remove specialist-call edges before changing this role to a peer."
                                .to_owned();
                        return;
                    }
                    self.draft.peer_ids.push(target_id.clone());
                }
                let edge = PeerEdge {
                    first_agent_id: source_id,
                    second_agent_id: target_id,
                };
                if !self.draft.peer_edges.iter().any(|candidate| {
                    (candidate.first_agent_id == edge.first_agent_id
                        && candidate.second_agent_id == edge.second_agent_id)
                        || (candidate.first_agent_id == edge.second_agent_id
                            && candidate.second_agent_id == edge.first_agent_id)
                }) {
                    self.draft.peer_edges.push(edge);
                }
                self.status =
                    "Peer authority edge added (consultation and handoff in both directions)."
                        .to_owned();
            }
        }
        self.prune_invalid_role_edges();
        self.layout_frames = 3;
    }

    fn remove_graph_edge(&mut self, edge: GraphEdge) {
        match edge.kind {
            GraphEdgeKind::Call => {
                self.draft.call_edges.retain(|item| {
                    format!("call:{}:{}", item.agent_id, item.specialist_id) != edge.key
                });
            }
            GraphEdgeKind::Peer => {
                self.draft.peer_edges.retain(|item| {
                    format!("peer:{}:{}", item.first_agent_id, item.second_agent_id) != edge.key
                });
            }
        }
        self.selected_edges.remove(&edge.key);
        self.layout_frames = 3;
    }

    fn prune_invalid_role_edges(&mut self) {
        let members: HashSet<String> = self
            .draft
            .members
            .iter()
            .map(|member| member.id.clone())
            .collect();
        let mut agents: HashSet<String> = self.draft.peer_ids.iter().cloned().collect();
        agents.insert(self.draft.primary_id.clone());
        self.draft.call_edges.retain(|edge| {
            edge.agent_id != edge.specialist_id
                && agents.contains(&edge.agent_id)
                && members.contains(&edge.specialist_id)
                && !agents.contains(&edge.specialist_id)
        });
        self.draft.peer_edges.retain(|edge| {
            edge.first_agent_id != edge.second_agent_id
                && agents.contains(&edge.first_agent_id)
                && agents.contains(&edge.second_agent_id)
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
            .retain(|edge| edge.agent_id != id && edge.specialist_id != id);
        self.draft
            .peer_edges
            .retain(|edge| edge.first_agent_id != id && edge.second_agent_id != id);
        self.draft.peer_ids.retain(|candidate| candidate != &id);
        self.selection = Selection::Primary;
        self.layout_frames = 3;
    }

    fn member_index_for_node(&self, id: NodeId) -> Option<usize> {
        self.member_uids.iter().position(|uid| *uid == id.0)
    }

    fn node_for_agent(&self, id: &str) -> Option<NodeId> {
        if id == self.draft.primary_id {
            return Some(primary_node_id());
        }
        let index = self
            .draft
            .members
            .iter()
            .position(|member| member.id == id && self.is_agent(&member.id))?;
        Some(NodeId::from_u64(self.member_uids[index]))
    }

    fn graph_edges(&self) -> Vec<GraphEdge> {
        let mut result = Vec::new();
        for call in &self.draft.call_edges {
            let Some(source) = self.node_for_agent(&call.agent_id) else {
                continue;
            };
            let Some(target) = self
                .draft
                .members
                .iter()
                .position(|member| member.id == call.specialist_id)
            else {
                continue;
            };
            result.push(GraphEdge {
                key: format!("call:{}:{}", call.agent_id, call.specialist_id),
                from: source,
                to: NodeId::from_u64(self.member_uids[target]),
                kind: GraphEdgeKind::Call,
            });
        }
        for peer in &self.draft.peer_edges {
            let Some(source) = self.node_for_agent(&peer.first_agent_id) else {
                continue;
            };
            let Some(target) = self.node_for_agent(&peer.second_agent_id) else {
                continue;
            };
            result.push(GraphEdge {
                key: format!("peer:{}:{}", peer.first_agent_id, peer.second_agent_id),
                from: source,
                to: target,
                kind: GraphEdgeKind::Peer,
            });
        }
        result
    }

    fn visible_graph_edges(&self) -> Vec<GraphEdge> {
        let edges = self.graph_edges();
        if !self.read_only() || self.show_all_edges {
            return edges;
        }
        let selected = match self.selection {
            Selection::Primary => primary_node_id(),
            Selection::Member(uid) => NodeId::from_u64(uid),
        };
        edges
            .into_iter()
            .filter(|edge| edge.from == selected || edge.to == selected)
            .collect()
    }

    fn node_in_focus(&self, node: NodeId) -> bool {
        if !self.read_only() || self.show_all_edges {
            return true;
        }
        let selected = match self.selection {
            Selection::Primary => primary_node_id(),
            Selection::Member(uid) => NodeId::from_u64(uid),
        };
        if node == selected {
            return true;
        }
        self.visible_graph_edges()
            .iter()
            .any(|edge| edge.from == node || edge.to == node)
    }

    fn relayout(&mut self, ctx: &egui::Context) {
        let sizes = egui_graph::with_graph_memory(ctx, team_graph_id(), |memory| {
            memory.node_sizes().clone()
        });
        let primary_size = sizes
            .get(&primary_node_id())
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
            .map(|edge| edge.specialist_id.as_str())
            .collect();
        let key_for_agent = |id: &str| {
            if id == self.draft.primary_id {
                Some(primary_node_id().value())
            } else {
                Some(self.member_uids[*member_index.get(id)?])
            }
        };
        let team = layout_engine::Team {
            primary_key: primary_node_id().value(),
            primary_size: layout_engine::Size {
                width: primary_size.x,
                height: primary_size.y,
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
                        agent: self.is_agent(&member.id),
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
                        from: key_for_agent(edge.agent_id.as_str())?,
                        to: self.member_uids[*member_index.get(edge.specialist_id.as_str())?],
                        relation: layout_engine::EdgeRelation::Call,
                    })
                })
                .chain(self.draft.peer_edges.iter().filter_map(|edge| {
                    Some(layout_engine::CallEdge {
                        from: key_for_agent(edge.first_agent_id.as_str())?,
                        to: key_for_agent(edge.second_agent_id.as_str())?,
                        relation: layout_engine::EdgeRelation::Peer,
                    })
                }))
                .collect(),
        };
        let layout = layout_engine::layout_team(&team);
        self.view.layout = layout
            .positions
            .into_iter()
            .map(|(key, point)| (NodeId::from_u64(key), egui::pos2(point.x, point.y)))
            .collect();
        if self.read_only() {
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
        let primary_size = sizes
            .get(&primary_node_id())
            .copied()
            .unwrap_or_else(|| egui::vec2(180.0, 72.0));
        let primary = self
            .view
            .layout
            .get(&primary_node_id())
            .copied()
            .map(|position| {
                (
                    primary_node_id(),
                    position,
                    LayoutNode::new(primary_size)
                        .socket_padding(socket_padding)
                        .outputs(
                            ports
                                .outputs
                                .get(&primary_node_id())
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
            .filter_map(|(index, _member)| {
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
                        .outputs(ports.outputs.get(&id).copied().unwrap_or_default().max(1)),
                ))
            });
        let edges = self
            .graph_edges()
            .into_iter()
            .filter_map(|edge| ports.endpoints.get(&edge.key).copied());
        self.routes = route_edges(
            primary.into_iter().chain(members),
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
        if let Some(position) = positions.get(&primary_node_id()).copied() {
            let size = sizes
                .get(&primary_node_id())
                .copied()
                .unwrap_or_else(|| egui::vec2(180.0, 72.0));
            centers.insert(primary_node_id(), position + size * 0.5);
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

    fn publish(&mut self) {
        match host_exchange(self.handle, "team.publish", Some(&self.draft)) {
            Ok(response) => {
                self.diagnostics = response.diagnostics;
                if let Some(published) = response.published {
                    let id = published.id.clone();
                    let revision = published.revision;
                    self.status = format!(
                        "Published {}@{} ({})",
                        published.id,
                        published.revision,
                        &published.digest[..published.digest.len().min(8)]
                    );
                    self.open_team_view(TeamView::Inspect, Some(&id), Some(revision));
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
    Call,
    Peer,
}

fn primary_node_id() -> NodeId {
    NodeId::from_u64(1)
}

fn team_graph_id() -> egui::Id {
    egui_graph::id("ALT Team Builder")
}

fn gateway_label(gateways: &[GatewayDescriptor], id: &str) -> String {
    gateways
        .iter()
        .find(|gateway| gateway.id == id)
        .map(|gateway| format!("{} · {}", gateway.name, gateway.id))
        .unwrap_or_else(|| id.to_owned())
}

fn read_only_model(ui: &mut egui::Ui, model: &ModelChoice) {
    ui.strong("Exact model");
    model_badge(ui, model);
    ui.horizontal_wrapped(|ui| {
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
            ui.weak(&selected.route);
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
                {
                    continue;
                }
                let active = selected.id == item.id && selected.route == item.route;
                let label = format!("{}  ·  {}", item.id, item.route);
                if ui.selectable_label(active, label).clicked() {
                    *selected = ModelChoice {
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
    ui.weak("Stored verbatim. The Primary and this model receive this exact text.");
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
    inspector_open: bool,
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
            inspector_open: false,
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
        self.show(root);
    }

    fn show(&mut self, root: &mut egui::Ui) {
        let ctx = root.ctx().clone();
        if ctx.input(|input| input.key_pressed(egui::Key::Escape)) {
            self.inspector_open = false;
        }
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
                if !self.inspector_open && ui.button("Details").clicked() {
                    self.inspector_open = true;
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
        egui::CentralPanel::default()
            .frame(egui::Frame::default().fill(egui::Color32::from_rgb(13, 15, 18)))
            .show_inside(root, |ui| self.thinking_graph(ui));
        if self.inspector_open {
            let screen = ctx.content_rect();
            let width = (screen.width() * 0.30).clamp(340.0, 500.0);
            let height = (screen.height() * 0.68).clamp(340.0, 720.0);
            let mut open = true;
            egui::Window::new("Execution provenance")
                .id(egui::Id::new("thinking-inspector"))
                .open(&mut open)
                .default_pos(egui::pos2(
                    (screen.right() - width - 18.0).max(screen.left() + 18.0),
                    screen.top() + 58.0,
                ))
                .default_size(egui::vec2(width, height))
                .min_width(300.0)
                .max_width(540.0)
                .max_height(screen.height() - 88.0)
                .resizable(true)
                .collapsible(false)
                .show(&ctx, |ui| {
                    egui::ScrollArea::vertical()
                        .id_salt("thinking-inspector-scroll")
                        .auto_shrink([false, false])
                        .show(ui, |ui| self.thinking_inspector(ui));
                });
            self.inspector_open = open;
        }
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
        let animated_edges = animated_thinking_edges(
            &self.state.active,
            self.selected.as_deref(),
            MAX_ANIMATED_LIVE_EDGES,
        );
        let mut view = std::mem::take(&mut self.view);
        let response = egui_graph::Graph::from_id(thinking_graph_id())
            .center_view(center_view)
            // Live provenance is read, not spatially authored. A stationary
            // dot field only adds non-semantic contrast behind active paths.
            .dot_grid(false)
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
                                let accent = thinking_node_accent(node);
                                let selected = node_ctx.interaction().selected;
                                let active = node.status == "running";
                                let mut frame = egui_graph::node::default_frame(
                                    node_ctx.style(),
                                    node_ctx.interaction(),
                                );
                                frame.fill =
                                    accent.gamma_multiply(if active { 0.20 } else { 0.11 });
                                if !selected {
                                    frame.stroke = egui::Stroke::new(
                                        if active { 2.0 } else { 1.2 },
                                        accent.gamma_multiply(if active { 1.0 } else { 0.72 }),
                                    );
                                }
                                node_ctx.framed_with(frame, |ui, _| {
                                    ui.visuals_mut().disabled_alpha = 1.0;
                                    ui.horizontal(|ui| {
                                        ui.colored_label(status_color(&node.status), "●");
                                        ui.label(
                                            egui::RichText::new(thinking_node_label(node))
                                                .strong()
                                                .color(accent),
                                        );
                                    });
                                    ui.label(
                                        egui::RichText::new(format!(
                                            "{} · {}",
                                            thinking_node_role(node),
                                            node.status
                                        ))
                                        .small()
                                        .color(accent.gamma_multiply(0.72)),
                                    );
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
                        .filter(|edge| !is_allowed_edge(&edge.kind))
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
                        if matches!(edge.kind.as_str(), "peer" | "peer-result") {
                            ui.painter()
                                .extend(egui::Shape::dashed_line(points, stroke, 8.0, 5.0));
                        } else {
                            ui.painter().add(egui::Shape::line(points.to_vec(), stroke));
                        }
                        if let Some(head) = arrow_head(points, 8.0) {
                            ui.painter().add(egui::Shape::convex_polygon(
                                head.to_vec(),
                                color,
                                egui::Stroke::NONE,
                            ));
                        }
                        if edge.direction == "bidirectional" {
                            let reversed: Vec<_> = points.iter().rev().copied().collect();
                            if let Some(head) = arrow_head(&reversed, 8.0) {
                                ui.painter().add(egui::Shape::convex_polygon(
                                    head.to_vec(),
                                    color,
                                    egui::Stroke::NONE,
                                ));
                            }
                        }
                        if edge.active > 0 && animated_edges.contains(&edge.id) {
                            let offset = (thinking_node_id(&edge.id).value() % 997) as f64 / 997.0;
                            let path_length = points
                                .windows(2)
                                .map(|segment| segment[0].distance(segment[1]))
                                .sum::<f32>()
                                .max(1.0);
                            let viewport = ui.clip_rect().size().min_elem().max(1.0);
                            let cycles_per_second =
                                (viewport * LIVE_MARKER_VIEWPORTS_PER_SECOND) / path_length;
                            let phase = (ui.input(|input| input.time) * cycles_per_second as f64
                                + offset)
                                .fract() as f32;
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
            self.inspector_open = self.selected.is_some();
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
        ui.heading(thinking_node_label(&node));
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
        let Some(primary) = self
            .state
            .active
            .nodes
            .values()
            .find(|node| {
                node.kind == "agent"
                    && node
                        .metadata
                        .get("primary")
                        .is_some_and(|value| value == "true")
            })
            .or_else(|| {
                self.state
                    .active
                    .nodes
                    .values()
                    .find(|node| node.kind == "agent")
            })
        else {
            self.view.layout.clear();
            if self.state.active.nodes.contains_key("user") {
                self.view
                    .layout
                    .insert(thinking_node_id("user"), egui::pos2(0.0, 0.0));
            }
            self.reroute(ctx);
            return;
        };
        let primary_size = size_for(&primary.id, egui::vec2(210.0, 82.0));
        let members: Vec<_> = self
            .state
            .active
            .nodes
            .values()
            .filter(|node| {
                matches!(node.kind.as_str(), "agent" | "specialist") && node.id != primary.id
            })
            .collect();
        let participant_keys: BTreeMap<_, _> = self
            .state
            .active
            .nodes
            .values()
            .filter(|node| matches!(node.kind.as_str(), "agent" | "specialist"))
            .map(|node| (node.id.as_str(), thinking_node_id(&node.id).value()))
            .collect();
        let callable_members: HashSet<_> = self
            .state
            .active
            .edges
            .values()
            .filter(|edge| {
                is_allowed_edge(&edge.kind)
                    && edge.from.starts_with("member:")
                    && edge.to.starts_with("member:")
            })
            .map(|edge| edge.to.as_str())
            .collect();
        let team = layout_engine::Team {
            primary_key: thinking_node_id(&primary.id).value(),
            primary_size: layout_engine::Size {
                width: primary_size.x,
                height: primary_size.y,
            },
            members: members
                .iter()
                .map(|node| {
                    let size = size_for(&node.id, egui::vec2(210.0, 82.0));
                    layout_engine::Member {
                        key: participant_keys[node.id.as_str()],
                        size: layout_engine::Size {
                            width: size.x,
                            height: size.y,
                        },
                        roles: layout_engine::Roles {
                            agent: node.kind == "agent",
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
                    is_allowed_edge(&edge.kind)
                        && edge.from.starts_with("member:")
                        && edge.to.starts_with("member:")
                })
                .filter_map(|edge| {
                    let &from = participant_keys.get(edge.from.as_str())?;
                    let &to = participant_keys.get(edge.to.as_str())?;
                    Some(layout_engine::CallEdge {
                        from,
                        to,
                        relation: if edge.kind == "allowed-peer" {
                            layout_engine::EdgeRelation::Peer
                        } else {
                            layout_engine::EdgeRelation::Call
                        },
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
            .filter(|node| is_thinking_tool_node(node))
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
            "Bubblewrap namespaces, no_new_privs, and Landlock confine terminal commands. Web research uses the separately selected research connection.",
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
        ui.strong("Web research");
        if !runtime.research_provider.is_empty() {
            ui.colored_label(
                egui::Color32::from_rgb(82, 211, 158),
                format!("{} selected", runtime.research_provider),
            );
        } else if runtime.exa_configured || runtime.linkup_configured {
            ui.colored_label(egui::Color32::from_rgb(236, 190, 74), "selection required")
                .on_hover_text("Choose Exa or Linkup with /research.");
        } else {
            ui.colored_label(egui::Color32::from_rgb(236, 190, 74), "credential required")
                .on_hover_text("Run `alt auth set exa` or `alt auth set linkup`.");
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
        .filter(|edge| is_allowed_edge(&edge.kind) || edge.kind == "tool")
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
        .filter(|edge| !is_allowed_edge(&edge.kind))
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
        "agent" | "specialist" => egui::vec2(210.0, 82.0),
        "tool" => egui::vec2(190.0, 76.0),
        "tool-discovery" => egui::vec2(150.0, 64.0),
        _ => egui::vec2(180.0, 72.0),
    }
}

fn is_thinking_tool_node(node: &ThinkingNode) -> bool {
    matches!(node.kind.as_str(), "tool" | "tool-discovery")
}

fn thinking_node_label(node: &ThinkingNode) -> String {
    if node.kind == "tool-discovery" {
        return "tool discovery".to_owned();
    }
    if node.kind != "tool"
        || !matches!(
            node.label.as_str(),
            "web_search" | "web_fetch" | "web_answer"
        )
    {
        return node.label.clone();
    }
    let provider = node
        .metadata
        .get("provider")
        .map(String::as_str)
        .filter(|value| !value.trim().is_empty())
        .unwrap_or_default();
    let provider = match provider.trim().to_ascii_lowercase().as_str() {
        "exa" => "Exa",
        "linkup" => "Linkup",
        _ => return node.label.clone(),
    };
    format!("{} · {provider}", node.label)
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
        .filter(|edge| !is_allowed_edge(&edge.kind))
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

fn thinking_node_role(node: &ThinkingNode) -> &'static str {
    match node.kind.as_str() {
        "user" => "USER",
        "tool" => "TOOL",
        "tool-discovery" => "DISCOVERY",
        "agent"
            if node
                .metadata
                .get("primary")
                .is_some_and(|value| value == "true") =>
        {
            "PRIMARY"
        }
        "agent" => "PEER",
        "specialist" => "SPECIALIST",
        _ => "EVENT",
    }
}

fn thinking_node_accent(node: &ThinkingNode) -> egui::Color32 {
    match thinking_node_role(node) {
        "PRIMARY" => egui::Color32::from_rgb(178, 132, 255),
        "PEER" => egui::Color32::from_rgb(99, 164, 255),
        "SPECIALIST" => egui::Color32::from_rgb(82, 211, 158),
        "USER" => egui::Color32::from_rgb(239, 143, 100),
        "TOOL" => egui::Color32::from_rgb(158, 166, 178),
        "DISCOVERY" => egui::Color32::from_rgb(116, 137, 159),
        _ => egui::Color32::from_rgb(126, 144, 164),
    }
}

fn thinking_edge_color(kind: &str) -> egui::Color32 {
    match kind {
        "result" | "failure" | "tool-result" | "answer" => egui::Color32::from_rgb(82, 211, 158),
        "enables" => egui::Color32::from_rgb(239, 190, 75),
        "handoff" => egui::Color32::from_rgb(194, 128, 255),
        "delegation" | "request" => egui::Color32::from_rgb(76, 185, 224),
        "peer" | "peer-result" => egui::Color32::from_rgb(236, 190, 74),
        "tool" => egui::Color32::from_rgb(152, 162, 175),
        "tool-discovery" => egui::Color32::from_rgb(116, 137, 159),
        "allowed" => egui::Color32::from_rgb(54, 61, 69),
        _ => egui::Color32::from_rgb(116, 137, 159),
    }
}

const MAX_ANIMATED_LIVE_EDGES: usize = 4;
const LIVE_MARKER_VIEWPORTS_PER_SECOND: f32 = 0.04;

/// Select a bounded set of motion traces without hiding any active lane.
///
/// Human tracking capacity is limited even when the underlying orchestration
/// is not. Every active edge remains bright and thick; only this stable,
/// selection-aware subset receives a moving marker. Incident edges of the
/// inspected node win, followed by newer work and then durable edge identity.
fn animated_thinking_edges(
    turn: &ThinkingTurn,
    selected: Option<&str>,
    limit: usize,
) -> HashSet<String> {
    let mut candidates: Vec<_> = turn
        .edges
        .values()
        .filter(|edge| edge.active > 0 && !is_allowed_edge(&edge.kind))
        .collect();
    candidates.sort_by(|left, right| {
        let left_incident = selected.is_some_and(|id| left.from == id || left.to == id);
        let right_incident = selected.is_some_and(|id| right.from == id || right.to == id);
        right_incident
            .cmp(&left_incident)
            .then_with(|| right.started_at_ms.cmp(&left.started_at_ms))
            .then_with(|| left.id.cmp(&right.id))
    });
    candidates
        .into_iter()
        .take(limit)
        .map(|edge| edge.id.clone())
        .collect()
}

fn is_allowed_edge(kind: &str) -> bool {
    kind == "allowed" || kind == "allowed-peer"
}

/// Route a read-only edge between the nearest points on two node rectangles.
///
/// The first pass is the straight, shortest visible segment. If it intersects
/// another node, the segment is deterministically replaced by the shortest
/// valid walk around one side of that node's expanded rectangle. Repeating the
/// operation handles multiple blockers without relying on vertical sockets,
/// which would impose a false flow axis on radial layouts.
#[cfg(test)]
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

/// Round polyline corners only when the sampled curve retains deterministic
/// clearance from every unrelated node. Quadratic Bézier samples remain a
/// polyline for exact arrow placement, dashes, hit testing, and screenshots.
#[cfg(test)]
fn rounded_obstacle_safe_route(
    points: &[egui::Pos2],
    obstacles: &[egui::Rect],
    clearance: f32,
    preferred_radius: f32,
) -> Vec<egui::Pos2> {
    if points.len() < 3 {
        return points.to_vec();
    }
    let expanded: Vec<_> = obstacles
        .iter()
        .map(|rect| rect.expand(clearance))
        .collect();
    let mut radius = preferred_radius;
    for _ in 0..5 {
        let candidate = sample_rounded_polyline(points, radius, 6);
        let collision = candidate.windows(2).any(|segment| {
            expanded
                .iter()
                .any(|rect| segment_rect_entry(segment[0], segment[1], *rect).is_some())
        });
        if !collision {
            return candidate;
        }
        radius *= 0.5;
    }
    points.to_vec()
}

#[cfg(test)]
fn sample_rounded_polyline(
    points: &[egui::Pos2],
    radius: f32,
    samples_per_corner: usize,
) -> Vec<egui::Pos2> {
    let mut result = Vec::with_capacity(points.len() * (samples_per_corner + 1));
    result.push(points[0]);
    for index in 1..points.len() - 1 {
        let previous = points[index - 1];
        let corner = points[index];
        let next = points[index + 1];
        let incoming = corner - previous;
        let outgoing = next - corner;
        let incoming_length = incoming.length();
        let outgoing_length = outgoing.length();
        if incoming_length <= f32::EPSILON || outgoing_length <= f32::EPSILON {
            continue;
        }
        let local_radius = radius
            .min(incoming_length * 0.36)
            .min(outgoing_length * 0.36);
        let entry = corner - incoming / incoming_length * local_radius;
        let exit = corner + outgoing / outgoing_length * local_radius;
        if result.last().is_none_or(|point| *point != entry) {
            result.push(entry);
        }
        for sample in 1..=samples_per_corner {
            let t = sample as f32 / samples_per_corner as f32;
            let one_minus_t = 1.0 - t;
            let point = egui::pos2(
                one_minus_t * one_minus_t * entry.x
                    + 2.0 * one_minus_t * t * corner.x
                    + t * t * exit.x,
                one_minus_t * one_minus_t * entry.y
                    + 2.0 * one_minus_t * t * corner.y
                    + t * t * exit.y,
            );
            result.push(point);
        }
    }
    result.push(*points.last().expect("non-empty rounded polyline"));
    result.dedup();
    result
}

#[cfg(test)]
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

#[cfg(test)]
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
        alt_native_gui_wake, animated_thinking_edges, arrow_head, assign_edge_ports,
        metadata_height_budget, pixel_perfect_scene_zoom_range, point_along_polyline,
        rect_ray_anchor, reflow_definition_for_display, rounded_obstacle_safe_route,
        route_between_rects, sample_polyline, segment_rect_entry, tangent_annotation,
        thinking_fingerprint, thinking_node_id, thinking_node_label, thinking_routes,
        thinking_routing_fingerprint, CallEdge, ConnectionKind, DraftAssignment, DraftMember,
        GatewayDescriptor, LiveRegistration, ModelChoice, PeerEdge, PortEdge, RuntimeCapabilities,
        Selection, TeamApp, TeamDraft, TeamView, ThinkingApp, ThinkingEdge, ThinkingNode,
        ThinkingProjection, ThinkingTurn, ThinkingTurnSummary,
    };
    use egui_graph::NodeId;
    use egui_kittest::kittest::Queryable;
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
    fn research_tool_labels_use_only_durable_provider_metadata() {
        let mut node = ThinkingNode {
            id: "tool:search".to_owned(),
            kind: "tool".to_owned(),
            label: "web_search".to_owned(),
            status: "running".to_owned(),
            actor: String::new(),
            metadata: BTreeMap::new(),
        };
        assert_eq!(thinking_node_label(&node), "web_search");
        node.metadata
            .insert("provider".to_owned(), "Linkup".to_owned());
        assert_eq!(thinking_node_label(&node), "web_search · Linkup");
        node.kind = "tool-discovery".to_owned();
        node.label = "tool_search".to_owned();
        assert_eq!(thinking_node_label(&node), "tool discovery");
    }

    fn dense_peer_draft() -> TeamDraft {
        let model = |id: &str| ModelChoice {
            route: "go".to_owned(),
            id: id.to_owned(),
        };
        let peer = |id: &str, role: &str| DraftMember {
            id: id.to_owned(),
            model: model(&format!("{id}-model")),
            definition: format!(
                "Own {role}; answer, consult, or accept leadership when appropriate."
            ),
        };
        let specialist = |id: &str, role: &str| DraftMember {
            id: id.to_owned(),
            model: model(&format!("{id}-model")),
            definition: format!("Return a stateless, caller-scoped {role} finding."),
        };
        TeamDraft {
            id: "dense-peer-math".to_owned(),
            name: "Dense Peer Geometry".to_owned(),
            base_revision: 1,
            gateway: "opencode".to_owned(),
            primary_id: "primary".to_owned(),
            primary: DraftAssignment {
                model: model("primary-model"),
                definition:
                    "Receive every user turn and own it unless leadership belongs with a peer."
                        .to_owned(),
            },
            members: vec![
                peer("systems-peer", "system design"),
                peer("research-peer", "evidence-led research"),
                peer("product-peer", "the product outcome"),
                peer("operations-peer", "operational reliability"),
                specialist("evidence", "evidence-checking"),
                specialist("runtime", "runtime-analysis"),
                specialist("human", "human-factors"),
                specialist("adversarial", "adversarial-review"),
                specialist("recovery", "recovery-analysis"),
                specialist("economics", "cost-analysis"),
                specialist("synthesis", "cross-finding synthesis"),
                specialist("domain", "domain-expertise"),
            ],
            peer_ids: [
                "systems-peer",
                "research-peer",
                "product-peer",
                "operations-peer",
            ]
            .into_iter()
            .map(str::to_owned)
            .collect(),
            call_edges: [
                ("systems-peer", "runtime"),
                ("systems-peer", "recovery"),
                ("systems-peer", "domain"),
                ("systems-peer", "evidence"),
                ("research-peer", "evidence"),
                ("research-peer", "economics"),
                ("research-peer", "synthesis"),
                ("research-peer", "human"),
                ("product-peer", "human"),
                ("product-peer", "economics"),
                ("product-peer", "runtime"),
                ("product-peer", "synthesis"),
                ("operations-peer", "recovery"),
                ("operations-peer", "adversarial"),
                ("operations-peer", "runtime"),
                ("operations-peer", "domain"),
                ("operations-peer", "human"),
                ("operations-peer", "synthesis"),
            ]
            .into_iter()
            .map(|(agent_id, specialist_id)| CallEdge {
                agent_id: agent_id.to_owned(),
                specialist_id: specialist_id.to_owned(),
            })
            .collect(),
            peer_edges: [
                ("primary", "systems-peer"),
                ("primary", "research-peer"),
                ("primary", "product-peer"),
                ("primary", "operations-peer"),
                ("systems-peer", "research-peer"),
                ("systems-peer", "product-peer"),
                ("research-peer", "operations-peer"),
            ]
            .into_iter()
            .map(|(first_agent_id, second_agent_id)| PeerEdge {
                first_agent_id: first_agent_id.to_owned(),
                second_agent_id: second_agent_id.to_owned(),
            })
            .collect(),
        }
    }

    fn shipped_team_draft(engineering: bool) -> TeamDraft {
        let model = |id: &str| ModelChoice {
            route: "zen".to_owned(),
            id: id.to_owned(),
        };
        let (id, name, primary_id, primary_model, peer_id, peer_model, specialist_id) =
            if engineering {
                (
                    "engineering",
                    "Free Engineering Team",
                    "engineering",
                    "hy3-free",
                    "coding",
                    "deepseek-v4-flash-free",
                    "visual-inspector",
                )
            } else {
                (
                    "free",
                    "Free Team",
                    "generalist",
                    "hy3-free",
                    "investigator",
                    "nemotron-3-ultra-free",
                    "visual-observer",
                )
            };
        TeamDraft {
            id: id.to_owned(),
            name: name.to_owned(),
            base_revision: 4,
            gateway: "opencode".to_owned(),
            primary_id: primary_id.to_owned(),
            primary: DraftAssignment {
                model: model(primary_model),
                definition: "Receive every user turn and retain leadership unless the requested final result belongs with the peer.".to_owned(),
            },
            members: vec![
                DraftMember {
                    id: peer_id.to_owned(),
                    model: model(peer_model),
                    definition: if engineering {
                        "Own well-scoped code work when leadership is handed here; otherwise contribute concrete implementation findings.".to_owned()
                    } else {
                        "Own evidence-intensive investigations and long-context verification when leadership is handed here.".to_owned()
                    },
                },
                DraftMember {
                    id: specialist_id.to_owned(),
                    model: model("mimo-v2.5-free"),
                    definition: "Begin from a clean slate and inspect only media explicitly attached to this invocation.".to_owned(),
                },
            ],
            peer_ids: vec![peer_id.to_owned()],
            call_edges: vec![
                CallEdge {
                    agent_id: primary_id.to_owned(),
                    specialist_id: specialist_id.to_owned(),
                },
                CallEdge {
                    agent_id: peer_id.to_owned(),
                    specialist_id: specialist_id.to_owned(),
                },
            ],
            peer_edges: vec![PeerEdge {
                first_agent_id: primary_id.to_owned(),
                second_agent_id: peer_id.to_owned(),
            }],
        }
    }

    fn shipped_working_projection(engineering: bool) -> ThinkingProjection {
        let node =
            |id: &str, kind: &str, label: &str, status: &str, primary: bool| -> ThinkingNode {
                ThinkingNode {
                    id: id.to_owned(),
                    kind: kind.to_owned(),
                    label: label.to_owned(),
                    status: status.to_owned(),
                    actor: String::new(),
                    metadata: if primary {
                        BTreeMap::from([("primary".to_owned(), "true".to_owned())])
                    } else {
                        BTreeMap::new()
                    },
                }
            };
        let edge = |id: &str,
                    from: &str,
                    to: &str,
                    kind: &str,
                    direction: &str,
                    status: &str,
                    active: usize|
         -> ThinkingEdge {
            ThinkingEdge {
                id: id.to_owned(),
                from: from.to_owned(),
                to: to.to_owned(),
                kind: kind.to_owned(),
                direction: direction.to_owned(),
                status: status.to_owned(),
                count: 1,
                active,
                started_at_ms: 0,
                metadata: BTreeMap::new(),
            }
        };
        let (task, nodes, edges) = if engineering {
            (
                "Implement a responsive view from the attached mockup and verify it.",
                BTreeMap::from([
                    (
                        "user".to_owned(),
                        node("user", "user", "You", "completed", false),
                    ),
                    (
                        "member:engineering".to_owned(),
                        node("member:engineering", "agent", "engineering", "idle", true),
                    ),
                    (
                        "member:coding".to_owned(),
                        node("member:coding", "agent", "coding", "running", false),
                    ),
                    (
                        "member:visual-inspector".to_owned(),
                        node(
                            "member:visual-inspector",
                            "specialist",
                            "visual-inspector",
                            "running",
                            false,
                        ),
                    ),
                    (
                        "tool:terminal".to_owned(),
                        node("tool:terminal", "tool", "terminal", "running", false),
                    ),
                ]),
                BTreeMap::from([
                    (
                        "allowed:user:engineering".to_owned(),
                        edge(
                            "allowed:user:engineering",
                            "user",
                            "member:engineering",
                            "allowed",
                            "outward",
                            "idle",
                            0,
                        ),
                    ),
                    (
                        "allowed-peer:coding:engineering".to_owned(),
                        edge(
                            "allowed-peer:coding:engineering",
                            "member:engineering",
                            "member:coding",
                            "allowed-peer",
                            "bidirectional",
                            "idle",
                            0,
                        ),
                    ),
                    (
                        "allowed:engineering:visual".to_owned(),
                        edge(
                            "allowed:engineering:visual",
                            "member:engineering",
                            "member:visual-inspector",
                            "allowed",
                            "outward",
                            "idle",
                            0,
                        ),
                    ),
                    (
                        "allowed:coding:visual".to_owned(),
                        edge(
                            "allowed:coding:visual",
                            "member:coding",
                            "member:visual-inspector",
                            "allowed",
                            "outward",
                            "idle",
                            0,
                        ),
                    ),
                    (
                        "flow:request".to_owned(),
                        edge(
                            "flow:request",
                            "user",
                            "member:engineering",
                            "request",
                            "outward",
                            "completed",
                            0,
                        ),
                    ),
                    (
                        "flow:handoff".to_owned(),
                        edge(
                            "flow:handoff",
                            "member:engineering",
                            "member:coding",
                            "handoff",
                            "outward",
                            "completed",
                            0,
                        ),
                    ),
                    (
                        "flow:delegation".to_owned(),
                        edge(
                            "flow:delegation",
                            "member:coding",
                            "member:visual-inspector",
                            "delegation",
                            "outward",
                            "running",
                            1,
                        ),
                    ),
                    (
                        "flow:tool".to_owned(),
                        edge(
                            "flow:tool",
                            "member:coding",
                            "tool:terminal",
                            "tool",
                            "outward",
                            "running",
                            1,
                        ),
                    ),
                ]),
            )
        } else {
            (
                "Assess the attached architecture diagram against current primary evidence.",
                BTreeMap::from([
                    (
                        "user".to_owned(),
                        node("user", "user", "You", "completed", false),
                    ),
                    (
                        "member:generalist".to_owned(),
                        node("member:generalist", "agent", "generalist", "running", true),
                    ),
                    (
                        "member:investigator".to_owned(),
                        node(
                            "member:investigator",
                            "agent",
                            "investigator",
                            "running",
                            false,
                        ),
                    ),
                    (
                        "member:visual-observer".to_owned(),
                        node(
                            "member:visual-observer",
                            "specialist",
                            "visual-observer",
                            "running",
                            false,
                        ),
                    ),
                    (
                        "tool:web-search".to_owned(),
                        node("tool:web-search", "tool", "web_search", "running", false),
                    ),
                ]),
                BTreeMap::from([
                    (
                        "allowed:user:generalist".to_owned(),
                        edge(
                            "allowed:user:generalist",
                            "user",
                            "member:generalist",
                            "allowed",
                            "outward",
                            "idle",
                            0,
                        ),
                    ),
                    (
                        "allowed-peer:generalist:investigator".to_owned(),
                        edge(
                            "allowed-peer:generalist:investigator",
                            "member:generalist",
                            "member:investigator",
                            "allowed-peer",
                            "bidirectional",
                            "idle",
                            0,
                        ),
                    ),
                    (
                        "allowed:generalist:visual".to_owned(),
                        edge(
                            "allowed:generalist:visual",
                            "member:generalist",
                            "member:visual-observer",
                            "allowed",
                            "outward",
                            "idle",
                            0,
                        ),
                    ),
                    (
                        "allowed:investigator:visual".to_owned(),
                        edge(
                            "allowed:investigator:visual",
                            "member:investigator",
                            "member:visual-observer",
                            "allowed",
                            "outward",
                            "idle",
                            0,
                        ),
                    ),
                    (
                        "flow:request".to_owned(),
                        edge(
                            "flow:request",
                            "user",
                            "member:generalist",
                            "request",
                            "outward",
                            "completed",
                            0,
                        ),
                    ),
                    (
                        "flow:peer".to_owned(),
                        edge(
                            "flow:peer",
                            "member:generalist",
                            "member:investigator",
                            "peer",
                            "bidirectional",
                            "running",
                            1,
                        ),
                    ),
                    (
                        "flow:delegation".to_owned(),
                        edge(
                            "flow:delegation",
                            "member:generalist",
                            "member:visual-observer",
                            "delegation",
                            "outward",
                            "running",
                            1,
                        ),
                    ),
                    (
                        "flow:tool".to_owned(),
                        edge(
                            "flow:tool",
                            "member:investigator",
                            "tool:web-search",
                            "tool",
                            "outward",
                            "running",
                            1,
                        ),
                    ),
                ]),
            )
        };
        let active = ThinkingTurn {
            id: if engineering {
                "turn:engineering"
            } else {
                "turn:free"
            }
            .to_owned(),
            ordinal: 1,
            task: task.to_owned(),
            status: "running".to_owned(),
            sequence: 12,
            nodes,
            edges,
        };
        ThinkingProjection {
            session_id: if engineering {
                "session:engineering-visual-audit"
            } else {
                "session:free-visual-audit"
            }
            .to_owned(),
            active_turn_id: active.id.clone(),
            revision: active.sequence as u64,
            turns: vec![ThinkingTurnSummary {
                id: active.id.clone(),
                ordinal: active.ordinal,
                task: active.task.clone(),
                status: active.status.clone(),
                sequence: active.sequence,
            }],
            active,
        }
    }

    fn assert_shipped_team_snapshot(engineering: bool, snapshot: &str) {
        let app = TeamApp::new(
            0,
            shipped_team_draft(engineering),
            vec![],
            vec![],
            vec![],
            vec![],
            String::new(),
            TeamView::Inspect,
            RuntimeCapabilities::default(),
        );
        let mut harness = egui_kittest::Harness::builder()
            .with_size(eframe::egui::vec2(1600.0, 1000.0))
            .wgpu()
            .build_ui_state(|ui, app| app.update(ui), app);
        harness.run();
        assert_eq!(harness.state().visible_graph_edges().len(), 3);
        harness.snapshot(snapshot);
    }

    #[test]
    fn shipped_free_team_matches_visual_snapshot() {
        assert_shipped_team_snapshot(false, "free_team_inspect");
    }

    #[test]
    fn shipped_free_engineering_team_matches_visual_snapshot() {
        assert_shipped_team_snapshot(true, "free_engineering_team_inspect");
    }

    fn assert_shipped_working_snapshot(engineering: bool, snapshot: &str) {
        let context = eframe::egui::Context::default();
        let mut app = ThinkingApp::new(
            0,
            shipped_working_projection(engineering),
            RuntimeCapabilities::default(),
            String::new(),
            &context,
        );
        app.observed_generation = 0;
        let mut harness = egui_kittest::Harness::builder()
            .with_size(eframe::egui::vec2(1600.0, 1000.0))
            .wgpu()
            .build_ui_state(|ui, app| app.show(ui), app);
        harness.run_steps(5);
        for edge in ["flow:request", "flow:delegation", "flow:tool"] {
            assert!(
                harness.state().routes.path(edge).is_some(),
                "missing routed active edge {edge}"
            );
        }
        harness.snapshot(snapshot);
    }

    #[test]
    fn shipped_free_team_has_readable_active_execution_snapshot() {
        assert_shipped_working_snapshot(false, "free_team_working");
    }

    #[test]
    fn shipped_free_engineering_team_has_readable_active_execution_snapshot() {
        assert_shipped_working_snapshot(true, "free_engineering_team_working");
    }

    #[test]
    fn dense_peer_graph_preserves_primary_ingress_and_peer_associations() {
        let app = TeamApp::new(
            0,
            dense_peer_draft(),
            vec![],
            vec![],
            vec![],
            vec![],
            String::new(),
            TeamView::Inspect,
            RuntimeCapabilities::default(),
        );
        let mut harness = egui_kittest::Harness::builder()
            .with_size(eframe::egui::vec2(1600.0, 1000.0))
            .wgpu()
            .build_ui_state(|ui, app| app.update(ui), app);
        harness.run();

        let app = harness.state();
        assert!(
            app.show_all_edges,
            "Inspect hid part of the Team by default"
        );
        assert_eq!(app.visible_graph_edges().len(), 25);
        let primary_y = app.view.layout[&super::primary_node_id()].y;
        let peer_y: Vec<_> = app.member_uids[..4]
            .iter()
            .map(|uid| app.view.layout[&NodeId::from_u64(*uid)].y)
            .collect();
        assert!(peer_y.iter().all(|y| *y > primary_y));
        assert_eq!(app.view.layout.len(), app.member_uids.len() + 1);
        harness.snapshot("dense_peer_team_inspect");
    }

    #[test]
    fn dense_peer_focus_reveals_one_overlapping_pool_without_erasing_context() {
        let mut app = TeamApp::new(
            0,
            dense_peer_draft(),
            vec![],
            vec![],
            vec![],
            vec![],
            String::new(),
            TeamView::Inspect,
            RuntimeCapabilities::default(),
        );
        app.selection = Selection::Member(100);
        app.show_all_edges = false;
        assert_eq!(app.visible_graph_edges().len(), 7);
        assert!(app.node_in_focus(NodeId::from_u64(100)));
        assert!(app.node_in_focus(NodeId::from_u64(105)));
        assert!(!app.node_in_focus(NodeId::from_u64(103)));

        let mut harness = egui_kittest::Harness::builder()
            .with_size(eframe::egui::vec2(1600.0, 1000.0))
            .wgpu()
            .build_ui_state(|ui, app| app.update(ui), app);
        harness.run();
        harness.snapshot("dense_peer_team_focus");
    }

    #[test]
    fn floating_inspector_never_swallow_the_graph_and_escape_closes_it() {
        let mut app = TeamApp::new(
            0,
            dense_peer_draft(),
            vec![],
            vec![],
            vec![],
            vec![],
            String::new(),
            TeamView::Inspect,
            RuntimeCapabilities::default(),
        );
        app.inspector_open = true;
        let mut harness = egui_kittest::Harness::builder()
            .with_size(eframe::egui::vec2(2048.0, 1152.0))
            .wgpu()
            .build_ui_state(|ui, app| app.update(ui), app);
        harness.run();
        assert!(harness.state().inspector_open);
        assert!(harness.state().graph_screen_rect.width() > 1_900.0);
        harness.snapshot("wide_team_inspector");

        harness.key_press(eframe::egui::Key::Escape);
        harness.run();
        assert!(!harness.state().inspector_open);
        assert!(harness.state().graph_screen_rect.width() > 1_900.0);
    }

    #[test]
    fn graph_ports_are_programmatically_draggable_and_targets_are_forgiving() {
        let model = |id: &str| ModelChoice {
            route: "go".to_owned(),
            id: id.to_owned(),
        };
        let draft = TeamDraft {
            id: "gesture-test".to_owned(),
            name: "Gesture Test".to_owned(),
            base_revision: 1,
            gateway: "opencode".to_owned(),
            primary_id: "primary".to_owned(),
            primary: DraftAssignment {
                model: model("primary"),
                definition: "Choose the accountable Agent.".to_owned(),
            },
            members: vec![
                DraftMember {
                    id: "agent".to_owned(),
                    model: model("agent"),
                    definition: "Own the result.".to_owned(),
                },
                DraftMember {
                    id: "contributor".to_owned(),
                    model: model("contributor"),
                    definition: "Return bounded findings.".to_owned(),
                },
                DraftMember {
                    id: "other-agent".to_owned(),
                    model: model("other-agent"),
                    definition: "Own another class of result.".to_owned(),
                },
            ],
            peer_ids: vec!["agent".to_owned(), "other-agent".to_owned()],
            call_edges: vec![],
            peer_edges: vec![],
        };
        let mut app = TeamApp::new(
            0,
            draft,
            vec![],
            vec![],
            vec![],
            vec![],
            String::new(),
            TeamView::Edit,
            RuntimeCapabilities::default(),
        );
        app.connection_kind = ConnectionKind::Call;
        let mut harness = egui_kittest::Harness::builder()
            .with_size(eframe::egui::vec2(1200.0, 820.0))
            .wgpu()
            .build_ui_state(|ui, app| app.update(ui), app);
        harness.run();

        let agent = NodeId::from_u64(harness.state().member_uids[0]);
        let contributor = NodeId::from_u64(harness.state().member_uids[1]);
        let (start_in_graph, target_in_graph) =
            egui_graph::with_graph_memory(&harness.ctx, super::team_graph_id(), |memory| {
                let (start, start_normal) = memory.node_sockets()[&agent].output(0).unwrap();
                let (target, target_normal) = memory.node_sockets()[&contributor].input(0).unwrap();
                (start + start_normal * 8.0, target + target_normal * 10.0)
            });
        let start = harness.state().graph_to_screen(start_in_graph);
        let target = harness.state().graph_to_screen(target_in_graph);
        harness.hover_at(start);
        harness.step();
        harness.drag_at(start);
        harness.step();
        assert!(harness.state().edge_in_progress.is_some());
        harness.hover_at(start.lerp(target, 0.55));
        harness.step();
        harness.snapshot("team_live_connection_drag");

        // Deliberately miss the small painted semicircle while remaining in
        // egui_graph's larger interaction radius.
        harness.hover_at(target);
        harness.drop_at(target);
        harness.run();
        assert_eq!(harness.state().draft.call_edges.len(), 1);
        assert_eq!(harness.state().draft.call_edges[0].agent_id, "agent");
        assert_eq!(
            harness.state().draft.call_edges[0].specialist_id,
            "contributor"
        );
    }

    #[test]
    fn editable_team_primary_drag_pans_empty_canvas() {
        let app = TeamApp::new(
            0,
            dense_peer_draft(),
            vec![],
            vec![],
            vec![],
            vec![],
            String::new(),
            TeamView::Edit,
            RuntimeCapabilities::default(),
        );
        let mut harness = egui_kittest::Harness::builder()
            .with_size(eframe::egui::vec2(1200.0, 820.0))
            .wgpu()
            .build_ui_state(|ui, app| app.update(ui), app);
        harness.run();

        let graph_rect = harness.state().graph_screen_rect;
        let before = harness.state().view.scene_rect.center();
        let selection_before = harness.state().selection;
        let start = graph_rect.right_bottom() - eframe::egui::vec2(30.0, 30.0);
        let end = start - eframe::egui::vec2(170.0, 75.0);
        harness.hover_at(start);
        harness.drag_at(start);
        harness.step();
        harness.hover_at(end);
        harness.step();
        harness.drop_at(end);
        harness.run();

        let after = harness.state().view.scene_rect.center();
        assert!(
            before.distance(after) > 10.0,
            "primary drag did not pan the authoring canvas: {before:?} -> {after:?}"
        );
        assert_eq!(harness.state().selection, selection_before);
        assert!(harness.state().edge_in_progress.is_none());
    }

    #[test]
    fn editable_team_toolbar_wraps_without_clipping_controls() {
        let app = TeamApp::new(
            0,
            dense_peer_draft(),
            vec![],
            vec![GatewayDescriptor {
                id: "opencode".to_owned(),
                name: "OpenCode".to_owned(),
            }],
            vec![],
            vec![],
            String::new(),
            TeamView::Edit,
            RuntimeCapabilities::default(),
        );
        let mut harness = egui_kittest::Harness::builder()
            .with_size(eframe::egui::vec2(1100.0, 760.0))
            .wgpu()
            .build_ui_state(|ui, app| app.update(ui), app);
        harness.run();

        let mode = harness.get_by_value("Edit Team").rect();
        let gateway = harness.get_by_value("OpenCode · opencode").rect();
        assert!(
            (mode.center().y - gateway.center().y).abs() <= 1.0,
            "mode and gateway selectors are vertically misaligned: {mode:?} {gateway:?}"
        );
        for label in [
            "+ Peer",
            "+ Specialist",
            "Call",
            "Peer",
            "Validate",
            "Publish revision",
        ] {
            let rect = harness.get_by_label(label).rect();
            assert!(
                rect.min.x >= 0.0 && rect.max.x <= 1100.0,
                "toolbar control {label:?} is horizontally clipped: {rect:?}"
            );
            assert!(
                rect.min.y >= 0.0 && rect.max.y <= harness.state().graph_screen_rect.top(),
                "toolbar control {label:?} escaped the toolbar: {rect:?}"
            );
        }
        harness.snapshot("editable_team_responsive_toolbar");
    }

    #[test]
    fn editable_team_drag_cannot_overlap_another_node() {
        let model = |id: &str| ModelChoice {
            route: "go".to_owned(),
            id: id.to_owned(),
        };
        let draft = TeamDraft {
            id: "collision-test".to_owned(),
            name: "Collision Test".to_owned(),
            base_revision: 1,
            gateway: "opencode".to_owned(),
            primary_id: "primary".to_owned(),
            primary: DraftAssignment {
                model: model("primary"),
                definition: "Choose the accountable Agent.".to_owned(),
            },
            members: vec![
                DraftMember {
                    id: "one".to_owned(),
                    model: model("one"),
                    definition: "First member.".to_owned(),
                },
                DraftMember {
                    id: "two".to_owned(),
                    model: model("two"),
                    definition: "Second member.".to_owned(),
                },
            ],
            peer_ids: vec!["one".to_owned(), "two".to_owned()],
            call_edges: vec![],
            peer_edges: vec![],
        };
        let app = TeamApp::new(
            0,
            draft,
            vec![],
            vec![],
            vec![],
            vec![],
            String::new(),
            TeamView::Edit,
            RuntimeCapabilities::default(),
        );
        let mut harness = egui_kittest::Harness::builder()
            .with_size(eframe::egui::vec2(1200.0, 820.0))
            .wgpu()
            .build_ui_state(|ui, app| app.update(ui), app);
        harness.run();

        let one = NodeId::from_u64(harness.state().member_uids[0]);
        let two = NodeId::from_u64(harness.state().member_uids[1]);
        let sizes = egui_graph::with_graph_memory(&harness.ctx, super::team_graph_id(), |memory| {
            memory.node_sizes().clone()
        });
        let one_center = harness
            .state()
            .graph_to_screen(harness.state().view.layout[&one] + sizes[&one] * 0.5);
        let two_center = harness
            .state()
            .graph_to_screen(harness.state().view.layout[&two] + sizes[&two] * 0.5);
        harness.hover_at(one_center);
        harness.drag_at(one_center);
        harness.step();
        harness.hover_at(two_center);
        harness.step();
        harness.drop_at(two_center);
        harness.run();

        let one_rect =
            eframe::egui::Rect::from_min_size(harness.state().view.layout[&one], sizes[&one]);
        let two_rect =
            eframe::egui::Rect::from_min_size(harness.state().view.layout[&two], sizes[&two]);
        let overlaps = one_rect.min.x < two_rect.max.x
            && two_rect.min.x < one_rect.max.x
            && one_rect.min.y < two_rect.max.y
            && two_rect.min.y < one_rect.max.y;
        assert!(
            !overlaps,
            "dragged node overlapped its target: {one_rect:?} {two_rect:?}"
        );
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
    fn rounded_read_only_route_preserves_expanded_obstacle_clearance() {
        let from_id = NodeId::from_u64(1);
        let blocker_id = NodeId::from_u64(2);
        let to_id = NodeId::from_u64(3);
        let from = eframe::egui::Rect::from_min_size(
            eframe::egui::pos2(0.0, 80.0),
            eframe::egui::vec2(90.0, 60.0),
        );
        let blocker = eframe::egui::Rect::from_min_size(
            eframe::egui::pos2(150.0, 60.0),
            eframe::egui::vec2(100.0, 100.0),
        );
        let to = eframe::egui::Rect::from_min_size(
            eframe::egui::pos2(320.0, 80.0),
            eframe::egui::vec2(90.0, 60.0),
        );
        let nodes = [(from_id, from), (blocker_id, blocker), (to_id, to)];
        let polyline = route_between_rects(from_id, from, to_id, to, &nodes);
        let rounded = rounded_obstacle_safe_route(&polyline, &[blocker], 7.0, 20.0);
        assert_eq!(rounded.first(), polyline.first());
        assert_eq!(rounded.last(), polyline.last());
        assert!(rounded.len() > polyline.len());
        assert!(rounded.windows(2).all(|segment| {
            segment_rect_entry(segment[0], segment[1], blocker.expand(7.0)).is_none()
        }));
        assert_eq!(
            rounded,
            rounded_obstacle_safe_route(&polyline, &[blocker], 7.0, 20.0)
        );
    }

    #[test]
    fn read_only_team_surface_renders_content_without_authoring_controls() {
        super::TEST_HOST_EXCHANGES.store(0, std::sync::atomic::Ordering::SeqCst);
        let model = |id: &str| ModelChoice {
            route: "zen".to_owned(),
            id: id.to_owned(),
        };
        let draft = TeamDraft {
            id: "inspection-team".to_owned(),
            name: "Inspection Team".to_owned(),
            base_revision: 4,
            gateway: "opencode".to_owned(),
            primary_id: "primary".to_owned(),
            primary: DraftAssignment {
                model: model("inspect-primary"),
                definition: "Receive every turn and own or hand off the result.".to_owned(),
            },
            members: vec![
                DraftMember {
                    id: "engineering".to_owned(),
                    model: model("inspect-engineer"),
                    definition: "Own implementation.".to_owned(),
                },
                DraftMember {
                    id: "research".to_owned(),
                    model: model("inspect-researcher"),
                    definition: "Establish external evidence.".to_owned(),
                },
                DraftMember {
                    id: "engineering-specialist".to_owned(),
                    model: model("inspect-engineering-specialist"),
                    definition: "Return bounded implementation findings.".to_owned(),
                },
                DraftMember {
                    id: "research-specialist".to_owned(),
                    model: model("inspect-research-specialist"),
                    definition: "Return bounded evidence findings.".to_owned(),
                },
            ],
            peer_ids: vec!["engineering".to_owned(), "research".to_owned()],
            call_edges: vec![
                CallEdge {
                    agent_id: "engineering".to_owned(),
                    specialist_id: "research-specialist".to_owned(),
                },
                CallEdge {
                    agent_id: "research".to_owned(),
                    specialist_id: "engineering-specialist".to_owned(),
                },
            ],
            peer_edges: vec![
                PeerEdge {
                    first_agent_id: "primary".to_owned(),
                    second_agent_id: "engineering".to_owned(),
                },
                PeerEdge {
                    first_agent_id: "primary".to_owned(),
                    second_agent_id: "research".to_owned(),
                },
            ],
        };
        let mut app = TeamApp::new(
            0,
            draft,
            vec![],
            vec![],
            vec![],
            vec![],
            String::new(),
            TeamView::Inspect,
            RuntimeCapabilities::default(),
        );
        assert_eq!(
            app.visible_graph_edges().len(),
            4,
            "Inspect should initially expose the complete Team architecture"
        );
        app.show_all_edges = false;
        app.selection = Selection::Member(100);
        let focused = app.visible_graph_edges();
        assert_eq!(
            focused.len(),
            2,
            "Peer focus should contain its primary relationship and outgoing specialist call"
        );
        assert!(focused
            .iter()
            .any(|edge| edge.key == "peer:primary:engineering"));
        assert!(!focused
            .iter()
            .any(|edge| edge.key == "peer:primary:research"));
        assert!(focused
            .iter()
            .any(|edge| edge.key == "call:engineering:research-specialist"));
        assert!(!focused
            .iter()
            .any(|edge| edge.key == "call:research:engineering-specialist"));
        app.show_all_edges = true;
        assert_eq!(
            app.visible_graph_edges().len(),
            4,
            "explicit exhaustive mode should expose every stored edge"
        );
        app.selection = Selection::Primary;
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
            "Inspect Team",
            "Focus selection",
            "Details",
            "inspect-primary",
            "engineering",
            "research",
        ] {
            assert!(
                text.contains(expected),
                "missing {expected:?} from:\n{text}"
            );
        }
        for forbidden in ["Publish revision", "Validate", "+ Peer", "+ Specialist"] {
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
            route: "zen".to_owned(),
            id: "model".to_owned(),
        };
        let draft = TeamDraft {
            id: "team-generated-id".to_owned(),
            name: "Scrollable Team".to_owned(),
            base_revision: 0,
            gateway: "opencode".to_owned(),
            primary_id: "primary".to_owned(),
            primary: DraftAssignment {
                model: model.clone(),
                definition: String::new(),
            },
            members: vec![DraftMember {
                id: "engineering".to_owned(),
                model,
                definition: "A long editable definition.\n".repeat(30),
            }],
            peer_ids: vec!["engineering".to_owned()],
            call_edges: vec![],
            peer_edges: vec![],
        };
        let mut app = TeamApp::new(
            0,
            draft,
            vec![],
            vec![],
            vec![],
            vec![],
            String::new(),
            TeamView::New,
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
            route: "zen".to_owned(),
            id: id.to_owned(),
        };
        let draft = TeamDraft {
            id: "free".to_owned(),
            name: "Free".to_owned(),
            base_revision: 1,
            gateway: "opencode".to_owned(),
            primary_id: "primary".to_owned(),
            primary: DraftAssignment {
                model: model("big-pickle"),
                definition: "Route between the two Agents.".to_owned(),
            },
            members: vec![
                DraftMember {
                    id: "engineering".to_owned(),
                    model: model("deepseek-v4-flash-free"),
                    definition: "Own engineering.".to_owned(),
                },
                DraftMember {
                    id: "research".to_owned(),
                    model: model("laguna-s-2.1-free"),
                    definition: "Own research.".to_owned(),
                },
                DraftMember {
                    id: "specialist".to_owned(),
                    model: model("mimo-v2.5-free"),
                    definition: "Return bounded findings.".to_owned(),
                },
            ],
            peer_ids: vec!["engineering".to_owned(), "research".to_owned()],
            call_edges: vec![CallEdge {
                agent_id: "engineering".to_owned(),
                specialist_id: "specialist".to_owned(),
            }],
            peer_edges: vec![PeerEdge {
                first_agent_id: "engineering".to_owned(),
                second_agent_id: "research".to_owned(),
            }],
        };
        let mut editor = TeamApp::new(
            0,
            draft.clone(),
            vec![],
            vec![],
            vec![],
            vec![],
            String::new(),
            TeamView::Edit,
            RuntimeCapabilities::default(),
        );
        let mut inspector = TeamApp::new(
            0,
            draft,
            vec![],
            vec![],
            vec![],
            vec![],
            String::new(),
            TeamView::Inspect,
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
            super::primary_node_id(),
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
    fn live_parallel_collaboration_has_a_stable_readable_snapshot() {
        let node =
            |id: &str, kind: &str, label: &str, status: &str, primary: bool| -> ThinkingNode {
                ThinkingNode {
                    id: id.to_owned(),
                    kind: kind.to_owned(),
                    label: label.to_owned(),
                    status: status.to_owned(),
                    actor: String::new(),
                    metadata: if primary {
                        BTreeMap::from([("primary".to_owned(), "true".to_owned())])
                    } else {
                        BTreeMap::new()
                    },
                }
            };
        let edge = |id: &str,
                    from: &str,
                    to: &str,
                    kind: &str,
                    direction: &str,
                    status: &str,
                    active: usize|
         -> ThinkingEdge {
            ThinkingEdge {
                id: id.to_owned(),
                from: from.to_owned(),
                to: to.to_owned(),
                kind: kind.to_owned(),
                direction: direction.to_owned(),
                status: status.to_owned(),
                count: 1,
                active,
                started_at_ms: 0,
                metadata: BTreeMap::new(),
            }
        };
        let active = ThinkingTurn {
            id: "turn:parallel".to_owned(),
            ordinal: 1,
            task: "Assess a design while runtime and evidence work proceed concurrently."
                .to_owned(),
            status: "running".to_owned(),
            sequence: 12,
            nodes: BTreeMap::from([
                (
                    "user".to_owned(),
                    node("user", "user", "You", "completed", false),
                ),
                (
                    "member:systems".to_owned(),
                    node(
                        "member:systems",
                        "agent",
                        "systems-primary",
                        "running",
                        true,
                    ),
                ),
                (
                    "member:research".to_owned(),
                    node("member:research", "agent", "research-peer", "idle", false),
                ),
                (
                    "member:runtime".to_owned(),
                    node("member:runtime", "specialist", "runtime", "running", false),
                ),
                (
                    "member:evidence".to_owned(),
                    node(
                        "member:evidence",
                        "agent",
                        "evidence-peer",
                        "running",
                        false,
                    ),
                ),
                (
                    "tool:terminal".to_owned(),
                    node("tool:terminal", "tool", "terminal", "running", false),
                ),
            ]),
            edges: BTreeMap::from([
                (
                    "allowed:user:systems".to_owned(),
                    edge(
                        "allowed:user:systems",
                        "user",
                        "member:systems",
                        "allowed",
                        "outward",
                        "idle",
                        0,
                    ),
                ),
                (
                    "allowed-peer:research:systems".to_owned(),
                    edge(
                        "allowed-peer:research:systems",
                        "member:research",
                        "member:systems",
                        "allowed-peer",
                        "bidirectional",
                        "idle",
                        0,
                    ),
                ),
                (
                    "allowed:systems:runtime".to_owned(),
                    edge(
                        "allowed:systems:runtime",
                        "member:systems",
                        "member:runtime",
                        "allowed",
                        "outward",
                        "idle",
                        0,
                    ),
                ),
                (
                    "allowed-peer:systems:evidence".to_owned(),
                    edge(
                        "allowed-peer:systems:evidence",
                        "member:systems",
                        "member:evidence",
                        "allowed-peer",
                        "bidirectional",
                        "idle",
                        0,
                    ),
                ),
                (
                    "flow:request".to_owned(),
                    edge(
                        "flow:request",
                        "user",
                        "member:systems",
                        "request",
                        "outward",
                        "completed",
                        0,
                    ),
                ),
                (
                    "flow:delegation".to_owned(),
                    edge(
                        "flow:delegation",
                        "member:systems",
                        "member:runtime",
                        "delegation",
                        "outward",
                        "running",
                        1,
                    ),
                ),
                (
                    "flow:peer".to_owned(),
                    edge(
                        "flow:peer",
                        "member:systems",
                        "member:evidence",
                        "peer",
                        "bidirectional",
                        "running",
                        1,
                    ),
                ),
                (
                    "flow:tool".to_owned(),
                    edge(
                        "flow:tool",
                        "member:runtime",
                        "tool:terminal",
                        "tool",
                        "outward",
                        "running",
                        1,
                    ),
                ),
            ]),
        };
        let state = ThinkingProjection {
            session_id: "session:visual-audit".to_owned(),
            active_turn_id: active.id.clone(),
            revision: 12,
            turns: vec![ThinkingTurnSummary {
                id: active.id.clone(),
                ordinal: active.ordinal,
                task: active.task.clone(),
                status: active.status.clone(),
                sequence: active.sequence,
            }],
            active,
        };
        let context = eframe::egui::Context::default();
        let mut app = ThinkingApp::new(
            0,
            state,
            RuntimeCapabilities::default(),
            String::new(),
            &context,
        );
        // The fixture is already the latest durable projection; no host
        // exchange is needed to render it in the isolated visual audit.
        app.observed_generation = 0;
        let mut harness = egui_kittest::Harness::builder()
            .with_size(eframe::egui::vec2(1600.0, 1000.0))
            .wgpu()
            .build_ui_state(|ui, app| app.show(ui), app);
        harness.run_steps(5);
        assert!(!harness.state().inspector_open);
        for edge in ["flow:request", "flow:delegation", "flow:peer", "flow:tool"] {
            assert!(
                harness.state().routes.path(edge).is_some(),
                "missing routed live edge {edge}"
            );
        }
        harness.snapshot("thinking_parallel_collaboration");
    }

    #[test]
    fn live_motion_budget_prioritizes_the_inspected_lane() {
        let make_edge = |index: usize| ThinkingEdge {
            id: format!("edge:{index}"),
            from: if index == 5 {
                "selected".to_owned()
            } else {
                "source".to_owned()
            },
            to: format!("target:{index}"),
            kind: "delegation".to_owned(),
            direction: "outward".to_owned(),
            status: "running".to_owned(),
            count: 1,
            active: 1,
            started_at_ms: index as i64,
            metadata: BTreeMap::new(),
        };
        let turn = ThinkingTurn {
            edges: (0..6)
                .map(|index| {
                    let edge = make_edge(index);
                    (edge.id.clone(), edge)
                })
                .collect(),
            ..ThinkingTurn::default()
        };
        let selected = animated_thinking_edges(&turn, Some("selected"), 4);
        assert_eq!(selected.len(), 4);
        assert!(selected.contains("edge:5"));
        assert!(selected.contains("edge:4"));
        assert!(selected.contains("edge:3"));
        assert!(selected.contains("edge:2"));
        assert!(!selected.contains("edge:0"));
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
                        "member:primary".to_owned(),
                        ThinkingNode {
                            id: "member:primary".to_owned(),
                            kind: "agent".to_owned(),
                            label: "primary".to_owned(),
                            status: "running".to_owned(),
                            actor: String::new(),
                            metadata: BTreeMap::from([("primary".to_owned(), "true".to_owned())]),
                        },
                    ),
                    (
                        "member:vision".to_owned(),
                        ThinkingNode {
                            id: "member:vision".to_owned(),
                            kind: "specialist".to_owned(),
                            label: "vision".to_owned(),
                            status: "running".to_owned(),
                            actor: String::new(),
                            metadata: BTreeMap::new(),
                        },
                    ),
                ]),
                edges: BTreeMap::from([(
                    "allowed-specialist:primary:vision".to_owned(),
                    ThinkingEdge {
                        id: "allowed-specialist:primary:vision".to_owned(),
                        from: "member:primary".to_owned(),
                        to: "member:vision".to_owned(),
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
                from: "member:vision".to_owned(),
                to: "member:primary".to_owned(),
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
    fn thinking_request_route_avoids_a_peer_between_user_and_primary() {
        let node = |id: &str, kind: &str, primary: bool| ThinkingNode {
            id: id.to_owned(),
            kind: kind.to_owned(),
            label: id.to_owned(),
            status: "idle".to_owned(),
            actor: String::new(),
            metadata: if primary {
                BTreeMap::from([("primary".to_owned(), "true".to_owned())])
            } else {
                BTreeMap::new()
            },
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
                    ("user".to_owned(), node("user", "user", false)),
                    (
                        "member:primary".to_owned(),
                        node("member:primary", "agent", true),
                    ),
                    (
                        "member:engineering".to_owned(),
                        node("member:engineering", "agent", false),
                    ),
                    (
                        "member:research".to_owned(),
                        node("member:research", "agent", false),
                    ),
                ]),
                edges: BTreeMap::from([(
                    "flow:request".to_owned(),
                    ThinkingEdge {
                        id: "flow:request".to_owned(),
                        from: "user".to_owned(),
                        to: "member:primary".to_owned(),
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
        // Exercise the worst geometric case directly: a peer lies between
        // the user and primary centres, so the request needs an obstacle-safe
        // corridor rather than a straight segment through that peer.
        let positions = HashMap::from([
            (thinking_node_id("user"), eframe::egui::pos2(0.0, -320.0)),
            (
                thinking_node_id("member:engineering"),
                eframe::egui::pos2(-15.0, -160.0),
            ),
            (
                thinking_node_id("member:primary"),
                eframe::egui::pos2(0.0, 40.0),
            ),
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
            (
                thinking_node_id("member:primary"),
                eframe::egui::vec2(210.0, 82.0),
            ),
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
            "the routed request still crosses the engineering peer: {path:?}"
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
