package tui

import (
	"sort"
	"strings"
	"unicode"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/rivo/uniseg"
)

type screenPoint struct {
	X int
	Y int
}

type screenSelection struct {
	anchor   screenPoint
	head     screenPoint
	active   bool
	dragging bool
}

func (selection *screenSelection) begin(point screenPoint) {
	selection.anchor = point
	selection.head = point
	selection.active = true
	selection.dragging = true
}

func (selection *screenSelection) extend(point screenPoint) {
	selection.head = point
}

func (selection *screenSelection) clear() {
	*selection = screenSelection{}
}

func (selection screenSelection) bounds() (screenPoint, screenPoint) {
	if pointBefore(selection.head, selection.anchor) {
		return selection.head, selection.anchor
	}
	return selection.anchor, selection.head
}

func pointBefore(left, right screenPoint) bool {
	return left.Y < right.Y || left.Y == right.Y && left.X < right.X
}

type selectionCell struct {
	x            int
	width        int
	semanticFrom int
	semanticTo   int
}

type selectionLine struct {
	line  int
	cells []selectionCell
}

type selectionBlock struct {
	text           string
	lines          []selectionLine
	screenRelative bool
}

func newSelectionBlock(startLine int, rendered, semantic string) selectionBlock {
	return newSelectionBlockIndented(startLine, rendered, semantic, 0)
}

func newSelectionBlockIndented(startLine int, rendered, semantic string, indent int) selectionBlock {
	semantic = sanitizeTerminalText(semantic)
	block := selectionBlock{text: semantic}
	if semantic == "" {
		return block
	}
	searchFrom := 0
	for offset, rawLine := range strings.Split(strings.TrimRight(rendered, "\n"), "\n") {
		plain := ansi.Strip(rawLine)
		line := selectionLine{line: startLine + offset}
		x := 0
		contentEnd := ansi.StringWidth(strings.TrimRightFunc(plain, unicode.IsSpace))
		graphemes := uniseg.NewGraphemes(plain)
		for graphemes.Next() {
			grapheme := graphemes.Str()
			width := max(1, graphemes.Width())
			if x < indent || x >= contentEnd {
				x += width
				continue
			}
			index := -1
			if strings.TrimSpace(grapheme) == "" {
				if strings.HasPrefix(semantic[searchFrom:], grapheme) {
					index = 0
				}
			} else {
				index = strings.Index(semantic[searchFrom:], grapheme)
			}
			if index < 0 {
				x += width
				continue
			}
			from := searchFrom + index
			to := from + len(grapheme)
			line.cells = append(line.cells, selectionCell{
				x: x, width: width, semanticFrom: from, semanticTo: to,
			})
			searchFrom = to
			x += width
		}
		if len(line.cells) > 0 {
			block.lines = append(block.lines, line)
		}
	}
	return block
}

// semanticTextFromRendered removes only chrome ALT itself emitted. It is not a
// language heuristic: every removed prefix or border is part of this TUI's
// rendering contract, so prose and command output remain untouched.
func semanticTextFromRendered(rendered string) string {
	var result []string
	for _, raw := range strings.Split(ansi.Strip(rendered), "\n") {
		line := strings.TrimRightFunc(raw, unicode.IsSpace)
		if strings.Trim(line, " ─╭╮╰╯") == "" {
			continue
		}
		if strings.HasPrefix(line, "│ ") && strings.HasSuffix(line, " │") {
			line = strings.TrimSuffix(strings.TrimPrefix(line, "│ "), " │")
			line = strings.TrimRightFunc(line, unicode.IsSpace)
		}
		for _, prefix := range []string{"• ", "■ ", "  ↳ ", "  └ ", "└ "} {
			if strings.HasPrefix(line, prefix) {
				line = strings.TrimPrefix(line, prefix)
				break
			}
		}
		if strings.HasPrefix(line, "─ ") {
			line = strings.TrimPrefix(line, "─ ")
			line = strings.TrimRight(line, " ─")
		}
		if line != "" {
			result = append(result, line)
		}
	}
	return strings.Join(result, "\n")
}

type selectedFragment struct {
	line int
	from int
	to   int
	text string
}

func (m Model) selectedText() string {
	if !m.selection.active || m.selection.anchor == m.selection.head {
		return ""
	}
	start, end := m.selection.bounds()
	offset := m.viewport.YOffset()
	blocks := m.selectionBlocksForView()
	fragments := make([]selectedFragment, 0, len(blocks))
	for _, block := range blocks {
		from := -1
		to := -1
		firstScreenLine := 0
		for _, line := range block.lines {
			screenLine := line.line
			limit := m.height
			if !block.screenRelative {
				screenLine -= offset
				limit = m.viewport.Height()
			}
			if screenLine < 0 || screenLine >= limit {
				continue
			}
			for _, cell := range line.cells {
				if !cellSelected(screenPoint{X: cell.x, Y: screenLine}, start, end) {
					continue
				}
				if from < 0 {
					from = cell.semanticFrom
					firstScreenLine = screenLine
				}
				to = cell.semanticTo
			}
		}
		if from >= 0 && to > from {
			fragments = append(fragments, selectedFragment{
				line: firstScreenLine, from: from, to: to, text: block.text[from:to],
			})
		}
	}
	if len(fragments) == 0 {
		return ""
	}
	sort.SliceStable(fragments, func(i, j int) bool {
		if fragments[i].line != fragments[j].line {
			return fragments[i].line < fragments[j].line
		}
		return fragments[i].from < fragments[j].from
	})
	parts := make([]string, 0, len(fragments))
	for _, fragment := range fragments {
		value := fragment.text
		if strings.TrimSpace(value) != "" {
			parts = append(parts, value)
		}
	}
	return strings.Join(parts, "\n")
}

func cellSelected(point, start, end screenPoint) bool {
	if point.Y < start.Y || point.Y > end.Y {
		return false
	}
	if start.Y == end.Y {
		return point.X >= start.X && point.X <= end.X
	}
	if point.Y == start.Y {
		return point.X >= start.X
	}
	if point.Y == end.Y {
		return point.X <= end.X
	}
	return true
}

func (m Model) highlightSelection(content string) string {
	start, end := m.selection.bounds()
	lines := strings.Split(content, "\n")
	offset := m.viewport.YOffset()
	style := lipgloss.NewStyle().Reverse(true)
	boundsByLine := make(map[int][2]int)
	for _, block := range m.selectionBlocksForView() {
		for _, line := range block.lines {
			screenLine := line.line
			limit := m.height
			if !block.screenRelative {
				screenLine -= offset
				limit = m.viewport.Height()
			}
			if screenLine < 0 || screenLine >= limit || screenLine >= len(lines) {
				continue
			}
			for _, cell := range line.cells {
				if cellSelected(screenPoint{X: cell.x, Y: screenLine}, start, end) {
					bounds, found := boundsByLine[screenLine]
					if !found {
						bounds = [2]int{cell.x, cell.x + cell.width}
					} else {
						bounds[0] = min(bounds[0], cell.x)
						bounds[1] = max(bounds[1], cell.x+cell.width)
					}
					boundsByLine[screenLine] = bounds
				}
			}
		}
	}
	for line, bounds := range boundsByLine {
		lines[line] = lipgloss.StyleRanges(
			lines[line], lipgloss.NewRange(bounds[0], bounds[1], style),
		)
	}
	return strings.Join(lines, "\n")
}

func (m Model) selectionBlocksForView() []selectionBlock {
	blocks := append([]selectionBlock(nil), m.selectionBlocks...)
	if m.profilePicker || m.picker != nil || m.shortcuts {
		return blocks
	}
	value := m.input.Value()
	if value == "" {
		return blocks
	}
	width := max(1, m.width)
	composer := userSurfaceStyle(m.background).
		Width(width).
		Padding(1, 0).
		Render(m.input.View())
	screenLine := m.viewport.Height()
	if m.slashPopup != nil {
		screenLine += m.slashPopup.Height()
	}
	if m.composerNotice != "" {
		screenLine++
	}
	block := newSelectionBlockIndented(screenLine, composer, value, 2)
	block.screenRelative = true
	blocks = append(blocks, block)
	return blocks
}
