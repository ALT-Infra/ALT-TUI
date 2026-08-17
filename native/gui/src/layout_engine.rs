use std::collections::{BTreeMap, BTreeSet, VecDeque};

#[derive(Clone, Copy, Debug, Default, PartialEq)]
pub(crate) struct Point {
    pub(crate) x: f32,
    pub(crate) y: f32,
}

#[derive(Clone, Copy, Debug, Default, PartialEq)]
pub(crate) struct Size {
    pub(crate) width: f32,
    pub(crate) height: f32,
}

impl Size {
    fn half_diagonal(self) -> f32 {
        0.5 * self.width.hypot(self.height)
    }
}

#[derive(Clone, Copy, Debug, Eq, Ord, PartialEq, PartialOrd)]
pub(crate) struct Roles {
    pub(crate) agent: bool,
    pub(crate) callable: bool,
}

#[derive(Clone, Debug)]
pub(crate) struct Member {
    pub(crate) key: u64,
    pub(crate) size: Size,
    pub(crate) roles: Roles,
}

#[derive(Clone, Copy, Debug, Eq, Ord, PartialEq, PartialOrd)]
pub(crate) struct CallEdge {
    pub(crate) from: u64,
    pub(crate) to: u64,
    pub(crate) relation: EdgeRelation,
}

#[derive(Clone, Copy, Debug, Eq, Ord, PartialEq, PartialOrd)]
pub(crate) enum EdgeRelation {
    Call,
    Peer,
}

#[derive(Clone, Debug)]
pub(crate) struct Team {
    pub(crate) primary_key: u64,
    pub(crate) primary_size: Size,
    pub(crate) members: Vec<Member>,
    pub(crate) call_edges: Vec<CallEdge>,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum LayoutKind {
    /// A distinguished Primary surrounded by one structurally homogeneous,
    /// strongly connected member orbit.
    PrimaryOrbit,
    /// A directed condensation graph whose components are laid out in ranks.
    LayeredModules,
}

#[derive(Clone, Debug)]
#[cfg_attr(not(test), allow(dead_code))]
pub(crate) struct Layout {
    pub(crate) kind: LayoutKind,
    /// Top-left positions in graph space.
    pub(crate) positions: BTreeMap<u64, Point>,
    /// Strongly connected member components, each sorted by stable key.
    pub(crate) components: Vec<Vec<u64>>,
    /// Stable structural-equivalence color for each member.
    pub(crate) colors: BTreeMap<u64, usize>,
}

const MODULE_GAP: f32 = 72.0;
const RANK_GAP: f32 = 128.0;
const NODE_GAP: f32 = 54.0;

pub(crate) fn layout_team(team: &Team) -> Layout {
    let canonical = CanonicalTeam::new(team);
    let sccs = strongly_connected_components(canonical.members.len(), &canonical.edges);
    let colors = structural_colors(&canonical);

    let homogeneous_orbit = sccs.len() == 1
        && !canonical.members.is_empty()
        && sccs[0].len() == canonical.members.len()
        && canonical
            .members
            .iter()
            .all(|member| member.roles.agent && member.roles.callable)
        && canonical
            .members
            .iter()
            .map(|member| colors[&member.key])
            .all_equal();

    let mut positions = physics_positions(team).unwrap_or_else(|| {
        if homogeneous_orbit {
            layout_primary_orbit(&canonical, &sccs[0])
        } else {
            layout_layered_modules(&canonical, &sccs, &colors)
        }
    });
    center_positions(&canonical, &mut positions);

    let components = sccs
        .iter()
        .map(|component| {
            let mut keys: Vec<_> = component
                .iter()
                .map(|&index| canonical.members[index].key)
                .collect();
            keys.sort_unstable();
            keys
        })
        .collect();

    Layout {
        kind: if homogeneous_orbit {
            LayoutKind::PrimaryOrbit
        } else {
            LayoutKind::LayeredModules
        },
        positions,
        components,
        colors,
    }
}

fn physics_positions(team: &Team) -> Option<BTreeMap<u64, Point>> {
    use alt_graph_physics as physics;

    let mut nodes = Vec::with_capacity(team.members.len() + 1);
    nodes.push(physics::Node {
        id: team.primary_key,
        size: physics::Size::new(
            team.primary_size.width as f64,
            team.primary_size.height as f64,
        ),
        pin: physics::Pin::Free,
    });
    nodes.extend(team.members.iter().map(|member| physics::Node {
        id: member.key,
        size: physics::Size::new(member.size.width as f64, member.size.height as f64),
        pin: physics::Pin::Free,
    }));

    let mut next_edge = 0u64;
    let mut edges = Vec::new();
    let mut constraints = vec![physics::AxisConstraint::Position {
        node: team.primary_key,
        axis: physics::Axis::Vertical,
        coordinate: 0.0,
        weight: 30.0,
    }];
    let mut push_edge = |source, target, kind, ideal_length, weight| {
        next_edge += 1;
        edges.push(physics::Edge {
            id: next_edge,
            source,
            target,
            kind,
            ideal_length,
            weight,
            source_port: physics::Port::Free,
            target_port: physics::Port::Free,
        });
    };
    let mut agent_keys: Vec<_> = team
        .members
        .iter()
        .filter(|member| member.roles.agent)
        .map(|member| member.key)
        .collect();
    agent_keys.sort_unstable();
    let agent_set: BTreeSet<_> = agent_keys.iter().copied().collect();
    for &member_key in &agent_keys {
        push_edge(
            team.primary_key,
            member_key,
            physics::EdgeKind::Directed {
                target_delta: 190.0,
            },
            230.0,
            1.4,
        );
        constraints.push(physics::AxisConstraint::Offset {
            source: team.primary_key,
            target: member_key,
            axis: physics::Axis::Vertical,
            delta: 190.0,
            weight: 16.0,
        });
        if member_key != agent_keys[0] {
            constraints.push(physics::AxisConstraint::Alignment {
                first: agent_keys[0],
                second: member_key,
                axis: physics::Axis::Vertical,
                weight: 20.0,
            });
        }
        constraints.push(physics::AxisConstraint::Separation {
            before: team.primary_key,
            after: member_key,
            axis: physics::Axis::Vertical,
            minimum: 150.0,
            weight: 24.0,
        });
    }
    for member in team.members.iter().filter(|member| !member.roles.agent) {
        constraints.push(physics::AxisConstraint::Separation {
            before: team.primary_key,
            after: member.key,
            axis: physics::Axis::Vertical,
            minimum: 145.0,
            weight: 24.0,
        });
    }
    let mut semantic_edges = team.call_edges.clone();
    semantic_edges.sort_unstable();
    for edge in &semantic_edges {
        let (kind, ideal, weight) = match edge.relation {
            EdgeRelation::Call => (
                physics::EdgeKind::Directed {
                    target_delta: 190.0,
                },
                230.0,
                1.0,
            ),
            // A peer relationship grants the same consultation and handoff
            // authority in both directions. It is an association, never a
            // hidden parent/child constraint chosen from serialized order.
            EdgeRelation::Peer => (physics::EdgeKind::Association, 250.0, 0.9),
        };
        push_edge(edge.from, edge.to, kind, ideal, weight);
        match edge.relation {
            EdgeRelation::Call if !agent_set.contains(&edge.to) => {
                constraints.push(physics::AxisConstraint::Separation {
                    before: edge.from,
                    after: edge.to,
                    axis: physics::Axis::Vertical,
                    minimum: 150.0,
                    weight: 18.0,
                });
            }
            EdgeRelation::Call | EdgeRelation::Peer => {}
        }
    }
    let config = physics::LayoutConfig {
        max_iterations: 360,
        // Keep one physical pixel beyond the visual clearance so f64 -> f32
        // conversion cannot turn an exact tangency into an overlap.
        clearance: NODE_GAP as f64 + 1.0,
        route_clearance: 14.0,
        hierarchy_weight: 2.8,
        crossing_weight: 0.3,
        ..physics::LayoutConfig::default()
    };
    let output = physics::layout(&physics::LayoutInput {
        nodes,
        edges,
        constraints,
        config,
    })
    .ok()?;
    Some(
        output
            .placements
            .into_iter()
            .map(|(key, placement)| {
                (
                    key,
                    Point {
                        x: (placement.center.x - placement.size.width * 0.5) as f32,
                        y: (placement.center.y - placement.size.height * 0.5) as f32,
                    },
                )
            })
            .collect(),
    )
}

/// Place an external terminal without moving the stable Team core.
///
/// The terminal is constrained to the boundary of the Minkowski expansion of
/// the core's axis-aligned rectangles. Candidate directions are the critical
/// directions induced by that geometry: rectangle axes, member rays and their
/// normals, and angular-gap bisectors. A lexicographic objective then chooses
/// the candidate with the least occluded request/return flow, the least
/// degenerate flow cycles, the greatest angular clearance, and the most compact
/// boundary position. The left boundary is only the final symmetry breaker.
///
/// This is the one-free-node reduction of a pinned, boundary-constrained graph
/// layout. It deliberately does not run a force simulation or move Team nodes.
pub(crate) fn place_boundary_terminal(team: &Team, layout: &Layout, terminal_size: Size) -> Point {
    let primary_top_left = layout
        .positions
        .get(&team.primary_key)
        .copied()
        .unwrap_or_default();
    let primary_center = center(primary_top_left, team.primary_size);
    let members: Vec<_> = team
        .members
        .iter()
        .filter_map(|member| {
            let position = layout.positions.get(&member.key).copied()?;
            let member_center = center(position, member.size);
            Some(CoreNode {
                key: member.key,
                center: member_center,
                size: member.size,
                agent: member.roles.agent,
            })
        })
        .collect();

    let primary = CoreNode {
        key: team.primary_key,
        center: primary_center,
        size: team.primary_size,
        agent: false,
    };
    let mut core = Vec::with_capacity(members.len() + 1);
    core.push(primary);
    core.extend(members.iter().copied());

    let occupied: Vec<_> = members
        .iter()
        .map(|member| {
            angle_key(
                (member.center.y - primary_center.y) as f64,
                (member.center.x - primary_center.x) as f64,
            )
        })
        .collect();
    let visibility_boundaries: Vec<_> = members
        .iter()
        .flat_map(|member| {
            let half_width = member.size.width * 0.5;
            let half_height = member.size.height * 0.5;
            [
                (-half_width, -half_height),
                (half_width, -half_height),
                (half_width, half_height),
                (-half_width, half_height),
            ]
            .into_iter()
            .map(|(dx, dy)| {
                angle_key(
                    (member.center.y + dy - primary_center.y) as f64,
                    (member.center.x + dx - primary_center.x) as f64,
                )
            })
        })
        .collect();
    let candidate_angles = boundary_candidate_angles(&occupied, &visibility_boundaries);
    let mut candidates: Vec<_> = candidate_angles
        .into_iter()
        .map(|angle| {
            boundary_candidate(
                angle,
                primary_center,
                terminal_size,
                &core,
                &members,
                &occupied,
            )
        })
        .collect();
    candidates.sort_by(boundary_candidate_order);
    let best = candidates
        .first()
        .expect("boundary placement always has axis candidates");
    top_left(best.center, terminal_size)
}

#[derive(Clone, Copy, Debug)]
struct CoreNode {
    key: u64,
    center: Point,
    size: Size,
    agent: bool,
}

#[derive(Clone, Copy, Debug)]
struct BoundaryCandidate {
    angle: i64,
    center: Point,
    request_blockers: usize,
    worst_return_blockers: usize,
    total_return_blockers: usize,
    minimum_cycle_sine: i64,
    angular_clearance: i64,
    radius: i64,
    preferred_distance: i64,
}

fn boundary_candidate_order(a: &BoundaryCandidate, b: &BoundaryCandidate) -> std::cmp::Ordering {
    a.request_blockers
        .cmp(&b.request_blockers)
        .then_with(|| a.worst_return_blockers.cmp(&b.worst_return_blockers))
        .then_with(|| a.total_return_blockers.cmp(&b.total_return_blockers))
        .then_with(|| b.minimum_cycle_sine.cmp(&a.minimum_cycle_sine))
        .then_with(|| b.angular_clearance.cmp(&a.angular_clearance))
        .then_with(|| a.radius.cmp(&b.radius))
        .then_with(|| a.preferred_distance.cmp(&b.preferred_distance))
        .then_with(|| a.angle.cmp(&b.angle))
}

// Quantization removes meaningless f32 trigonometric noise before exact
// lexicographic comparison. One microradian and one millipixel are both far
// below visible resolution while preserving a transitive total order.
const ANGLE_SCALE: f64 = 1_000_000.0;
const UNIT_SCALE: f64 = 1_000_000.0;
const RADIUS_SCALE: f64 = 1_000.0;

fn tau_key() -> i64 {
    (std::f64::consts::TAU * ANGLE_SCALE).round() as i64
}

fn angle_key(y: f64, x: f64) -> i64 {
    ((y.atan2(x).rem_euclid(std::f64::consts::TAU) * ANGLE_SCALE).round() as i64)
        .rem_euclid(tau_key())
}

fn angle_from_key(key: i64) -> f64 {
    key as f64 / ANGLE_SCALE
}

fn circular_key_distance(a: i64, b: i64) -> i64 {
    let delta = (a - b).rem_euclid(tau_key());
    delta.min(tau_key() - delta)
}

fn boundary_candidate_angles(occupied: &[i64], visibility_boundaries: &[i64]) -> Vec<i64> {
    let quarter = (std::f64::consts::FRAC_PI_2 * ANGLE_SCALE).round() as i64;
    let half = (std::f64::consts::PI * ANGLE_SCALE).round() as i64;
    let mut candidates = BTreeSet::from([
        0,
        quarter.rem_euclid(tau_key()),
        half.rem_euclid(tau_key()),
        (3 * quarter).rem_euclid(tau_key()),
    ]);
    let mut angles = occupied.to_vec();
    angles.sort_unstable();
    angles.dedup();
    for &angle in &angles {
        candidates.insert(angle);
        candidates.insert((angle + quarter).rem_euclid(tau_key()));
        candidates.insert((angle - quarter).rem_euclid(tau_key()));
        candidates.insert((angle + half).rem_euclid(tau_key()));
    }
    match angles.len() {
        0 => {}
        1 => {
            candidates.insert((angles[0] + half).rem_euclid(tau_key()));
        }
        _ => {
            for (index, &angle) in angles.iter().enumerate() {
                let next = angles[(index + 1) % angles.len()];
                let gap = (next - angle).rem_euclid(tau_key());
                candidates.insert((angle + gap / 2).rem_euclid(tau_key()));
            }
        }
    }
    let mut boundaries = visibility_boundaries.to_vec();
    boundaries.sort_unstable();
    boundaries.dedup();
    if boundaries.len() > 1 {
        for (index, &angle) in boundaries.iter().enumerate() {
            let next = boundaries[(index + 1) % boundaries.len()];
            let gap = (next - angle).rem_euclid(tau_key());
            candidates.insert((angle + gap / 2).rem_euclid(tau_key()));
        }
    }
    candidates.into_iter().collect()
}

fn boundary_candidate(
    angle: i64,
    primary_center: Point,
    terminal_size: Size,
    core: &[CoreNode],
    members: &[CoreNode],
    occupied: &[i64],
) -> BoundaryCandidate {
    let radians = angle_from_key(angle);
    let direction = Point {
        x: radians.cos() as f32,
        y: radians.sin() as f32,
    };
    let radius = outside_core_radius(primary_center, direction, terminal_size, core);
    let terminal_center = Point {
        x: primary_center.x + direction.x * radius,
        y: primary_center.y + direction.y * radius,
    };
    let request_blockers = members
        .iter()
        .filter(|node| segment_intersects_node(terminal_center, primary_center, node))
        .count();

    let agents: Vec<_> = members.iter().filter(|node| node.agent).collect();
    let return_blockers: Vec<_> = agents
        .iter()
        .map(|agent| {
            core.iter()
                .filter(|node| node.key != agent.key)
                .filter(|node| segment_intersects_node(agent.center, terminal_center, node))
                .count()
        })
        .collect();
    let worst_return_blockers = return_blockers.iter().copied().max().unwrap_or(0);
    let total_return_blockers = return_blockers.iter().sum();
    let minimum_cycle_sine = agents
        .iter()
        .filter_map(|agent| {
            let dx = (agent.center.x - primary_center.x) as f64;
            let dy = (agent.center.y - primary_center.y) as f64;
            let length = dx.hypot(dy);
            (length > f64::EPSILON).then(|| {
                ((direction.x as f64 * dy - direction.y as f64 * dx).abs() / length * UNIT_SCALE)
                    .round() as i64
            })
        })
        .min()
        .unwrap_or(UNIT_SCALE as i64);
    let angular_clearance = occupied
        .iter()
        .map(|&other| circular_key_distance(angle, other))
        .min()
        .unwrap_or(tau_key());
    let preferred_distance =
        circular_key_distance(angle, (std::f64::consts::PI * ANGLE_SCALE).round() as i64);

    BoundaryCandidate {
        angle,
        center: terminal_center,
        request_blockers,
        worst_return_blockers,
        total_return_blockers,
        minimum_cycle_sine,
        angular_clearance,
        radius: (radius as f64 * RADIUS_SCALE).round() as i64,
        preferred_distance,
    }
}

fn outside_core_radius(
    primary_center: Point,
    direction: Point,
    terminal_size: Size,
    core: &[CoreNode],
) -> f32 {
    let core_support = core
        .iter()
        .map(|node| {
            let delta_x = node.center.x - primary_center.x;
            let delta_y = node.center.y - primary_center.y;
            delta_x * direction.x + delta_y * direction.y + rectangle_support(node.size, direction)
        })
        .fold(0.0_f32, f32::max);
    core_support + rectangle_support(terminal_size, direction) + NODE_GAP
}

fn rectangle_support(size: Size, direction: Point) -> f32 {
    direction.x.abs() * size.width * 0.5 + direction.y.abs() * size.height * 0.5
}

fn segment_intersects_node(from: Point, to: Point, node: &CoreNode) -> bool {
    let min_x = node.center.x - node.size.width * 0.5;
    let max_x = node.center.x + node.size.width * 0.5;
    let min_y = node.center.y - node.size.height * 0.5;
    let max_y = node.center.y + node.size.height * 0.5;
    let dx = to.x - from.x;
    let dy = to.y - from.y;
    let mut low = 0.0_f32;
    let mut high = 1.0_f32;
    for (origin, delta, min, max) in [(from.x, dx, min_x, max_x), (from.y, dy, min_y, max_y)] {
        if delta.abs() <= f32::EPSILON {
            if origin < min || origin > max {
                return false;
            }
            continue;
        }
        let a = (min - origin) / delta;
        let b = (max - origin) / delta;
        low = low.max(a.min(b));
        high = high.min(a.max(b));
        if low > high {
            return false;
        }
    }
    high >= 0.0 && low <= 1.0
}

fn center(top_left: Point, size: Size) -> Point {
    Point {
        x: top_left.x + size.width * 0.5,
        y: top_left.y + size.height * 0.5,
    }
}

fn layout_primary_orbit(team: &CanonicalTeam, component: &[usize]) -> BTreeMap<u64, Point> {
    let mut positions = BTreeMap::new();
    let order = optimized_orbit_order(component, &team.members, &team.edges);
    let radius = orbit_radius(&order, &team.members, Some(team.primary_size), NODE_GAP);
    positions.insert(
        team.primary_key,
        top_left(Point::default(), team.primary_size),
    );
    place_orbit(
        &mut positions,
        Point::default(),
        radius,
        &order,
        &team.members,
    );
    positions
}

fn layout_layered_modules(
    team: &CanonicalTeam,
    components: &[Vec<usize>],
    colors: &BTreeMap<u64, usize>,
) -> BTreeMap<u64, Point> {
    if team.members.is_empty() {
        return BTreeMap::from([(
            team.primary_key,
            top_left(Point::default(), team.primary_size),
        )]);
    }

    let mut component_of = vec![0usize; team.members.len()];
    for (component_index, component) in components.iter().enumerate() {
        for &member in component {
            component_of[member] = component_index;
        }
    }

    let mut dag_edges = BTreeSet::new();
    for &(from, to) in &team.edges {
        let a = component_of[from];
        let b = component_of[to];
        if a != b {
            dag_edges.insert((a, b));
        }
    }
    let ranks = condensation_ranks(components.len(), &dag_edges);

    let mut module_geometries = Vec::with_capacity(components.len());
    for component in components {
        module_geometries.push(module_geometry(component, &team.members, &team.edges));
    }

    let max_rank = ranks.iter().copied().max().unwrap_or(0);
    let mut positions = BTreeMap::new();
    let mut previous_bottom = team.primary_size.height * 0.5;
    positions.insert(
        team.primary_key,
        top_left(Point::default(), team.primary_size),
    );

    for rank in 0..=max_rank {
        let mut modules: Vec<_> = (0..components.len())
            .filter(|&component| ranks[component] == rank)
            .collect();
        modules.sort_by_key(|&component| {
            components[component]
                .iter()
                .map(|&member| (colors[&team.members[member].key], team.members[member].key))
                .min()
                .unwrap_or_default()
        });
        if modules.is_empty() {
            continue;
        }
        let row_height = modules
            .iter()
            .map(|&component| module_geometries[component].size.height)
            .fold(0.0_f32, f32::max);
        let row_width = modules
            .iter()
            .map(|&component| module_geometries[component].size.width)
            .sum::<f32>()
            + MODULE_GAP * modules.len().saturating_sub(1) as f32;
        let row_center_y = previous_bottom + RANK_GAP + row_height * 0.5;
        let mut cursor_x = -row_width * 0.5;
        for component in modules {
            let geometry = &module_geometries[component];
            let module_center = Point {
                x: cursor_x + geometry.size.width * 0.5,
                y: row_center_y,
            };
            for (&member, relative) in components[component]
                .iter()
                .zip(geometry.relative_centers.iter())
            {
                let node = &team.members[member];
                positions.insert(
                    node.key,
                    top_left(
                        Point {
                            x: module_center.x + relative.x,
                            y: module_center.y + relative.y,
                        },
                        node.size,
                    ),
                );
            }
            cursor_x += geometry.size.width + MODULE_GAP;
        }
        previous_bottom = row_center_y + row_height * 0.5;
    }
    positions
}

struct ModuleGeometry {
    size: Size,
    relative_centers: Vec<Point>,
}

fn module_geometry(
    component: &[usize],
    members: &[Member],
    edges: &[(usize, usize)],
) -> ModuleGeometry {
    if component.len() == 1 {
        return ModuleGeometry {
            size: members[component[0]].size,
            relative_centers: vec![Point::default()],
        };
    }

    let order = optimized_orbit_order(component, members, edges);
    let radius = orbit_radius(&order, members, None, NODE_GAP);
    let mut by_member = BTreeMap::new();
    for (slot, &member) in order.iter().enumerate() {
        let angle = orbit_angle(slot, order.len());
        by_member.insert(
            member,
            Point {
                x: radius * angle.cos(),
                y: radius * angle.sin(),
            },
        );
    }
    let max_half_width = component
        .iter()
        .map(|&member| members[member].size.width * 0.5)
        .fold(0.0_f32, f32::max);
    let max_half_height = component
        .iter()
        .map(|&member| members[member].size.height * 0.5)
        .fold(0.0_f32, f32::max);
    ModuleGeometry {
        size: Size {
            width: 2.0 * (radius + max_half_width),
            height: 2.0 * (radius + max_half_height),
        },
        relative_centers: component.iter().map(|member| by_member[member]).collect(),
    }
}

fn place_orbit(
    positions: &mut BTreeMap<u64, Point>,
    center: Point,
    radius: f32,
    order: &[usize],
    members: &[Member],
) {
    for (slot, &member) in order.iter().enumerate() {
        let angle = orbit_angle(slot, order.len());
        let node = &members[member];
        positions.insert(
            node.key,
            top_left(
                Point {
                    x: center.x + radius * angle.cos(),
                    y: center.y + radius * angle.sin(),
                },
                node.size,
            ),
        );
    }
}

fn orbit_angle(slot: usize, count: usize) -> f32 {
    -std::f32::consts::FRAC_PI_2 + std::f32::consts::TAU * slot as f32 / count.max(1) as f32
}

fn orbit_radius(order: &[usize], members: &[Member], center: Option<Size>, gap: f32) -> f32 {
    match order.len() {
        0 => return 0.0,
        1 => {
            return center
                .map(|center| center.half_diagonal() + members[order[0]].size.half_diagonal() + gap)
                .unwrap_or(0.0);
        }
        _ => {}
    }
    let chord_factor = 2.0 * (std::f32::consts::PI / order.len() as f32).sin();
    let adjacent_radius = order
        .iter()
        .enumerate()
        .map(|(slot, &member)| {
            let next = order[(slot + 1) % order.len()];
            (members[member].size.half_diagonal() + members[next].size.half_diagonal() + gap)
                / chord_factor
        })
        .fold(0.0_f32, f32::max);
    let center_radius = center
        .map(|center| {
            let center_radius = center.half_diagonal();
            order
                .iter()
                .map(|&member| center_radius + members[member].size.half_diagonal() + gap)
                .fold(0.0_f32, f32::max)
        })
        .unwrap_or(0.0);
    adjacent_radius.max(center_radius)
}

fn top_left(center: Point, size: Size) -> Point {
    Point {
        x: center.x - size.width * 0.5,
        y: center.y - size.height * 0.5,
    }
}

fn center_positions(team: &CanonicalTeam, positions: &mut BTreeMap<u64, Point>) {
    let mut min = Point {
        x: f32::INFINITY,
        y: f32::INFINITY,
    };
    let mut max = Point {
        x: f32::NEG_INFINITY,
        y: f32::NEG_INFINITY,
    };
    for (&key, point) in positions.iter() {
        let size = if key == team.primary_key {
            team.primary_size
        } else {
            team.members
                .iter()
                .find(|member| member.key == key)
                .map(|member| member.size)
                .unwrap_or_default()
        };
        min.x = min.x.min(point.x);
        min.y = min.y.min(point.y);
        max.x = max.x.max(point.x + size.width);
        max.y = max.y.max(point.y + size.height);
    }
    if !min.x.is_finite() {
        return;
    }
    let center = Point {
        x: (min.x + max.x) * 0.5,
        y: (min.y + max.y) * 0.5,
    };
    for point in positions.values_mut() {
        point.x -= center.x;
        point.y -= center.y;
    }
}

struct CanonicalTeam {
    primary_key: u64,
    primary_size: Size,
    members: Vec<Member>,
    edges: Vec<(usize, usize)>,
}

impl CanonicalTeam {
    fn new(team: &Team) -> Self {
        let mut members = team.members.clone();
        members.sort_by_key(|member| member.key);
        let index: BTreeMap<_, _> = members
            .iter()
            .enumerate()
            .map(|(index, member)| (member.key, index))
            .collect();
        let mut edges = Vec::new();
        for edge in &team.call_edges {
            let (Some(&from), Some(&to)) = (index.get(&edge.from), index.get(&edge.to)) else {
                continue;
            };
            if from == to {
                continue;
            }
            edges.push((from, to));
            if edge.relation == EdgeRelation::Peer {
                edges.push((to, from));
            }
        }
        edges.sort_unstable();
        edges.dedup();
        Self {
            primary_key: team.primary_key,
            primary_size: team.primary_size,
            members,
            edges,
        }
    }
}

fn structural_colors(team: &CanonicalTeam) -> BTreeMap<u64, usize> {
    let mut colors: Vec<usize> = team
        .members
        .iter()
        .map(|member| match (member.roles.agent, member.roles.callable) {
            (false, false) => 0,
            (true, false) => 1,
            (false, true) => 2,
            (true, true) => 3,
        })
        .collect();
    loop {
        let signatures: Vec<_> = (0..team.members.len())
            .map(|member| {
                let mut incoming: Vec<_> = team
                    .edges
                    .iter()
                    .filter(|(_, to)| *to == member)
                    .map(|(from, _)| colors[*from])
                    .collect();
                let mut outgoing: Vec<_> = team
                    .edges
                    .iter()
                    .filter(|(from, _)| *from == member)
                    .map(|(_, to)| colors[*to])
                    .collect();
                incoming.sort_unstable();
                outgoing.sort_unstable();
                (colors[member], incoming, outgoing)
            })
            .collect();
        let unique: BTreeMap<_, _> = signatures
            .iter()
            .cloned()
            .collect::<BTreeSet<_>>()
            .into_iter()
            .enumerate()
            .map(|(color, signature)| (signature, color))
            .collect();
        let next: Vec<_> = signatures
            .iter()
            .map(|signature| unique[signature])
            .collect();
        if next == colors {
            return team
                .members
                .iter()
                .zip(colors)
                .map(|(member, color)| (member.key, color))
                .collect();
        }
        colors = next;
    }
}

fn strongly_connected_components(node_count: usize, edges: &[(usize, usize)]) -> Vec<Vec<usize>> {
    struct Tarjan<'a> {
        adjacency: &'a [Vec<usize>],
        next_index: usize,
        indices: Vec<Option<usize>>,
        lowlink: Vec<usize>,
        stack: Vec<usize>,
        on_stack: Vec<bool>,
        components: Vec<Vec<usize>>,
    }

    impl Tarjan<'_> {
        fn visit(&mut self, node: usize) {
            let index = self.next_index;
            self.next_index += 1;
            self.indices[node] = Some(index);
            self.lowlink[node] = index;
            self.stack.push(node);
            self.on_stack[node] = true;

            for &next in &self.adjacency[node] {
                if self.indices[next].is_none() {
                    self.visit(next);
                    self.lowlink[node] = self.lowlink[node].min(self.lowlink[next]);
                } else if self.on_stack[next] {
                    self.lowlink[node] = self.lowlink[node].min(self.indices[next].unwrap());
                }
            }

            if self.lowlink[node] == index {
                let mut component = Vec::new();
                loop {
                    let member = self.stack.pop().expect("Tarjan stack must contain root");
                    self.on_stack[member] = false;
                    component.push(member);
                    if member == node {
                        break;
                    }
                }
                component.sort_unstable();
                self.components.push(component);
            }
        }
    }

    let mut adjacency = vec![Vec::new(); node_count];
    for &(from, to) in edges {
        if from < node_count && to < node_count && from != to {
            adjacency[from].push(to);
        }
    }
    for neighbors in &mut adjacency {
        neighbors.sort_unstable();
        neighbors.dedup();
    }
    let mut tarjan = Tarjan {
        adjacency: &adjacency,
        next_index: 0,
        indices: vec![None; node_count],
        lowlink: vec![0; node_count],
        stack: Vec::new(),
        on_stack: vec![false; node_count],
        components: Vec::new(),
    };
    for node in 0..node_count {
        if tarjan.indices[node].is_none() {
            tarjan.visit(node);
        }
    }
    tarjan
        .components
        .sort_by_key(|component| component.iter().copied().min().unwrap_or(usize::MAX));
    tarjan.components
}

fn condensation_ranks(component_count: usize, edges: &BTreeSet<(usize, usize)>) -> Vec<usize> {
    let mut incoming = vec![0usize; component_count];
    let mut outgoing = vec![Vec::new(); component_count];
    for &(from, to) in edges {
        outgoing[from].push(to);
        incoming[to] += 1;
    }
    let mut ready = VecDeque::new();
    for (component, &degree) in incoming.iter().enumerate() {
        if degree == 0 {
            ready.push_back(component);
        }
    }
    let mut ranks = vec![0usize; component_count];
    let mut visited = 0usize;
    while let Some(component) = ready.pop_front() {
        visited += 1;
        for &next in &outgoing[component] {
            ranks[next] = ranks[next].max(ranks[component] + 1);
            incoming[next] -= 1;
            if incoming[next] == 0 {
                ready.push_back(next);
            }
        }
    }
    debug_assert_eq!(visited, component_count, "SCC condensation must be acyclic");
    ranks
}

fn optimized_orbit_order(
    component: &[usize],
    members: &[Member],
    edges: &[(usize, usize)],
) -> Vec<usize> {
    let mut order = component.to_vec();
    order.sort_by_key(|&member| members[member].key);
    if order.len() <= 2 {
        return order;
    }

    // Circular crossing minimization is combinatorial. Use deterministic
    // vertex insertion for every component size instead of changing behavior
    // at an arbitrary member count. The smallest stable key remains anchored
    // because rotation cannot change the objective. Each accepted move
    // strictly decreases (crossings, stable-key order), so this terminates.
    let stable_visit = order[1..].to_vec();
    loop {
        let mut changed = false;
        for &member in &stable_visit {
            let current_score = circular_crossings(&order, edges);
            let current_keys: Vec<_> = order.iter().map(|&index| members[index].key).collect();
            let Some(position) = order.iter().position(|&index| index == member) else {
                continue;
            };
            let mut remainder = order.clone();
            remainder.remove(position);

            let mut best = order.clone();
            let mut best_score = current_score;
            let mut best_keys = current_keys;
            for insertion in 1..=remainder.len() {
                let mut candidate = remainder.clone();
                candidate.insert(insertion, member);
                let score = circular_crossings(&candidate, edges);
                let keys: Vec<_> = candidate.iter().map(|&index| members[index].key).collect();
                if score < best_score || score == best_score && keys < best_keys {
                    best = candidate;
                    best_score = score;
                    best_keys = keys;
                }
            }
            if best != order {
                order = best;
                changed = true;
            }
        }
        if !changed {
            return order;
        }
    }
}

fn circular_crossings(order: &[usize], edges: &[(usize, usize)]) -> usize {
    let positions: BTreeMap<_, _> = order
        .iter()
        .enumerate()
        .map(|(position, &member)| (member, position))
        .collect();
    let undirected: Vec<_> = edges
        .iter()
        .map(|&(a, b)| (a.min(b), a.max(b)))
        .collect::<BTreeSet<_>>()
        .into_iter()
        .collect();
    let mut crossings = 0;
    for (left_index, &(a, b)) in undirected.iter().enumerate() {
        for &(c, d) in &undirected[left_index + 1..] {
            if a == c || a == d || b == c || b == d {
                continue;
            }
            let Some((&pa, &pb, &pc, &pd)) = positions
                .get(&a)
                .zip(positions.get(&b))
                .zip(positions.get(&c))
                .zip(positions.get(&d))
                .map(|(((a, b), c), d)| (a, b, c, d))
            else {
                continue;
            };
            let between = |x: usize, start: usize, end: usize| {
                if start < end {
                    start < x && x < end
                } else {
                    x > start || x < end
                }
            };
            if between(pc, pa, pb) != between(pd, pa, pb) {
                crossings += 1;
            }
        }
    }
    crossings
}

trait IteratorAllEqual: Iterator {
    fn all_equal(mut self) -> bool
    where
        Self::Item: PartialEq,
        Self: Sized,
    {
        let Some(first) = self.next() else {
            return true;
        };
        self.all(|item| item == first)
    }
}

impl<I: Iterator> IteratorAllEqual for I {}

#[cfg(test)]
mod tests {
    use super::*;

    fn member(key: u64, agent: bool, callable: bool) -> Member {
        Member {
            key,
            size: Size {
                width: 220.0,
                height: 92.0,
            },
            roles: Roles { agent, callable },
        }
    }

    fn complete_team(keys: &[u64]) -> Team {
        Team {
            primary_key: 1,
            primary_size: Size {
                width: 180.0,
                height: 72.0,
            },
            members: keys.iter().map(|&key| member(key, true, true)).collect(),
            call_edges: keys
                .iter()
                .flat_map(|&from| {
                    keys.iter()
                        .filter(move |&&to| to != from)
                        .map(move |&to| CallEdge {
                            from,
                            to,
                            relation: EdgeRelation::Call,
                        })
                })
                .collect(),
        }
    }

    #[test]
    fn physics_layout_preserves_primary_flow_and_clearance_for_a_dense_cycle() {
        let team = complete_team(&[100, 101, 102, 103, 104, 105]);
        let layout = layout_team(&team);
        assert_eq!(layout.kind, LayoutKind::PrimaryOrbit);

        let primary = layout.positions[&team.primary_key];
        let primary_center = Point {
            x: primary.x + team.primary_size.width * 0.5,
            y: primary.y + team.primary_size.height * 0.5,
        };
        let mut centers = Vec::new();
        for node in &team.members {
            let point = layout.positions[&node.key];
            centers.push((
                node.key,
                Point {
                    x: point.x + node.size.width * 0.5,
                    y: point.y + node.size.height * 0.5,
                },
            ));
        }
        assert!(centers
            .iter()
            .all(|(_, center)| center.y > primary_center.y));

        let rect = |key: u64, size: Size| {
            let point = layout.positions[&key];
            (
                point.x - NODE_GAP * 0.5,
                point.y - NODE_GAP * 0.5,
                point.x + size.width + NODE_GAP * 0.5,
                point.y + size.height + NODE_GAP * 0.5,
            )
        };
        let mut boxes = vec![(team.primary_key, rect(team.primary_key, team.primary_size))];
        boxes.extend(
            team.members
                .iter()
                .map(|node| (node.key, rect(node.key, node.size))),
        );
        for (index, &(a_key, a)) in boxes.iter().enumerate() {
            for &(b_key, b) in &boxes[index + 1..] {
                let overlaps = a.0 < b.2 && b.0 < a.2 && a.1 < b.3 && b.1 < a.3;
                assert!(!overlaps, "{a_key} overlaps {b_key}");
            }
        }
    }

    #[test]
    fn layout_is_invariant_under_input_permutation() {
        let a = layout_team(&complete_team(&[100, 101, 102, 103, 104, 105]));
        let b = layout_team(&complete_team(&[104, 101, 105, 100, 103, 102]));
        assert_eq!(a.kind, b.kind);
        assert_eq!(a.positions, b.positions);
        assert_eq!(a.colors, b.colors);
        assert_eq!(a.components, b.components);
    }

    #[test]
    fn peer_endpoint_order_cannot_create_a_hidden_authority_rank() {
        let base = Team {
            primary_key: 1,
            primary_size: Size {
                width: 180.0,
                height: 72.0,
            },
            members: vec![
                member(100, true, false),
                member(101, true, false),
                member(102, true, false),
            ],
            call_edges: vec![
                CallEdge {
                    from: 100,
                    to: 101,
                    relation: EdgeRelation::Peer,
                },
                CallEdge {
                    from: 101,
                    to: 102,
                    relation: EdgeRelation::Peer,
                },
            ],
        };
        let mut reversed = base.clone();
        for edge in &mut reversed.call_edges {
            std::mem::swap(&mut edge.from, &mut edge.to);
        }

        let a = layout_team(&base);
        let b = layout_team(&reversed);
        assert_eq!(a.positions, b.positions);
        assert_eq!(a.components, b.components);
        assert_eq!(a.colors, b.colors);
    }

    #[test]
    fn boundary_terminal_forms_an_unoccluded_cycle_beside_a_two_member_orbit() {
        let team = complete_team(&[100, 101]);
        let layout = layout_team(&team);
        let terminal_size = Size {
            width: 180.0,
            height: 72.0,
        };
        let terminal = place_boundary_terminal(&team, &layout, terminal_size);
        let primary = center(layout.positions[&team.primary_key], team.primary_size);
        let terminal_center = center(terminal, terminal_size);
        let members: Vec<_> = team
            .members
            .iter()
            .map(|member| center(layout.positions[&member.key], member.size))
            .collect();

        assert!(
            terminal_center.x < primary.x,
            "input-side tie-break was not left"
        );
        assert!((terminal_center.y - primary.y).abs() < 0.001);
        let core: Vec<_> = std::iter::once(CoreNode {
            key: team.primary_key,
            center: primary,
            size: team.primary_size,
            agent: false,
        })
        .chain(
            team.members
                .iter()
                .zip(members.iter())
                .map(|(member, center)| CoreNode {
                    key: member.key,
                    center: *center,
                    size: member.size,
                    agent: member.roles.agent,
                }),
        )
        .collect();
        assert!(core[1..].iter().all(|node| !segment_intersects_node(
            terminal_center,
            primary,
            node
        )));
        for agent in &core[1..] {
            assert!(core
                .iter()
                .filter(|node| node.key != agent.key)
                .all(|node| { !segment_intersects_node(agent.center, terminal_center, node) }));
        }
    }

    #[test]
    fn boundary_terminal_keeps_layered_flow_cycles_off_the_primary_axis() {
        let team = Team {
            primary_key: 1,
            primary_size: Size {
                width: 180.0,
                height: 72.0,
            },
            members: vec![member(100, true, false), member(101, true, false)],
            call_edges: vec![],
        };
        let layout = layout_team(&team);
        assert_eq!(layout.kind, LayoutKind::LayeredModules);
        let terminal_size = Size {
            width: 180.0,
            height: 72.0,
        };
        let terminal = place_boundary_terminal(&team, &layout, terminal_size);
        let primary = center(layout.positions[&team.primary_key], team.primary_size);
        let terminal_center = center(terminal, terminal_size);
        assert!((terminal_center.x - primary.x).abs() > 1.0);
        for member in &team.members {
            let agent = CoreNode {
                key: member.key,
                center: center(layout.positions[&member.key], member.size),
                size: member.size,
                agent: true,
            };
            let primary_node = CoreNode {
                key: team.primary_key,
                center: primary,
                size: team.primary_size,
                agent: false,
            };
            assert!(!segment_intersects_node(
                agent.center,
                terminal_center,
                &primary_node
            ));
        }
    }

    #[test]
    fn one_agent_does_not_collapse_request_and_return_into_one_line() {
        let team = Team {
            primary_key: 1,
            primary_size: Size {
                width: 180.0,
                height: 72.0,
            },
            members: vec![member(100, true, false)],
            call_edges: vec![],
        };
        let layout = layout_team(&team);
        let terminal_size = Size {
            width: 180.0,
            height: 72.0,
        };
        let terminal = place_boundary_terminal(&team, &layout, terminal_size);
        let primary = center(layout.positions[&team.primary_key], team.primary_size);
        let agent = center(layout.positions[&100], team.members[0].size);
        let terminal = center(terminal, terminal_size);
        let cross = (agent.x - primary.x) * (terminal.y - primary.y)
            - (agent.y - primary.y) * (terminal.x - primary.x);
        assert!(cross.abs() > 1.0, "the visible flow cycle is collinear");
        let primary_node = CoreNode {
            key: team.primary_key,
            center: primary,
            size: team.primary_size,
            agent: false,
        };
        assert!(!segment_intersects_node(agent, terminal, &primary_node));
    }

    #[test]
    fn boundary_terminal_is_invariant_under_member_input_permutation() {
        let a_team = complete_team(&[100, 101, 102, 103, 104, 105]);
        let b_team = complete_team(&[104, 101, 105, 100, 103, 102]);
        let a_layout = layout_team(&a_team);
        let b_layout = layout_team(&b_team);
        let terminal_size = Size {
            width: 180.0,
            height: 72.0,
        };
        assert_eq!(
            place_boundary_terminal(&a_team, &a_layout, terminal_size),
            place_boundary_terminal(&b_team, &b_layout, terminal_size)
        );
    }

    #[test]
    fn boundary_terminal_keeps_request_path_clear_for_a_dense_cycle() {
        let team = complete_team(&[100, 101, 102, 103, 104, 105]);
        let layout = layout_team(&team);
        let terminal_size = Size {
            width: 180.0,
            height: 72.0,
        };
        let terminal = place_boundary_terminal(&team, &layout, terminal_size);
        let primary = center(layout.positions[&team.primary_key], team.primary_size);
        let terminal = center(terminal, terminal_size);
        for member in &team.members {
            let obstacle = CoreNode {
                key: member.key,
                center: center(layout.positions[&member.key], member.size),
                size: member.size,
                agent: true,
            };
            assert!(!segment_intersects_node(terminal, primary, &obstacle));
        }
    }

    #[test]
    fn terminal_has_the_required_geometric_clearance_from_every_core_rectangle() {
        let terminal_size = Size {
            width: 181.0,
            height: 73.0,
        };
        for count in 1..=8 {
            let keys: Vec<_> = (0..count).map(|index| 100 + index as u64).collect();
            let mut team = complete_team(&keys);
            for (index, member) in team.members.iter_mut().enumerate() {
                member.size.width += index as f32 * 7.0;
                member.size.height += index as f32 * 3.0;
            }
            let layout = layout_team(&team);
            let terminal = center(
                place_boundary_terminal(&team, &layout, terminal_size),
                terminal_size,
            );
            let core = std::iter::once((
                center(layout.positions[&team.primary_key], team.primary_size),
                team.primary_size,
            ))
            .chain(team.members.iter().map(|member| {
                (
                    center(layout.positions[&member.key], member.size),
                    member.size,
                )
            }));
            for (core_center, core_size) in core {
                let dx = ((terminal.x - core_center.x).abs()
                    - (terminal_size.width + core_size.width) * 0.5)
                    .max(0.0);
                let dy = ((terminal.y - core_center.y).abs()
                    - (terminal_size.height + core_size.height) * 0.5)
                    .max(0.0);
                assert!(
                    dx.hypot(dy) >= NODE_GAP - 0.001,
                    "terminal clearance failed for orbit size {count}"
                );
            }
        }
    }

    #[test]
    fn tarjan_matches_mutual_reachability_for_every_four_node_digraph() {
        let n = 4;
        let possible: Vec<_> = (0..n)
            .flat_map(|from| {
                (0..n)
                    .filter(move |&to| to != from)
                    .map(move |to| (from, to))
            })
            .collect();
        for mask in 0usize..(1usize << possible.len()) {
            let edges: Vec<_> = possible
                .iter()
                .enumerate()
                .filter(|(bit, _)| mask & (1 << bit) != 0)
                .map(|(_, &edge)| edge)
                .collect();
            let components = strongly_connected_components(n, &edges);
            let component_of: BTreeMap<_, _> = components
                .iter()
                .enumerate()
                .flat_map(|(component, members)| {
                    members.iter().map(move |&member| (member, component))
                })
                .collect();
            let mut reachable = vec![vec![false; n]; n];
            for (node, row) in reachable.iter_mut().enumerate() {
                row[node] = true;
            }
            for &(from, to) in &edges {
                reachable[from][to] = true;
            }
            for via in 0..n {
                for from in 0..n {
                    for to in 0..n {
                        reachable[from][to] |= reachable[from][via] && reachable[via][to];
                    }
                }
            }
            for a in 0..n {
                for b in 0..n {
                    assert_eq!(
                        component_of[&a] == component_of[&b],
                        reachable[a][b] && reachable[b][a],
                        "mask={mask:#x}, pair=({a},{b})"
                    );
                }
            }
        }
    }

    #[test]
    fn condensation_ranks_are_acyclic_and_monotone() {
        let edges = BTreeSet::from([(0, 1), (0, 2), (1, 3), (2, 3)]);
        let ranks = condensation_ranks(4, &edges);
        assert_eq!(ranks, vec![0, 1, 1, 2]);
        for (from, to) in edges {
            assert!(ranks[from] < ranks[to]);
        }
    }

    #[test]
    fn deterministic_circular_search_finds_zero_crossing_order() {
        // The canonical order makes the two independent chords cross:
        // (0, 2) and (1, 3). Another circular order separates them.
        let members = vec![
            member(10, true, true),
            member(20, true, true),
            member(30, true, true),
            member(40, true, true),
        ];
        let edges = vec![(0, 2), (1, 3)];
        assert_eq!(circular_crossings(&[0, 1, 2, 3], &edges), 1);
        let order = optimized_orbit_order(&[0, 1, 2, 3], &members, &edges);
        assert_eq!(circular_crossings(&order, &edges), 0);
    }

    #[test]
    fn circular_optimization_has_no_member_count_discontinuity() {
        let members: Vec<_> = (0..12)
            .map(|index| member(100 + index as u64, true, true))
            .collect();
        let edges = vec![
            (0, 6),
            (1, 7),
            (2, 8),
            (3, 9),
            (4, 10),
            (5, 11),
            (0, 7),
            (2, 9),
            (4, 11),
        ];
        let canonical: Vec<_> = (0..members.len()).collect();
        let optimized = optimized_orbit_order(&canonical, &members, &edges);
        assert_eq!(optimized.len(), canonical.len());
        assert!(
            circular_crossings(&optimized, &edges) < circular_crossings(&canonical, &edges),
            "all-size optimizer did not improve the twelve-member case"
        );

        let mut permuted = canonical.clone();
        permuted.rotate_left(5);
        permuted.reverse();
        assert_eq!(
            optimized_orbit_order(&permuted, &members, &edges),
            optimized,
            "optimization must depend on graph structure and stable keys, not input order"
        );
    }
}
