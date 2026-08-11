package tooling

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/adk/middlewares/summarization"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

const agentCompactionInstruction = `Condense the working exchange into a clear continuation brief. Preserve the current objective, user constraints, decisions already made, exact identifiers and paths, evidence that affects later choices, unresolved work, failed attempts, and the next concrete step. Distinguish observations from inferences. Do not invent missing details. Older evidence remains available through the exact transcript reference that ALT will attach, so make this brief useful for deciding what to reopen rather than trying to reproduce every byte.`

func (r *Runtime) contextCompactionHandler(
	ctx context.Context,
	owner string,
	summarizer model.BaseChatModel,
) (adk.ChatModelAgentMiddleware, error) {
	if summarizer == nil {
		return nil, nil
	}
	return summarization.New(ctx, &summarization.Config{
		Model: summarizer,
		Trigger: &summarization.TriggerCondition{
			ContextTokens: 60_000, ContextMessages: 80,
		},
		UserInstruction: agentCompactionInstruction,
		Finalize: func(ctx context.Context, original []*schema.Message, summary *schema.Message) ([]*schema.Message, error) {
			reference, err := r.archiveAgentTranscript(ctx, owner, original)
			if err != nil {
				return nil, err
			}
			copy := *summary
			note := "\n\nExact pre-compaction transcript: " + reference +
				". Use context_open if a detail omitted from this working brief becomes relevant."
			if len(copy.AssistantGenMultiContent) > 0 {
				copy.AssistantGenMultiContent = append(append([]schema.MessageOutputPart(nil), copy.AssistantGenMultiContent...), schema.MessageOutputPart{
					Type: schema.ChatMessagePartTypeText, Text: note,
				})
			} else {
				copy.Content += note
			}
			final, err := summarization.DefaultFinalize(ctx, original, &copy)
			if err != nil {
				return nil, err
			}
			if r.options.RecordAgentCompaction != nil {
				if err := r.options.RecordAgentCompaction(ctx, owner, reference, len(original), len(final)); err != nil {
					return nil, err
				}
			}
			return final, nil
		},
	})
}

func (r *Runtime) archiveAgentTranscript(ctx context.Context, owner string, messages []*schema.Message) (string, error) {
	archivable := make([]*schema.Message, 0, len(messages))
	for _, message := range messages {
		if message == nil {
			archivable = append(archivable, nil)
			continue
		}
		copy := *message
		if !r.options.PersistReasoning {
			copy.ReasoningContent = ""
			if len(copy.AssistantGenMultiContent) > 0 {
				parts := make([]schema.MessageOutputPart, 0, len(copy.AssistantGenMultiContent))
				for _, part := range copy.AssistantGenMultiContent {
					if part.Type == schema.ChatMessagePartTypeReasoning {
						continue
					}
					parts = append(parts, part)
				}
				copy.AssistantGenMultiContent = parts
			}
		}
		archivable = append(archivable, &copy)
	}
	content, err := json.Marshal(archivable)
	if err != nil {
		return "", fmt.Errorf("encode exact agent transcript: %w", err)
	}
	digest := sha256.Sum256(append(append([]byte(owner), 0), content...))
	reference := toolOutputPathPrefix + hex.EncodeToString(digest[:])
	path := filepath.Join(r.archive, "tool-output", hex.EncodeToString(digest[:]))
	if err := writeImmutableFile(path, content); err != nil {
		return "", err
	}
	if r.options.ArchiveToolOutput != nil {
		if err := r.options.ArchiveToolOutput(ctx, owner, reference, content); err != nil {
			return "", fmt.Errorf("index exact agent transcript: %w", err)
		}
	}
	return reference, nil
}

func writeImmutableFile(path string, content []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create exact transcript directory: %w", err)
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err == nil {
		if _, writeErr := file.Write(content); writeErr != nil {
			file.Close()
			return fmt.Errorf("write exact transcript: %w", writeErr)
		}
		if closeErr := file.Close(); closeErr != nil {
			return fmt.Errorf("close exact transcript: %w", closeErr)
		}
		return nil
	}
	if !os.IsExist(err) {
		return fmt.Errorf("create exact transcript: %w", err)
	}
	existing, readErr := os.ReadFile(path)
	if readErr != nil {
		return fmt.Errorf("verify exact transcript: %w", readErr)
	}
	if !bytes.Equal(existing, content) {
		return fmt.Errorf("exact transcript reference collision")
	}
	return nil
}
