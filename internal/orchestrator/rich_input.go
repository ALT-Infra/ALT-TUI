package orchestrator

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"altv1/internal/content"
	"altv1/internal/provider"

	"github.com/cloudwego/eino/schema"
)

// richUserMessage turns durable ALT content into Eino's provider-neutral
// multimodal message. Unknown image capability is intentionally optimistic:
// only an explicit authenticated "unsupported" attestation suppresses the
// image part. Either way, the model receives a verified local evidence path.
func (r *sessionRuntime) richUserMessage(
	ctx context.Context,
	modelReference string,
	text string,
	references []string,
) (*schema.Message, error) {
	artifacts, manifests, err := r.resolveArtifacts(ctx, references)
	if err != nil {
		return nil, err
	}
	if len(manifests) > 0 {
		text = strings.TrimSpace(text) + "\n\nAVAILABLE ATTACHMENTS:\n" + strings.Join(manifests, "\n") +
			"\nALT also supplies each image directly when the selected model route accepts image input. The path is an exact fallback for tools, not a request to reopen an image the model can already see. If this model cannot interpret an attachment, it may collaborate through an authorized peer or specialist, use another available tool, or state the limitation honestly."
	}
	if len(artifacts) == 0 {
		return schema.UserMessage(text), nil
	}

	parts := []schema.MessageInputPart{{Type: schema.ChatMessagePartTypeText, Text: text}}
	capabilities := r.providers.Capabilities(r.profile.Gateway, r.profile.Models[modelReference])
	if capabilities.ImageInput != provider.CapabilityUnsupported {
		for _, artifact := range artifacts {
			if artifact.Kind != content.ArtifactImage {
				continue
			}
			encoded := base64.StdEncoding.EncodeToString(artifact.Data)
			parts = append(parts, schema.MessageInputPart{
				Type: schema.ChatMessagePartTypeImageURL,
				Extra: map[string]any{
					"alt_artifact_reference": artifact.Reference,
					"sha256":                 artifact.Digest,
				},
				Image: &schema.MessageInputImage{
					MessagePartCommon: schema.MessagePartCommon{
						Base64Data: &encoded,
						MIMEType:   artifact.MIMEType,
					},
					Detail: schema.ImageURLDetailAuto,
				},
			})
		}
	}
	return &schema.Message{Role: schema.User, UserInputMultiContent: parts}, nil
}

// richExactInputMessage preserves the user's authored text parts verbatim and
// keeps images in their original positions. Dynamic Team state, role metadata,
// and handoff information belong in the system instruction; they are never
// concatenated into this immutable user message.
func (r *sessionRuntime) richExactInputMessage(
	ctx context.Context,
	modelReference string,
	input content.Input,
) (*schema.Message, error) {
	references := make([]string, 0, len(input.AttachmentRefs()))
	for _, reference := range input.AttachmentRefs() {
		references = append(references, reference.Reference)
	}
	artifacts, _, err := r.resolveArtifacts(ctx, references)
	if err != nil {
		return nil, err
	}
	byReference := make(map[string]content.Artifact, len(artifacts))
	for _, artifact := range artifacts {
		byReference[artifact.Reference] = artifact
	}
	capabilities := r.providers.Capabilities(r.profile.Gateway, r.profile.Models[modelReference])
	parts := make([]schema.MessageInputPart, 0, len(input.Parts))
	for _, part := range input.Parts {
		switch part.Type {
		case content.PartText:
			parts = append(parts, schema.MessageInputPart{Type: schema.ChatMessagePartTypeText, Text: part.Text})
		case content.PartAttachment:
			if part.Attachment == nil || capabilities.ImageInput == provider.CapabilityUnsupported {
				continue
			}
			artifact, ok := byReference[part.Attachment.Reference]
			if !ok || artifact.Kind != content.ArtifactImage {
				continue
			}
			encoded := base64.StdEncoding.EncodeToString(artifact.Data)
			parts = append(parts, schema.MessageInputPart{
				Type: schema.ChatMessagePartTypeImageURL,
				Extra: map[string]any{
					"alt_artifact_reference": artifact.Reference,
					"sha256":                 artifact.Digest,
				},
				Image: &schema.MessageInputImage{
					MessagePartCommon: schema.MessagePartCommon{Base64Data: &encoded, MIMEType: artifact.MIMEType},
					Detail:            schema.ImageURLDetailAuto,
				},
			})
		}
	}
	return &schema.Message{Role: schema.User, UserInputMultiContent: parts}, nil
}

func (r *sessionRuntime) resolveArtifacts(ctx context.Context, references []string) ([]content.Artifact, []string, error) {
	seen := map[string]bool{}
	artifacts := make([]content.Artifact, 0, len(references))
	manifests := make([]string, 0, len(references))
	for _, reference := range references {
		reference = strings.TrimSpace(reference)
		if reference == "" || seen[reference] {
			continue
		}
		seen[reference] = true
		artifact, err := r.store.Artifact(ctx, r.session.ID, reference)
		if err != nil {
			return nil, nil, err
		}
		path, err := r.materializeArtifact(artifact)
		if err != nil {
			return nil, nil, err
		}
		artifacts = append(artifacts, artifact)
		manifests = append(manifests, fmt.Sprintf(
			"- %s: %s, %dx%d, %d bytes, sha256:%s, path %s",
			artifact.Reference, artifact.MIMEType, artifact.Width, artifact.Height,
			artifact.ByteCount, artifact.Digest, path,
		))
	}
	return artifacts, manifests, nil
}

func (r *sessionRuntime) materializeArtifact(artifact content.Artifact) (string, error) {
	root := strings.TrimSpace(r.engineOptions.ArtifactRoot)
	if root == "" {
		root = filepath.Join(os.TempDir(), "alt-v1-artifacts")
	}
	directory := filepath.Join(root, r.session.ConversationID)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return "", fmt.Errorf("create attachment evidence directory: %w", err)
	}
	path := filepath.Join(directory, artifact.Digest+content.Extension(artifact.MIMEType))
	if existing, err := os.ReadFile(path); err == nil {
		digest := sha256.Sum256(existing)
		if hex.EncodeToString(digest[:]) == artifact.Digest {
			return path, nil
		}
		return "", fmt.Errorf("attachment evidence path %s contains unexpected bytes", path)
	} else if !os.IsNotExist(err) {
		return "", fmt.Errorf("inspect attachment evidence path: %w", err)
	}
	temporary, err := os.CreateTemp(directory, ".attachment-*")
	if err != nil {
		return "", fmt.Errorf("create attachment evidence file: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return "", fmt.Errorf("protect attachment evidence file: %w", err)
	}
	if _, err := temporary.Write(artifact.Data); err != nil {
		temporary.Close()
		return "", fmt.Errorf("write attachment evidence file: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return "", fmt.Errorf("sync attachment evidence file: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return "", fmt.Errorf("close attachment evidence file: %w", err)
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return "", fmt.Errorf("publish attachment evidence file: %w", err)
	}
	return path, nil
}

func projectionAttachmentReferences(state *Projection) []string {
	var references []string
	for _, ref := range state.TaskInput.AttachmentRefs() {
		references = append(references, ref.Reference)
	}
	for _, input := range state.UserInstructionInputs {
		for _, ref := range input.AttachmentRefs() {
			references = append(references, ref.Reference)
		}
	}
	return uniqueStrings(references)
}
