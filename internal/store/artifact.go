package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"time"

	"altv1/internal/content"
	"altv1/internal/event"
)

func insertArtifacts(
	ctx context.Context,
	tx *sql.Tx,
	sessionID string,
	artifacts []content.Artifact,
	now time.Time,
) error {
	seen := map[string]bool{}
	for _, artifact := range artifacts {
		if seen[artifact.Reference] {
			return fmt.Errorf("duplicate attachment reference %s", artifact.Reference)
		}
		seen[artifact.Reference] = true
		if err := validateArtifact(artifact); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `
			INSERT INTO artifacts(
				reference, session_id, kind, mime_type, name, digest,
				byte_count, width, height, content, created_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			artifact.Reference, sessionID, artifact.Kind, artifact.MIMEType,
			artifact.Name, artifact.Digest, artifact.ByteCount, artifact.Width,
			artifact.Height, artifact.Data, now.Format(time.RFC3339Nano),
		)
		if err != nil {
			return fmt.Errorf("insert attachment %s: %w", artifact.Reference, err)
		}
	}
	return nil
}

func validateArtifact(artifact content.Artifact) error {
	if artifact.Reference == "" || artifact.Kind == "" || artifact.MIMEType == "" {
		return fmt.Errorf("attachment identity, kind, and MIME type are required")
	}
	if len(artifact.Data) == 0 || artifact.ByteCount != len(artifact.Data) {
		return fmt.Errorf("attachment %s byte count does not match its payload", artifact.Reference)
	}
	digest := sha256.Sum256(artifact.Data)
	if hex.EncodeToString(digest[:]) != artifact.Digest {
		return fmt.Errorf("attachment %s digest does not match its payload", artifact.Reference)
	}
	return nil
}

// Artifact resolves an immutable attachment only when it belongs to the same durable
// conversation as the requesting session. This permits later turns to refer to an earlier
// image without widening access to another conversation.
func (s *Store) Artifact(ctx context.Context, sessionID, reference string) (content.Artifact, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT a.reference, a.kind, a.mime_type, a.name, a.digest,
		       a.byte_count, a.width, a.height, a.content
		FROM artifacts a
		JOIN sessions owner ON owner.id = a.session_id
		JOIN sessions current ON current.id = ?
		WHERE a.reference = ? AND owner.conversation_id = current.conversation_id`,
		sessionID, reference,
	)
	var artifact content.Artifact
	if err := row.Scan(
		&artifact.Reference, &artifact.Kind, &artifact.MIMEType, &artifact.Name,
		&artifact.Digest, &artifact.ByteCount, &artifact.Width, &artifact.Height,
		&artifact.Data,
	); err != nil {
		if err == sql.ErrNoRows {
			return content.Artifact{}, fmt.Errorf("attachment %s is not available to session %s", reference, sessionID)
		}
		return content.Artifact{}, fmt.Errorf("read attachment %s: %w", reference, err)
	}
	if err := validateArtifact(artifact); err != nil {
		return content.Artifact{}, err
	}
	return artifact, nil
}

func (s *Store) appendWithArtifacts(
	ctx context.Context,
	sessionID string,
	draft event.Draft,
	artifacts []content.Artifact,
) (event.Event, error) {
	s.appendMu.Lock()
	defer s.appendMu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return event.Event{}, fmt.Errorf("begin attachment event transaction: %w", err)
	}
	defer tx.Rollback()
	var sequence int64
	if err := tx.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(sequence), 0) + 1 FROM events WHERE session_id = ?`,
		sessionID,
	).Scan(&sequence); err != nil {
		return event.Event{}, fmt.Errorf("allocate attachment event sequence: %w", err)
	}
	now := time.Now().UTC()
	item, err := draft.Materialize(sessionID, sequence, now)
	if err != nil {
		return event.Event{}, err
	}
	if err := insertArtifacts(ctx, tx, sessionID, artifacts, now); err != nil {
		return event.Event{}, err
	}
	if err := insertEvent(ctx, tx, item); err != nil {
		return event.Event{}, err
	}
	if err := insertContextRecord(ctx, tx, item); err != nil {
		return event.Event{}, err
	}
	if err := applySessionProjection(ctx, tx, item); err != nil {
		return event.Event{}, err
	}
	if err := tx.Commit(); err != nil {
		return event.Event{}, fmt.Errorf("commit attachment event: %w", err)
	}
	s.publish(item)
	return item, nil
}

// AppendInput atomically archives new attachment bytes with the event that first names them.
func (s *Store) AppendInput(
	ctx context.Context,
	sessionID string,
	draft event.Draft,
	artifacts []content.Artifact,
) (event.Event, error) {
	data, ok := draft.Data.(event.UserInstructionData)
	if !ok {
		return event.Event{}, fmt.Errorf("attachment event must contain user instruction data")
	}
	if err := (content.Payload{Input: data.Input, Artifacts: artifacts}).Validate(); err != nil {
		return event.Event{}, fmt.Errorf("validate rich input: %w", err)
	}
	if len(artifacts) == 0 {
		return s.Append(ctx, sessionID, draft)
	}
	return s.appendWithArtifacts(ctx, sessionID, draft, artifacts)
}
