package tui

import (
	"os"
	"regexp"
	"strconv"
	"strings"

	"altv1/internal/content"
	"altv1/internal/nativegui"

	tea "charm.land/bubbletea/v2"
)

var imagePlaceholderPattern = regexp.MustCompile(`\[Image #(\d+)\]`)

type clipboardImageMsg struct {
	data  []byte
	found bool
}

func readClipboardImageCmd() tea.Cmd {
	return func() tea.Msg {
		data, found := nativegui.ClipboardImage()
		return clipboardImageMsg{data: data, found: found}
	}
}

func (m *Model) attachImage(data []byte, name string) error {
	artifact, err := content.NewImage(data, name)
	if err != nil {
		return err
	}
	m.composerAttachments = append(m.composerAttachments, artifact)
	placeholder := "[Image #" + strconv.Itoa(len(m.composerAttachments)) + "]"
	value := m.input.Value()
	if value != "" && !strings.HasSuffix(value, " ") && !strings.HasSuffix(value, "\n") {
		placeholder = " " + placeholder
	}
	m.input.InsertString(placeholder + " ")
	m.input.Focus()
	m.status = "attached image · " + strconv.Itoa(artifact.Width) + "×" + strconv.Itoa(artifact.Height)
	m.updateLayout()
	return nil
}

func (m *Model) attachImagePathFromPaste(value string) bool {
	path := strings.TrimSpace(value)
	if path == "" || strings.ContainsAny(path, "\r\n") {
		return false
	}
	info, err := os.Stat(path)
	if err != nil || info.IsDir() || info.Size() > content.MaxImageBytes {
		return false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	if err := m.attachImage(data, path); err != nil {
		return false
	}
	return true
}

// composerPayload converts visible atomic placeholders into ordered content
// parts. Deleting a placeholder detaches that image; ordinary surrounding
// prose remains exactly as authored.
func composerPayload(value string, artifacts []content.Artifact) content.Payload {
	parts := make([]content.Part, 0, len(artifacts)*2+1)
	selected := make([]content.Artifact, 0, len(artifacts))
	selectedByReference := map[string]bool{}
	cursor := 0
	for _, bounds := range imagePlaceholderPattern.FindAllStringSubmatchIndex(value, -1) {
		index, err := strconv.Atoi(value[bounds[2]:bounds[3]])
		if err != nil || index < 1 || index > len(artifacts) {
			continue
		}
		if bounds[0] > cursor {
			parts = append(parts, content.Part{Type: content.PartText, Text: value[cursor:bounds[0]]})
		}
		ref := artifacts[index-1].ArtifactRef
		parts = append(parts, content.Part{Type: content.PartAttachment, Attachment: &ref})
		if !selectedByReference[ref.Reference] {
			selectedByReference[ref.Reference] = true
			selected = append(selected, artifacts[index-1])
		}
		cursor = bounds[1]
	}
	if cursor < len(value) {
		parts = append(parts, content.Part{Type: content.PartText, Text: value[cursor:]})
	}
	if len(parts) == 0 && value != "" {
		parts = append(parts, content.Part{Type: content.PartText, Text: value})
	}
	return content.Payload{Input: content.Input{Parts: parts}, Artifacts: selected}
}

func payloadDisplay(payload content.Payload) string {
	return strings.TrimSpace(payload.Input.DisplayText())
}

func (m *Model) ensureQueuedInputs() {
	for len(m.queuedInputs) < len(m.queued) {
		m.queuedInputs = append(m.queuedInputs, content.TextPayload(m.queued[len(m.queuedInputs)]))
	}
	if len(m.queuedInputs) > len(m.queued) {
		m.queuedInputs = m.queuedInputs[:len(m.queued)]
	}
}

func (m *Model) appendQueued(payload content.Payload) {
	m.ensureQueuedInputs()
	m.queued = append(m.queued, payloadDisplay(payload))
	m.queuedInputs = append(m.queuedInputs, payload)
}

func (m *Model) popQueued() (string, content.Payload, bool) {
	if len(m.queued) == 0 {
		return "", content.Payload{}, false
	}
	m.ensureQueuedInputs()
	display, payload := m.queued[0], m.queuedInputs[0]
	m.queued = append([]string(nil), m.queued[1:]...)
	m.queuedInputs = append([]content.Payload(nil), m.queuedInputs[1:]...)
	return display, payload, true
}

func (m *Model) prependQueued(display string, payload content.Payload) {
	m.ensureQueuedInputs()
	m.queued = append([]string{display}, m.queued...)
	m.queuedInputs = append([]content.Payload{payload}, m.queuedInputs...)
}

func mergePayloadDrafts(payloads []content.Payload, queuedCount int) (string, []content.Artifact) {
	var draft strings.Builder
	var artifacts []content.Artifact
	for payloadIndex, payload := range payloads {
		if payloadIndex > 0 {
			draft.WriteString("\n\n")
		}
		if len(payloads) > 1 {
			if payloadIndex < queuedCount {
				draft.WriteString("[Queued message " + strconv.Itoa(payloadIndex+1) + "]\n")
			} else {
				draft.WriteString("[Current draft]\n")
			}
		}
		byReference := make(map[string]content.Artifact, len(payload.Artifacts))
		for _, artifact := range payload.Artifacts {
			byReference[artifact.Reference] = artifact
		}
		for _, part := range payload.Input.Parts {
			switch part.Type {
			case content.PartText:
				draft.WriteString(part.Text)
			case content.PartAttachment:
				if part.Attachment == nil {
					continue
				}
				artifact, ok := byReference[part.Attachment.Reference]
				if !ok {
					continue
				}
				artifacts = append(artifacts, artifact)
				draft.WriteString("[Image #" + strconv.Itoa(len(artifacts)) + "]")
			}
		}
	}
	return draft.String(), artifacts
}
