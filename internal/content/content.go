// Package content defines provider-neutral user input and durable attachment metadata.
// Binary evidence is deliberately kept out of event JSON; parts carry immutable references
// whose bytes live in the store.
package content

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"path/filepath"
	"strings"

	"github.com/google/uuid"
)

const MaxImageBytes = 64 << 20

type PartType string

const (
	PartText       PartType = "text"
	PartAttachment PartType = "attachment"
)

type ArtifactKind string

const (
	ArtifactImage ArtifactKind = "image"
)

// ArtifactRef is safe to persist in events and working views. It contains identity and
// inspection metadata, never the binary payload.
type ArtifactRef struct {
	Reference string       `json:"reference"`
	Kind      ArtifactKind `json:"kind"`
	MIMEType  string       `json:"mime_type"`
	Name      string       `json:"name,omitempty"`
	Digest    string       `json:"digest"`
	ByteCount int          `json:"byte_count"`
	Width     int          `json:"width,omitempty"`
	Height    int          `json:"height,omitempty"`
}

type Artifact struct {
	ArtifactRef
	Data []byte `json:"-"`
}

type Part struct {
	Type       PartType     `json:"type"`
	Text       string       `json:"text,omitempty"`
	Attachment *ArtifactRef `json:"attachment,omitempty"`
}

type Input struct {
	Parts []Part `json:"parts,omitempty"`
}

type Payload struct {
	Input     Input
	Artifacts []Artifact
}

// Validate proves that every attachment named by the provider-neutral input
// has exactly one matching binary artifact and that no unreferenced bytes are
// smuggled into the durable transaction.
func (p Payload) Validate() error {
	artifacts := make(map[string]Artifact, len(p.Artifacts))
	for _, artifact := range p.Artifacts {
		if strings.TrimSpace(artifact.Reference) == "" {
			return fmt.Errorf("attachment reference is required")
		}
		if _, exists := artifacts[artifact.Reference]; exists {
			return fmt.Errorf("duplicate attachment reference %s", artifact.Reference)
		}
		artifacts[artifact.Reference] = artifact
	}
	referenced := make(map[string]bool, len(p.Artifacts))
	for _, part := range p.Input.Parts {
		switch part.Type {
		case PartText:
			if part.Attachment != nil {
				return fmt.Errorf("text input part cannot carry attachment metadata")
			}
		case PartAttachment:
			if part.Attachment == nil || strings.TrimSpace(part.Attachment.Reference) == "" {
				return fmt.Errorf("attachment input part requires an immutable reference")
			}
			artifact, exists := artifacts[part.Attachment.Reference]
			if !exists {
				return fmt.Errorf("attachment %s is referenced without its binary artifact", part.Attachment.Reference)
			}
			if artifact.ArtifactRef != *part.Attachment {
				return fmt.Errorf("attachment %s metadata does not match its binary artifact", part.Attachment.Reference)
			}
			referenced[part.Attachment.Reference] = true
		default:
			return fmt.Errorf("unknown input part type %q", part.Type)
		}
	}
	for reference := range artifacts {
		if !referenced[reference] {
			return fmt.Errorf("attachment %s has bytes but is absent from the input", reference)
		}
	}
	return nil
}

func Text(value string) Input {
	if value == "" {
		return Input{}
	}
	return Input{Parts: []Part{{Type: PartText, Text: value}}}
}

func TextPayload(value string) Payload {
	return Payload{Input: Text(value)}
}

func (in Input) Empty() bool {
	for _, part := range in.Parts {
		if part.Type == PartText && strings.TrimSpace(part.Text) != "" {
			return false
		}
		if part.Type == PartAttachment && part.Attachment != nil {
			return false
		}
	}
	return true
}

// DisplayText is the semantic transcript form. Attachment numbering is local to this input
// and therefore stable across redraw, recovery, and provider serialization.
func (in Input) DisplayText() string {
	var result strings.Builder
	imageNumber := 0
	for _, part := range in.Parts {
		switch part.Type {
		case PartText:
			result.WriteString(part.Text)
		case PartAttachment:
			if part.Attachment == nil {
				continue
			}
			imageNumber++
			if part.Attachment.Kind == ArtifactImage {
				fmt.Fprintf(&result, "[Image #%d]", imageNumber)
			} else {
				fmt.Fprintf(&result, "[Attachment #%d]", imageNumber)
			}
		}
	}
	return result.String()
}

func (in Input) AttachmentRefs() []ArtifactRef {
	seen := map[string]bool{}
	var result []ArtifactRef
	for _, part := range in.Parts {
		if part.Type != PartAttachment || part.Attachment == nil || seen[part.Attachment.Reference] {
			continue
		}
		seen[part.Attachment.Reference] = true
		result = append(result, *part.Attachment)
	}
	return result
}

func NewImage(data []byte, name string) (Artifact, error) {
	if len(data) == 0 {
		return Artifact{}, fmt.Errorf("image is empty")
	}
	if len(data) > MaxImageBytes {
		return Artifact{}, fmt.Errorf("image is %d bytes; ALT accepts at most %d bytes", len(data), MaxImageBytes)
	}
	config, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return Artifact{}, fmt.Errorf("decode image metadata: %w", err)
	}
	mime, ok := imageMIME(format)
	if !ok {
		return Artifact{}, fmt.Errorf("unsupported image format %q", format)
	}
	if config.Width < 1 || config.Height < 1 {
		return Artifact{}, fmt.Errorf("image dimensions are invalid")
	}
	if int64(config.Width)*int64(config.Height) > 100_000_000 {
		return Artifact{}, fmt.Errorf("image dimensions %dx%d exceed ALT's 100 megapixel safety limit", config.Width, config.Height)
	}
	id, err := uuid.NewV7()
	if err != nil {
		return Artifact{}, fmt.Errorf("create attachment reference: %w", err)
	}
	digest := sha256.Sum256(data)
	return Artifact{
		ArtifactRef: ArtifactRef{
			Reference: "artifact:" + id.String(), Kind: ArtifactImage,
			MIMEType: mime, Name: filepath.Base(strings.TrimSpace(name)),
			Digest: hex.EncodeToString(digest[:]), ByteCount: len(data),
			Width: config.Width, Height: config.Height,
		},
		Data: append([]byte(nil), data...),
	}, nil
}

func imageMIME(format string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(format)) {
	case "png":
		return "image/png", true
	case "jpeg":
		return "image/jpeg", true
	case "gif":
		return "image/gif", true
	default:
		return "", false
	}
}

func Extension(mime string) string {
	switch strings.ToLower(strings.TrimSpace(mime)) {
	case "image/png":
		return ".png"
	case "image/jpeg":
		return ".jpg"
	case "image/gif":
		return ".gif"
	default:
		return ".bin"
	}
}
