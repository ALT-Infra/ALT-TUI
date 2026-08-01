package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"altv1/internal/event"
	"altv1/internal/profile"

	"github.com/google/uuid"
)

type SessionStatus string

const (
	SessionRunning   SessionStatus = "running"
	SessionCompleted SessionStatus = "completed"
	SessionFailed    SessionStatus = "failed"
	SessionCancelled SessionStatus = "cancelled"
)

type Session struct {
	ID              string
	ConversationID  string
	ProfileID       string
	ProfileRevision int
	ProfileDigest   string
	Title           string
	Task            string
	Workspace       string
	LeadID          string
	Status          SessionStatus
	FinalAnswer     string
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// SessionCursor is the stable boundary immediately after a page of durable
// conversations. UpdatedAt is compared as a timestamp and ConversationID
// breaks ties, so pagination neither relies on an in-memory row offset nor
// loses conversations that share an update time.
type SessionCursor struct {
	UpdatedAt      time.Time
	ConversationID string
}

type SessionPage struct {
	Items []Session
	Next  *SessionCursor
}

// PromptCursor is a cursor into the immutable creation order of orchestration
// turns. Session IDs are UUIDv7 values, so their bytewise order is creation
// order and remains stable while newer turns are appended.
type PromptCursor struct {
	SessionID string
}

type PromptRecord struct {
	SessionID string
	Text      string
}

type PromptPage struct {
	Items []PromptRecord
	Next  *PromptCursor
}

// PromptSnapshot pins interactive Up/Down navigation to the history that
// existed when the TUI started. Prompts submitted afterwards live in the
// TUI's local history and cannot shift these persistent offsets.
type PromptSnapshot struct {
	Count    int
	NewestID string
}

func (s *Store) CreateSession(
	ctx context.Context,
	document *profile.Document,
	task string,
	workspace string,
) (*Session, error) {
	return s.createSession(ctx, document, task, workspace, "", "")
}

// CreateContinuation creates another orchestration turn in the same durable
// conversation. The pinned Team Profile and workspace cannot drift between
// turns; changing either starts a new conversation instead.
func (s *Store) CreateContinuation(
	ctx context.Context,
	previousSessionID string,
	document *profile.Document,
	task string,
) (*Session, error) {
	previous, err := s.Session(ctx, previousSessionID)
	if err != nil {
		return nil, err
	}
	if previous.Status == SessionRunning {
		return nil, fmt.Errorf("session %s is still running", previousSessionID)
	}
	if previous.ProfileID != document.Profile.ID ||
		previous.ProfileRevision != document.Profile.Revision ||
		previous.ProfileDigest != document.Digest {
		return nil, fmt.Errorf("continuation Team Profile does not match the pinned profile")
	}
	return s.createSession(
		ctx, document, task, previous.Workspace,
		previous.ConversationID, previous.Title,
	)
}

func (s *Store) createSession(
	ctx context.Context,
	document *profile.Document,
	task string,
	workspace string,
	conversationID string,
	title string,
) (*Session, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return nil, fmt.Errorf("create session id: %w", err)
	}
	if conversationID == "" {
		conversationID = id.String()
	}
	if title == "" {
		title = defaultSessionTitle(task)
	}
	now := time.Now().UTC()
	session := &Session{
		ID:              id.String(),
		ConversationID:  conversationID,
		ProfileID:       document.Profile.ID,
		ProfileRevision: document.Profile.Revision,
		ProfileDigest:   document.Digest,
		Title:           title,
		Task:            task,
		Workspace:       workspace,
		Status:          SessionRunning,
		CreatedAt:       now,
		UpdatedAt:       now,
	}

	s.appendMu.Lock()
	defer s.appendMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin session transaction: %w", err)
	}
	defer tx.Rollback()

	_, err = tx.ExecContext(ctx, `
		INSERT INTO sessions(
			id, conversation_id, profile_id, profile_revision, profile_digest, title, task,
			workspace, status, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		session.ID,
		session.ConversationID,
		session.ProfileID,
		session.ProfileRevision,
		session.ProfileDigest,
		session.Title,
		session.Task,
		session.Workspace,
		session.Status,
		now.Format(time.RFC3339Nano),
		now.Format(time.RFC3339Nano),
	)
	if err != nil {
		return nil, fmt.Errorf("insert session: %w", err)
	}
	drafts := []event.Draft{
		{Kind: event.SessionCreated, Actor: "user", Data: event.SessionCreatedData{Task: task}},
		{Kind: event.ProfilePinned, Actor: "system", Data: event.ProfilePinnedData{
			ProfileID: document.Profile.ID,
			Revision:  document.Profile.Revision,
			Digest:    document.Digest,
		}},
	}
	materialized := make([]event.Event, 0, len(drafts))
	for i, draft := range drafts {
		item, err := draft.Materialize(session.ID, int64(i+1), now)
		if err != nil {
			return nil, err
		}
		if err := insertEvent(ctx, tx, item); err != nil {
			return nil, err
		}
		materialized = append(materialized, item)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit session: %w", err)
	}
	for _, item := range materialized {
		s.publish(item)
	}
	return session, nil
}

func (s *Store) Session(ctx context.Context, id string) (*Session, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, conversation_id, profile_id, profile_revision, profile_digest, title, task,
		       workspace, lead_id, status, final_answer, created_at, updated_at
		FROM sessions WHERE id = ?`, id)
	return scanSession(row)
}

func (s *Store) ListSessions(ctx context.Context, limit int) ([]Session, error) {
	if limit < 1 {
		return nil, fmt.Errorf("session list limit must be positive")
	}
	page, err := s.ListSessionPage(ctx, nil, limit)
	if err != nil {
		return nil, err
	}
	return page.Items, nil
}

func (s *Store) ListSessionPage(
	ctx context.Context,
	cursor *SessionCursor,
	limit int,
) (SessionPage, error) {
	if limit < 1 {
		return SessionPage{}, fmt.Errorf("session page size must be positive")
	}
	updatedBefore := "9999-12-31T23:59:59.999999999Z"
	conversationBefore := "\U0010ffff"
	if cursor != nil {
		updatedBefore = cursor.UpdatedAt.UTC().Format(time.RFC3339Nano)
		conversationBefore = cursor.ConversationID
	}
	rows, err := s.db.QueryContext(ctx, `
		WITH latest AS (
			SELECT s.*,
			       ROW_NUMBER() OVER (
			           PARTITION BY conversation_id
			           ORDER BY id DESC
			       ) AS conversation_rank
			FROM sessions s
		)
		SELECT id, conversation_id, profile_id, profile_revision,
		       profile_digest, title, task, workspace, lead_id,
		       status, final_answer, created_at, updated_at
		FROM latest
		WHERE conversation_rank = 1
		  AND (
		      julianday(updated_at) < julianday(?)
		      OR (
		          julianday(updated_at) = julianday(?)
		          AND conversation_id < ?
		      )
		  )
		ORDER BY julianday(updated_at) DESC, conversation_id DESC
		LIMIT ?`,
		updatedBefore, updatedBefore, conversationBefore, limit+1)
	if err != nil {
		return SessionPage{}, fmt.Errorf("list session page: %w", err)
	}
	defer rows.Close()

	result := make([]Session, 0, limit+1)
	for rows.Next() {
		item, err := scanSession(rows)
		if err != nil {
			return SessionPage{}, err
		}
		result = append(result, *item)
	}
	if err := rows.Err(); err != nil {
		return SessionPage{}, err
	}
	var next *SessionCursor
	if len(result) > limit {
		boundary := result[limit-1]
		next = &SessionCursor{
			UpdatedAt:      boundary.UpdatedAt,
			ConversationID: boundary.ConversationID,
		}
		result = result[:limit]
	}
	return SessionPage{Items: result, Next: next}, nil
}

func (s *Store) RecoverableSessions(ctx context.Context) ([]Session, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, conversation_id, profile_id, profile_revision, profile_digest, title, task,
		       workspace, lead_id, status, final_answer, created_at, updated_at
		FROM sessions WHERE status = ? ORDER BY updated_at`, SessionRunning)
	if err != nil {
		return nil, fmt.Errorf("list recoverable sessions: %w", err)
	}
	defer rows.Close()
	var result []Session
	for rows.Next() {
		item, err := scanSession(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, *item)
	}
	return result, rows.Err()
}

type scanner interface {
	Scan(dest ...any) error
}

func scanSession(row scanner) (*Session, error) {
	var item Session
	var lead, answer sql.NullString
	var status, created, updated string
	if err := row.Scan(
		&item.ID,
		&item.ConversationID,
		&item.ProfileID,
		&item.ProfileRevision,
		&item.ProfileDigest,
		&item.Title,
		&item.Task,
		&item.Workspace,
		&lead,
		&status,
		&answer,
		&created,
		&updated,
	); err != nil {
		if isNotFound(err) {
			return nil, fmt.Errorf("session not found")
		}
		return nil, fmt.Errorf("scan session: %w", err)
	}
	item.LeadID = scanNullableString(lead)
	item.FinalAnswer = scanNullableString(answer)
	item.Status = SessionStatus(status)
	item.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
	item.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updated)
	return &item, nil
}

func (s *Store) RenameSession(ctx context.Context, id, title string) error {
	title = strings.TrimSpace(title)
	if title == "" {
		return fmt.Errorf("session title cannot be empty")
	}
	result, err := s.db.ExecContext(ctx, `
		UPDATE sessions SET title = ?, updated_at = ?
		WHERE conversation_id = (
			SELECT conversation_id FROM sessions WHERE id = ?
		)`,
		title, time.Now().UTC().Format(time.RFC3339Nano), id,
	)
	if err != nil {
		return fmt.Errorf("rename session: %w", err)
	}
	affected, _ := result.RowsAffected()
	if affected < 1 {
		return fmt.Errorf("session not found")
	}
	return nil
}

func (s *Store) ResolveSessionID(ctx context.Context, reference string) (string, error) {
	reference = strings.TrimSpace(reference)
	if reference == "" {
		return "", fmt.Errorf("session reference cannot be empty")
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT DISTINCT conversation_id FROM sessions
		WHERE id = ? OR id LIKE ? OR conversation_id = ? OR conversation_id LIKE ?
		ORDER BY conversation_id LIMIT 2`,
		reference, reference+"%", reference, reference+"%",
	)
	if err != nil {
		return "", fmt.Errorf("resolve session: %w", err)
	}
	defer rows.Close()
	var conversations []string
	for rows.Next() {
		var conversationID string
		if err := rows.Scan(&conversationID); err != nil {
			return "", err
		}
		conversations = append(conversations, conversationID)
	}
	if len(conversations) == 0 {
		return "", fmt.Errorf("session %q not found", reference)
	}
	if len(conversations) > 1 {
		return "", fmt.Errorf("session reference %q is ambiguous", reference)
	}
	var latestID string
	if err := s.db.QueryRowContext(ctx, `
		SELECT id FROM sessions WHERE conversation_id = ?
		ORDER BY created_at DESC LIMIT 1`, conversations[0]).Scan(&latestID); err != nil {
		return "", fmt.Errorf("resolve latest session turn: %w", err)
	}
	return latestID, nil
}

func (s *Store) ConversationSessions(ctx context.Context, sessionID string) ([]Session, error) {
	session, err := s.Session(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, conversation_id, profile_id, profile_revision, profile_digest,
		       title, task, workspace, lead_id, status, final_answer,
		       created_at, updated_at
		FROM sessions WHERE conversation_id = ? ORDER BY created_at`,
		session.ConversationID,
	)
	if err != nil {
		return nil, fmt.Errorf("list conversation turns: %w", err)
	}
	defer rows.Close()
	var result []Session
	for rows.Next() {
		item, err := scanSession(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, *item)
	}
	return result, rows.Err()
}

func (s *Store) ListPromptPage(
	ctx context.Context,
	cursor *PromptCursor,
	limit int,
) (PromptPage, error) {
	if limit < 1 {
		return PromptPage{}, fmt.Errorf("prompt page size must be positive")
	}
	before := "\U0010ffff"
	if cursor != nil {
		before = cursor.SessionID
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, task FROM sessions
		WHERE id < ?
		ORDER BY id DESC
		LIMIT ?`, before, limit+1)
	if err != nil {
		return PromptPage{}, fmt.Errorf("list prompt page: %w", err)
	}
	defer rows.Close()
	type promptRow struct {
		id   string
		task string
	}
	result := make([]promptRow, 0, limit+1)
	for rows.Next() {
		var item promptRow
		if err := rows.Scan(&item.id, &item.task); err != nil {
			return PromptPage{}, fmt.Errorf("scan prompt history: %w", err)
		}
		result = append(result, item)
	}
	if err := rows.Err(); err != nil {
		return PromptPage{}, err
	}
	var next *PromptCursor
	if len(result) > limit {
		next = &PromptCursor{SessionID: result[limit-1].id}
		result = result[:limit]
	}
	items := make([]PromptRecord, len(result))
	for index, item := range result {
		items[index] = PromptRecord{SessionID: item.id, Text: item.task}
	}
	return PromptPage{Items: items, Next: next}, nil
}

func (s *Store) PromptSnapshot(ctx context.Context) (PromptSnapshot, error) {
	var snapshot PromptSnapshot
	if err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*), COALESCE(MAX(id), '') FROM sessions`,
	).Scan(&snapshot.Count, &snapshot.NewestID); err != nil {
		return PromptSnapshot{}, fmt.Errorf("snapshot prompt history: %w", err)
	}
	return snapshot, nil
}

func (s *Store) PromptAt(
	ctx context.Context,
	snapshot PromptSnapshot,
	offset int,
) (string, bool, error) {
	if offset < 0 || offset >= snapshot.Count || snapshot.NewestID == "" {
		return "", false, nil
	}
	var prompt string
	err := s.db.QueryRowContext(ctx, `
		SELECT task FROM sessions
		WHERE id <= ?
		ORDER BY id DESC
		LIMIT 1 OFFSET ?`,
		snapshot.NewestID, offset,
	).Scan(&prompt)
	if isNotFound(err) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("load prompt history offset %d: %w", offset, err)
	}
	return prompt, true, nil
}

func defaultSessionTitle(task string) string {
	title := strings.Join(strings.Fields(task), " ")
	const limit = 72
	if len(title) <= limit {
		return title
	}
	return strings.TrimSpace(title[:limit-1]) + "…"
}
