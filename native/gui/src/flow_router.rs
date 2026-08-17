//! Deterministic routing for the live execution graph.
//!
//! The Team layout is free/radial, so an edge cannot inherit a global
//! top-to-bottom socket direction.  This module treats nodes as rectangular
//! obstacles, assigns every incident edge a distinct free port on the complete
//! boundary, and routes between short outward-facing endpoint extensions.  Direction
//! remains a property of the edge; it is not encoded by an arbitrary node side.

use eframe::egui;
use egui_graph::NodeId;
use std::collections::{BTreeMap, HashMap};

#[derive(Clone, Debug)]
pub(crate) struct FlowNode {
    pub(crate) id: NodeId,
    pub(crate) rect: egui::Rect,
}

#[derive(Clone, Debug)]
pub(crate) struct FlowEdge {
    pub(crate) key: String,
    pub(crate) from: NodeId,
    pub(crate) to: NodeId,
}

#[derive(Clone, Debug, Default)]
pub(crate) struct FlowRoutes {
    paths: HashMap<String, Vec<egui::Pos2>>,
}

impl FlowRoutes {
    pub(crate) fn path(&self, key: &str) -> Option<&[egui::Pos2]> {
        self.paths.get(key).map(Vec::as_slice)
    }
}

#[derive(Clone, Copy, Debug, Eq, Ord, PartialEq, PartialOrd)]
enum Side {
    Top,
    Right,
    Bottom,
    Left,
}

impl Side {
    fn normal(self) -> egui::Vec2 {
        match self {
            Self::Top => egui::vec2(0.0, -1.0),
            Self::Right => egui::vec2(1.0, 0.0),
            Self::Bottom => egui::vec2(0.0, 1.0),
            Self::Left => egui::vec2(-1.0, 0.0),
        }
    }

    fn interval(self, rect: egui::Rect, corner_clearance: f32) -> (f32, f32) {
        let (lo, hi) = match self {
            Self::Top | Self::Bottom => (rect.min.x, rect.max.x),
            Self::Left | Self::Right => (rect.min.y, rect.max.y),
        };
        let inset = corner_clearance.min((hi - lo).max(0.0) * 0.5);
        (lo + inset, hi - inset)
    }

    fn point(self, rect: egui::Rect, cross: f32) -> egui::Pos2 {
        match self {
            Self::Top => egui::pos2(cross, rect.min.y),
            Self::Right => egui::pos2(rect.max.x, cross),
            Self::Bottom => egui::pos2(cross, rect.max.y),
            Self::Left => egui::pos2(rect.min.x, cross),
        }
    }
}

#[derive(Clone, Copy, Debug)]
struct Port {
    point: egui::Pos2,
    normal: egui::Vec2,
}

#[derive(Clone, Debug)]
struct PortRequest {
    edge: String,
    source: bool,
    ideal: f32,
}

/// Route all edges at once so port allocation is independent of map iteration
/// and competing edges cannot be assigned the same boundary position.
pub(crate) fn route_flow_edges(
    nodes: &[FlowNode],
    edges: &[FlowEdge],
    clearance: f32,
) -> FlowRoutes {
    let rects: HashMap<_, _> = nodes.iter().map(|node| (node.id, node.rect)).collect();
    let ports = assign_ports(&rects, edges, clearance.max(0.0));
    let mut paths = HashMap::new();
    let extension = (clearance * 1.5).max(18.0);
    let rounding = (clearance * 0.75).max(8.0);

    let mut stable_edges = edges.to_vec();
    stable_edges.sort_by(|a, b| a.key.cmp(&b.key));
    for edge in stable_edges {
        let Some(&(source, target)) = ports.get(&edge.key) else {
            continue;
        };
        let source_extension = source.point + source.normal * extension;
        let target_extension = target.point + target.normal * extension;
        let mut spine = vec![source.point, source_extension];
        let middle = route_visibility(
            source_extension,
            target_extension,
            edge.from,
            edge.to,
            nodes,
            clearance + rounding,
        );
        spine.extend(middle.into_iter().skip(1));
        if spine.last().copied() != Some(target_extension) {
            spine.push(target_extension);
        }
        spine.push(target.point);
        simplify_polyline(&mut spine);
        paths.insert(edge.key, rounded_polyline(&spine, rounding));
    }
    FlowRoutes { paths }
}

fn assign_ports(
    rects: &HashMap<NodeId, egui::Rect>,
    edges: &[FlowEdge],
    clearance: f32,
) -> HashMap<String, (Port, Port)> {
    let mut groups: BTreeMap<(NodeId, Side), Vec<PortRequest>> = BTreeMap::new();
    for edge in edges {
        let (Some(&from), Some(&to)) = (rects.get(&edge.from), rects.get(&edge.to)) else {
            continue;
        };
        let (source_side, source_ideal) = boundary_intent(from, to.center());
        let (target_side, target_ideal) = boundary_intent(to, from.center());
        groups
            .entry((edge.from, source_side))
            .or_default()
            .push(PortRequest {
                edge: edge.key.clone(),
                source: true,
                ideal: source_ideal,
            });
        groups
            .entry((edge.to, target_side))
            .or_default()
            .push(PortRequest {
                edge: edge.key.clone(),
                source: false,
                ideal: target_ideal,
            });
    }

    let mut endpoints: HashMap<String, (Option<Port>, Option<Port>)> = HashMap::new();
    for ((node, side), mut requests) in groups {
        let Some(&rect) = rects.get(&node) else {
            continue;
        };
        requests.sort_by(|a, b| {
            a.ideal
                .total_cmp(&b.ideal)
                .then_with(|| a.edge.cmp(&b.edge))
                .then_with(|| a.source.cmp(&b.source))
        });
        let (lo, hi) = side.interval(rect, clearance.max(8.0));
        let ideals: Vec<_> = requests.iter().map(|request| request.ideal).collect();
        let positions = project_ordered_positions(&ideals, lo, hi, clearance.max(12.0));
        for (request, cross) in requests.into_iter().zip(positions) {
            let port = Port {
                point: side.point(rect, cross),
                normal: side.normal(),
            };
            let pair = endpoints.entry(request.edge).or_insert((None, None));
            if request.source {
                pair.0 = Some(port);
            } else {
                pair.1 = Some(port);
            }
        }
    }
    endpoints
        .into_iter()
        .filter_map(|(key, (source, target))| Some((key, (source?, target?))))
        .collect()
}

/// Exact Euclidean projection of ordered ideal positions onto a one-dimensional
/// boundary with a minimum gap.  Subtracting `i * gap` turns the constraint
/// into isotonic regression; pooled-adjacent-violators gives the least-squares
/// optimum, after which the common feasible interval is clamped.
fn project_ordered_positions(ideals: &[f32], lo: f32, hi: f32, requested_gap: f32) -> Vec<f32> {
    if ideals.is_empty() {
        return Vec::new();
    }
    if ideals.len() == 1 {
        return vec![ideals[0].clamp(lo, hi)];
    }
    let gap = requested_gap
        .max(0.0)
        .min(((hi - lo) / (ideals.len() - 1) as f32).max(0.0));
    let upper = hi - gap * (ideals.len() - 1) as f32;

    #[derive(Clone, Copy)]
    struct Block {
        start: usize,
        end: usize,
        sum: f32,
    }
    impl Block {
        fn mean(self) -> f32 {
            self.sum / (self.end - self.start) as f32
        }
    }

    let mut blocks: Vec<Block> = Vec::with_capacity(ideals.len());
    for (index, ideal) in ideals.iter().copied().enumerate() {
        blocks.push(Block {
            start: index,
            end: index + 1,
            sum: ideal - index as f32 * gap,
        });
        while blocks.len() >= 2 {
            let n = blocks.len();
            if blocks[n - 2].mean() <= blocks[n - 1].mean() {
                break;
            }
            let right = blocks.pop().expect("right isotonic block");
            let left = blocks.pop().expect("left isotonic block");
            blocks.push(Block {
                start: left.start,
                end: right.end,
                sum: left.sum + right.sum,
            });
        }
    }

    let mut projected = vec![0.0; ideals.len()];
    for block in blocks {
        let value = block.mean().clamp(lo, upper);
        for (index, slot) in projected[block.start..block.end].iter_mut().enumerate() {
            let global = block.start + index;
            *slot = value + global as f32 * gap;
        }
    }
    projected
}

fn boundary_intent(rect: egui::Rect, toward: egui::Pos2) -> (Side, f32) {
    let center = rect.center();
    let direction = toward - center;
    if direction.length_sq() <= f32::EPSILON {
        return (Side::Top, center.x);
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
    if x_scale < y_scale {
        let side = if direction.x > 0.0 {
            Side::Right
        } else {
            Side::Left
        };
        let point = center + direction * x_scale;
        (side, point.y)
    } else {
        let side = if direction.y > 0.0 {
            Side::Bottom
        } else {
            Side::Top
        };
        let point = center + direction * y_scale;
        (side, point.x)
    }
}

fn route_visibility(
    from: egui::Pos2,
    to: egui::Pos2,
    from_id: NodeId,
    to_id: NodeId,
    nodes: &[FlowNode],
    clearance: f32,
) -> Vec<egui::Pos2> {
    let mut points = vec![from, to];
    let pass_limit = nodes.len().saturating_mul(4).max(1);
    for _ in 0..pass_limit {
        let obstruction = points.windows(2).enumerate().find_map(|(segment, pair)| {
            nodes
                .iter()
                .filter(|node| node.id != from_id && node.id != to_id)
                .filter_map(|node| {
                    let expanded = node.rect.expand(clearance);
                    segment_rect_entry(pair[0], pair[1], expanded)
                        .map(|entry| (entry, node.id, expanded))
                })
                .min_by(|a, b| a.0.total_cmp(&b.0).then_with(|| a.1.cmp(&b.1)))
                .map(|(_, _, rect)| (segment, rect))
        });
        let Some((segment, blocker)) = obstruction else {
            break;
        };
        let a = points[segment];
        let b = points[segment + 1];
        let corners = [
            blocker.left_top(),
            blocker.right_top(),
            blocker.right_bottom(),
            blocker.left_bottom(),
        ];
        let interior = blocker.shrink(0.5);
        let mut candidates = Vec::new();
        for side in 0..4 {
            let p = corners[side];
            let q = corners[(side + 1) % 4];
            for (first, second) in [(p, q), (q, p)] {
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
        let Some((_, first, second)) = candidates.into_iter().min_by(|a, b| {
            a.0.total_cmp(&b.0)
                .then_with(|| a.1.x.total_cmp(&b.1.x))
                .then_with(|| a.1.y.total_cmp(&b.1.y))
                .then_with(|| a.2.x.total_cmp(&b.2.x))
                .then_with(|| a.2.y.total_cmp(&b.2.y))
        }) else {
            break;
        };
        points.splice(segment + 1..segment + 1, [first, second]);
        simplify_polyline(&mut points);
    }
    points
}

fn rounded_polyline(points: &[egui::Pos2], radius: f32) -> Vec<egui::Pos2> {
    if points.len() < 3 {
        return points.to_vec();
    }
    let mut result = vec![points[0]];
    for index in 1..points.len() - 1 {
        let previous = points[index - 1];
        let corner = points[index];
        let next = points[index + 1];
        let incoming = corner - previous;
        let outgoing = next - corner;
        if incoming.length_sq() <= f32::EPSILON || outgoing.length_sq() <= f32::EPSILON {
            continue;
        }
        let in_dir = incoming.normalized();
        let out_dir = outgoing.normalized();
        if in_dir.dot(out_dir) > 0.9999 {
            continue;
        }
        let cut = radius
            .min(incoming.length() * 0.3)
            .min(outgoing.length() * 0.3);
        let enter = corner - in_dir * cut;
        let leave = corner + out_dir * cut;
        if result.last().copied() != Some(enter) {
            result.push(enter);
        }
        // Quadratic Bézier with `corner` as its control point. Four samples
        // are sufficient at this bounded radius and retain exact endpoint
        // tangents along the adjacent straight segments.
        for sample in 1..=4 {
            let t = sample as f32 / 4.0;
            let one_t = 1.0 - t;
            let value = enter.to_vec2() * one_t * one_t
                + corner.to_vec2() * 2.0 * one_t * t
                + leave.to_vec2() * t * t;
            result.push(egui::pos2(value.x, value.y));
        }
    }
    if result.last().copied() != points.last().copied() {
        result.push(*points.last().expect("non-empty polyline"));
    }
    result
}

fn simplify_polyline(points: &mut Vec<egui::Pos2>) {
    points.dedup_by(|a, b| (*a - *b).length_sq() <= 1e-6);
    let mut index = 1;
    while index + 1 < points.len() {
        let a = points[index] - points[index - 1];
        let b = points[index + 1] - points[index];
        let cross = a.x * b.y - a.y * b.x;
        if cross.abs() <= 1e-4 && a.dot(b) >= 0.0 {
            points.remove(index);
        } else {
            index += 1;
        }
    }
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

#[cfg(test)]
mod tests {
    use super::*;

    fn rect(center: egui::Pos2) -> egui::Rect {
        egui::Rect::from_center_size(center, egui::vec2(180.0, 90.0))
    }

    fn assert_endpoint_departure(path: &[egui::Pos2], source: egui::Rect, target: egui::Rect) {
        assert!(path.len() >= 2);
        assert!(source.contains(path[0]));
        assert!(target.contains(*path.last().expect("target endpoint")));
        assert!(
            !source.shrink(0.01).contains(path[1]),
            "route did not immediately leave its source: {path:?}"
        );
        assert!(
            !target
                .shrink(0.01)
                .contains(path[path.len().saturating_sub(2)]),
            "route approached its target through the interior: {path:?}"
        );
        for point in &path[1..path.len() - 1] {
            assert!(!source.shrink(0.01).contains(*point));
            assert!(!target.shrink(0.01).contains(*point));
        }
    }

    #[test]
    fn free_ports_depart_through_the_boundary_in_every_quadrant() {
        let source_id = NodeId::from_u64(1);
        let source = rect(egui::Pos2::ZERO);
        for (index, target_center) in [
            egui::pos2(320.0, -240.0),
            egui::pos2(320.0, 240.0),
            egui::pos2(-320.0, 240.0),
            egui::pos2(-320.0, -240.0),
        ]
        .into_iter()
        .enumerate()
        {
            let target_id = NodeId::from_u64(index as u64 + 2);
            let target = rect(target_center);
            let routes = route_flow_edges(
                &[
                    FlowNode {
                        id: source_id,
                        rect: source,
                    },
                    FlowNode {
                        id: target_id,
                        rect: target,
                    },
                ],
                &[FlowEdge {
                    key: "edge".to_owned(),
                    from: source_id,
                    to: target_id,
                }],
                16.0,
            );
            assert_endpoint_departure(routes.path("edge").expect("edge route"), source, target);
        }
    }

    #[test]
    fn reported_primary_to_engineering_edge_uses_facing_sides_without_a_loop() {
        let primary_id = NodeId::from_u64(1);
        let engineering_id = NodeId::from_u64(2);
        let primary = egui::Rect::from_min_size(egui::pos2(400.0, 373.0), egui::vec2(180.0, 98.0));
        let engineering =
            egui::Rect::from_min_size(egui::pos2(367.0, 60.0), egui::vec2(210.0, 96.0));
        let routes = route_flow_edges(
            &[
                FlowNode {
                    id: primary_id,
                    rect: primary,
                },
                FlowNode {
                    id: engineering_id,
                    rect: engineering,
                },
            ],
            &[FlowEdge {
                key: "flow:route".to_owned(),
                from: primary_id,
                to: engineering_id,
            }],
            16.0,
        );
        let path = routes.path("flow:route").expect("route");
        assert_eq!(path.first().expect("source").y, primary.top());
        assert_eq!(path.last().expect("target").y, engineering.bottom());
        assert!(
            path[1].y < path[0].y,
            "edge left Primary in the wrong direction"
        );
        assert!(
            path[path.len() - 2].y > path[path.len() - 1].y,
            "edge entered engineering through the wrong direction"
        );
        assert!(path.windows(2).all(|segment| segment[1].y <= segment[0].y));
        assert_endpoint_departure(path, primary, engineering);
    }

    #[test]
    fn competing_incident_edges_receive_distinct_ports_independent_of_input_order() {
        let hub = NodeId::from_u64(1);
        let a = NodeId::from_u64(2);
        let b = NodeId::from_u64(3);
        let nodes = [
            FlowNode {
                id: hub,
                rect: rect(egui::Pos2::ZERO),
            },
            FlowNode {
                id: a,
                rect: rect(egui::pos2(300.0, -12.0)),
            },
            FlowNode {
                id: b,
                rect: rect(egui::pos2(300.0, 12.0)),
            },
        ];
        let edges = vec![
            FlowEdge {
                key: "a".to_owned(),
                from: hub,
                to: a,
            },
            FlowEdge {
                key: "b".to_owned(),
                from: hub,
                to: b,
            },
        ];
        let first = route_flow_edges(&nodes, &edges, 16.0);
        let mut reversed = edges.clone();
        reversed.reverse();
        let second = route_flow_edges(&nodes, &reversed, 16.0);
        assert_ne!(first.path("a").unwrap()[0], first.path("b").unwrap()[0]);
        assert_eq!(first.path("a"), second.path("a"));
        assert_eq!(first.path("b"), second.path("b"));
    }

    #[test]
    fn routed_path_avoids_an_unrelated_rectangle_with_clearance() {
        let a = NodeId::from_u64(1);
        let blocker = NodeId::from_u64(2);
        let b = NodeId::from_u64(3);
        let blocker_rect = rect(egui::Pos2::ZERO);
        let nodes = [
            FlowNode {
                id: a,
                rect: rect(egui::pos2(-360.0, 0.0)),
            },
            FlowNode {
                id: blocker,
                rect: blocker_rect,
            },
            FlowNode {
                id: b,
                rect: rect(egui::pos2(360.0, 0.0)),
            },
        ];
        let routes = route_flow_edges(
            &nodes,
            &[FlowEdge {
                key: "edge".to_owned(),
                from: a,
                to: b,
            }],
            16.0,
        );
        let forbidden = blocker_rect.expand(15.0);
        let path = routes.path("edge").expect("edge route");
        assert!(path
            .windows(2)
            .all(|segment| { segment_rect_entry(segment[0], segment[1], forbidden).is_none() }));
    }
}
