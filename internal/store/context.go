package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"altv1/internal/event"

	"github.com/google/uuid"
)

const contextReferencePrefix = "alt://context/records/"

type ContextRecord struct {
	ID             string
	SessionID      string
	SourceSequence int64
	Kind           event.Kind
	Actor          string
	CorrelationID  string
	CausationID    string
	Content        []byte
	Digest         string
	ByteCount      int
	CreatedAt      time.Time
}

func (r ContextRecord) Reference() string {
	return contextReferencePrefix + url.PathEscape(r.ID)
}

type ContextMatch struct {
	Reference      string
	SessionID      string
	SourceSequence int64
	Kind           event.Kind
	Actor          string
	CorrelationID  string
	Preview        string
	Rank           float64
}

type ContextBrowsePage struct {
	Records    []ContextMatch
	NextCursor string
}

type contextBrowseCursor struct {
	CreatedAt string `json:"created_at"`
	ID        string `json:"id"`
}

// ContextScope is an execution-time authority boundary over the canonical
// context archive. SessionIDs establishes the outer boundary. Within it,
// callers either receive every record or only records owned by one of the
// listed correlations plus exact records explicitly granted by ID.
type ContextScope struct {
	SessionIDs               []string
	CorrelationIDs           []string
	RecordIDs                []string
	Owners                   []string
	ArtifactReferences       []string
	IncludeAllSessionRecords bool
}

type ContextEpoch struct {
	SessionID             string
	ScopeKind             string
	ScopeID               string
	Epoch                 int
	SourceThroughSequence int64
	View                  json.RawMessage
	ViewDigest            string
	EstimatedTokens       int
	CreatedAt             time.Time
}

func insertContextRecord(ctx context.Context, tx *sql.Tx, item event.Event) error {
	digest := sha256.Sum256(item.Data)
	result, err := tx.ExecContext(ctx, `
		INSERT INTO context_records(
			id, session_id, source_sequence, kind, actor, correlation_id,
			causation_id, content, digest, byte_count, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		item.ID,
		item.SessionID,
		item.Sequence,
		item.Kind,
		item.Actor,
		item.CorrelationID,
		item.CausationID,
		[]byte(item.Data),
		hex.EncodeToString(digest[:]),
		len(item.Data),
		item.At.Format(time.RFC3339Nano),
	)
	if err != nil {
		return fmt.Errorf("archive context record: %w", err)
	}
	rowID, err := result.LastInsertId()
	if err != nil {
		return fmt.Errorf("resolve context record row: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO context_records_fts(rowid, record_id, session_id, content)
		VALUES (?, ?, ?, ?)`, rowID, item.ID, item.SessionID, string(item.Data)); err != nil {
		return fmt.Errorf("index context record: %w", err)
	}
	return nil
}

func (s *Store) backfillContextRecords(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, `
		SELECT e.id, e.session_id, e.sequence, e.at, e.kind, e.actor,
		       e.correlation_id, e.causation_id, e.data
		FROM events e
		LEFT JOIN context_records r ON r.id = e.id
		WHERE r.id IS NULL
		ORDER BY e.session_id, e.sequence`)
	if err != nil {
		return fmt.Errorf("inspect context archive backfill: %w", err)
	}
	var missing []event.Event
	for rows.Next() {
		var item event.Event
		var at, kind string
		var data []byte
		if err := rows.Scan(
			&item.ID, &item.SessionID, &item.Sequence, &at, &kind,
			&item.Actor, &item.CorrelationID, &item.CausationID, &data,
		); err != nil {
			rows.Close()
			return fmt.Errorf("scan context archive backfill: %w", err)
		}
		item.Kind = event.Kind(kind)
		item.Data = append(json.RawMessage(nil), data...)
		item.At, err = time.Parse(time.RFC3339Nano, at)
		if err != nil {
			rows.Close()
			return fmt.Errorf("parse context archive backfill time: %w", err)
		}
		missing = append(missing, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("iterate context archive backfill: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close context archive backfill: %w", err)
	}
	if len(missing) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin context archive backfill: %w", err)
	}
	defer tx.Rollback()
	for _, item := range missing {
		if err := insertContextRecord(ctx, tx, item); err != nil {
			return fmt.Errorf("backfill context event %s: %w", item.ID, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit context archive backfill: %w", err)
	}
	return nil
}

func ParseContextReference(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if !strings.HasPrefix(raw, contextReferencePrefix) {
		return "", fmt.Errorf("invalid ALT context reference")
	}
	id, err := url.PathUnescape(strings.TrimPrefix(raw, contextReferencePrefix))
	if err != nil || strings.Contains(id, "/") {
		return "", fmt.Errorf("invalid ALT context reference")
	}
	if _, err := uuid.Parse(id); err != nil {
		return "", fmt.Errorf("invalid ALT context record id")
	}
	return id, nil
}

// ContextRecordID validates an ALT context reference and returns its stable
// occurrence ID. It is useful when a leader explicitly grants a record to an
// otherwise isolated assignment.
func ContextRecordID(reference string) (string, error) {
	return ParseContextReference(reference)
}

func (s *Store) ContextRecord(ctx context.Context, sessionID, reference string) (ContextRecord, error) {
	return s.ContextRecordInScope(ctx, ContextScope{
		SessionIDs:               []string{sessionID},
		IncludeAllSessionRecords: true,
	}, reference)
}

func (s *Store) ContextRecordInScope(ctx context.Context, scope ContextScope, reference string) (ContextRecord, error) {
	id, err := ParseContextReference(reference)
	if err != nil {
		return ContextRecord{}, err
	}
	predicate, args, err := contextScopePredicate(scope, "r")
	if err != nil {
		return ContextRecord{}, err
	}
	args = append([]any{id}, args...)
	row := s.db.QueryRowContext(ctx, `
		SELECT id, session_id, source_sequence, kind, actor, correlation_id,
		       causation_id, content, digest, byte_count, created_at
		FROM context_records r
		WHERE r.id = ? AND `+predicate, args...)
	return scanContextRecord(row)
}

func (s *Store) ContextRecords(ctx context.Context, sessionID string, afterSequence int64, limit int) ([]ContextRecord, error) {
	if limit <= 0 || limit > 1000 {
		return nil, fmt.Errorf("context record limit must be within [1,1000]")
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, session_id, source_sequence, kind, actor, correlation_id,
		       causation_id, content, digest, byte_count, created_at
		FROM context_records
		WHERE session_id = ? AND source_sequence > ?
		ORDER BY source_sequence
		LIMIT ?`, sessionID, afterSequence, limit)
	if err != nil {
		return nil, fmt.Errorf("query context records: %w", err)
	}
	defer rows.Close()
	var records []ContextRecord
	for rows.Next() {
		record, err := scanContextRecord(rows)
		if err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	return records, rows.Err()
}

func (s *Store) SearchContext(ctx context.Context, sessionID, query string, limit int) ([]ContextMatch, error) {
	return s.SearchContextInScope(ctx, ContextScope{
		SessionIDs:               []string{sessionID},
		IncludeAllSessionRecords: true,
	}, query, limit)
}

func (s *Store) SearchContextInScope(ctx context.Context, scope ContextScope, query string, limit int) ([]ContextMatch, error) {
	if limit <= 0 || limit > 100 {
		return nil, fmt.Errorf("context search limit must be within [1,100]")
	}
	match, err := contextMatchExpression(query)
	if err != nil {
		return nil, err
	}
	predicate, args, err := contextScopePredicate(scope, "r")
	if err != nil {
		return nil, err
	}
	args = append([]any{match}, args...)
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, `
		SELECT r.id, r.source_sequence, r.kind, r.actor, r.correlation_id,
		       r.session_id,
		       snippet(context_records_fts, 2, '', '', ' … ', 24),
		       bm25(context_records_fts)
		FROM context_records_fts
		JOIN context_records r ON r.rowid = context_records_fts.rowid
		WHERE context_records_fts MATCH ? AND `+predicate+`
		ORDER BY bm25(context_records_fts), r.source_sequence DESC
		LIMIT ?`, args...)
	if err != nil {
		return nil, fmt.Errorf("search context records: %w", err)
	}
	defer rows.Close()
	var matches []ContextMatch
	for rows.Next() {
		var value ContextMatch
		var id, kind string
		if err := rows.Scan(
			&id, &value.SourceSequence, &kind, &value.Actor,
			&value.CorrelationID, &value.SessionID, &value.Preview, &value.Rank,
		); err != nil {
			return nil, fmt.Errorf("scan context match: %w", err)
		}
		value.Reference = contextReferencePrefix + url.PathEscape(id)
		value.Kind = event.Kind(kind)
		matches = append(matches, value)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	artifacts, err := s.SearchContextArtifactsInScope(ctx, scope, query, limit)
	if err != nil {
		return nil, err
	}
	return mergeContextMatches(limit, matches, artifacts), nil
}

func (s *Store) BrowseContextInScope(ctx context.Context, scope ContextScope, cursor string, limit int) (ContextBrowsePage, error) {
	if limit <= 0 || limit > 100 {
		return ContextBrowsePage{}, fmt.Errorf("context browse limit must be within [1,100]")
	}
	predicate, args, err := contextScopePredicate(scope, "r")
	if err != nil {
		return ContextBrowsePage{}, err
	}
	if strings.TrimSpace(cursor) != "" {
		boundary, err := decodeContextBrowseCursor(cursor)
		if err != nil {
			return ContextBrowsePage{}, err
		}
		predicate += " AND (r.created_at < ? OR (r.created_at = ? AND r.id < ?))"
		args = append(args, boundary.CreatedAt, boundary.CreatedAt, boundary.ID)
	}
	args = append(args, limit+1)
	rows, err := s.db.QueryContext(ctx, `
		SELECT r.id, r.session_id, r.source_sequence, r.kind, r.actor,
		       r.correlation_id, r.content, r.created_at
		FROM context_records r
		WHERE `+predicate+`
		ORDER BY r.created_at DESC, r.id DESC LIMIT ?`, args...)
	if err != nil {
		return ContextBrowsePage{}, fmt.Errorf("browse context records: %w", err)
	}
	defer rows.Close()
	type rowValue struct {
		match     ContextMatch
		id        string
		createdAt string
	}
	var values []rowValue
	for rows.Next() {
		var value rowValue
		var kind string
		var content []byte
		if err := rows.Scan(
			&value.id, &value.match.SessionID, &value.match.SourceSequence,
			&kind, &value.match.Actor, &value.match.CorrelationID,
			&content, &value.createdAt,
		); err != nil {
			return ContextBrowsePage{}, fmt.Errorf("scan context browse record: %w", err)
		}
		value.match.Reference = contextReferencePrefix + url.PathEscape(value.id)
		value.match.Kind = event.Kind(kind)
		value.match.Preview = contextPreview(string(content), 800)
		values = append(values, value)
	}
	if err := rows.Err(); err != nil {
		return ContextBrowsePage{}, err
	}
	page := ContextBrowsePage{}
	if len(values) > limit {
		boundary := values[limit-1]
		page.NextCursor = encodeContextBrowseCursor(contextBrowseCursor{CreatedAt: boundary.createdAt, ID: boundary.id})
		values = values[:limit]
	}
	for _, value := range values {
		page.Records = append(page.Records, value.match)
	}
	return page, nil
}

func encodeContextBrowseCursor(cursor contextBrowseCursor) string {
	encoded, _ := json.Marshal(cursor)
	return base64.RawURLEncoding.EncodeToString(encoded)
}

func decodeContextBrowseCursor(raw string) (contextBrowseCursor, error) {
	encoded, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(raw))
	if err != nil {
		return contextBrowseCursor{}, fmt.Errorf("invalid context browse cursor")
	}
	var cursor contextBrowseCursor
	if json.Unmarshal(encoded, &cursor) != nil || cursor.CreatedAt == "" || cursor.ID == "" {
		return contextBrowseCursor{}, fmt.Errorf("invalid context browse cursor")
	}
	if _, err := time.Parse(time.RFC3339Nano, cursor.CreatedAt); err != nil {
		return contextBrowseCursor{}, fmt.Errorf("invalid context browse cursor")
	}
	if _, err := uuid.Parse(cursor.ID); err != nil {
		return contextBrowseCursor{}, fmt.Errorf("invalid context browse cursor")
	}
	return cursor, nil
}

func contextPreview(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit]) + "…"
}

func contextScopePredicate(scope ContextScope, alias string) (string, []any, error) {
	sessions := uniqueContextValues(scope.SessionIDs)
	if len(sessions) == 0 {
		return "", nil, fmt.Errorf("context scope requires at least one session")
	}
	if alias != "" {
		alias += "."
	}
	var args []any
	sessionMarks := make([]string, len(sessions))
	for index, value := range sessions {
		sessionMarks[index] = "?"
		args = append(args, value)
	}
	predicate := alias + "session_id IN (" + strings.Join(sessionMarks, ",") + ")"
	if scope.IncludeAllSessionRecords {
		return predicate, args, nil
	}

	var grants []string
	correlations := uniqueContextValues(scope.CorrelationIDs)
	if len(correlations) > 0 {
		marks := make([]string, len(correlations))
		for index, value := range correlations {
			marks[index] = "?"
			args = append(args, value)
		}
		grants = append(grants, alias+"correlation_id IN ("+strings.Join(marks, ",")+")")
	}
	records := uniqueContextValues(scope.RecordIDs)
	if len(records) > 0 {
		marks := make([]string, len(records))
		for index, value := range records {
			marks[index] = "?"
			args = append(args, value)
		}
		grants = append(grants, alias+"id IN ("+strings.Join(marks, ",")+")")
	}
	if len(grants) == 0 {
		return predicate + " AND 0", args, nil
	}
	return predicate + " AND (" + strings.Join(grants, " OR ") + ")", args, nil
}

func uniqueContextValues(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result
}

func contextMatchExpression(query string) (string, error) {
	fields := strings.Fields(strings.TrimSpace(query))
	if len(fields) == 0 {
		return "", fmt.Errorf("context search query is required")
	}
	if len(fields) > 32 {
		return "", fmt.Errorf("context search query contains too many terms")
	}
	for index, field := range fields {
		fields[index] = `"` + strings.ReplaceAll(field, `"`, `""`) + `"`
	}
	return strings.Join(fields, " AND "), nil
}

func (s *Store) CommitContextEpoch(ctx context.Context, epoch ContextEpoch) (ContextEpoch, error) {
	epoch.SessionID = strings.TrimSpace(epoch.SessionID)
	epoch.ScopeKind = strings.TrimSpace(epoch.ScopeKind)
	epoch.ScopeID = strings.TrimSpace(epoch.ScopeID)
	if epoch.SessionID == "" || epoch.ScopeKind == "" || epoch.ScopeID == "" {
		return ContextEpoch{}, fmt.Errorf("context epoch session and scope are required")
	}
	if epoch.SourceThroughSequence < 1 {
		return ContextEpoch{}, fmt.Errorf("context epoch source sequence must be positive")
	}
	if !json.Valid(epoch.View) {
		return ContextEpoch{}, fmt.Errorf("context epoch view must be valid JSON")
	}
	if epoch.EstimatedTokens < 0 {
		return ContextEpoch{}, fmt.Errorf("context epoch token estimate cannot be negative")
	}
	digest := sha256.Sum256(epoch.View)
	epoch.ViewDigest = hex.EncodeToString(digest[:])
	epoch.CreatedAt = time.Now().UTC()

	// Epoch allocation and event appends share SQLite's single-writer lane.
	// Serializing them also guarantees monotonically allocated epochs when
	// independent specialist scopes commit concurrently.
	s.appendMu.Lock()
	defer s.appendMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ContextEpoch{}, fmt.Errorf("begin context epoch: %w", err)
	}
	defer tx.Rollback()
	var last int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(sequence), 0) FROM events WHERE session_id = ?`, epoch.SessionID).Scan(&last); err != nil {
		return ContextEpoch{}, fmt.Errorf("inspect context epoch source: %w", err)
	}
	if epoch.SourceThroughSequence > last {
		return ContextEpoch{}, fmt.Errorf("context epoch source sequence %d exceeds durable event sequence %d", epoch.SourceThroughSequence, last)
	}
	if err := tx.QueryRowContext(ctx, `
		SELECT COALESCE(MAX(epoch), 0) + 1
		FROM context_epochs
		WHERE session_id = ? AND scope_kind = ? AND scope_id = ?`,
		epoch.SessionID, epoch.ScopeKind, epoch.ScopeID,
	).Scan(&epoch.Epoch); err != nil {
		return ContextEpoch{}, fmt.Errorf("allocate context epoch: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO context_epochs(
			session_id, scope_kind, scope_id, epoch,
			source_through_sequence, view, view_digest,
			estimated_tokens, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		epoch.SessionID, epoch.ScopeKind, epoch.ScopeID, epoch.Epoch,
		epoch.SourceThroughSequence, []byte(epoch.View), epoch.ViewDigest,
		epoch.EstimatedTokens, epoch.CreatedAt.Format(time.RFC3339Nano),
	); err != nil {
		return ContextEpoch{}, fmt.Errorf("insert context epoch: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return ContextEpoch{}, fmt.Errorf("commit context epoch: %w", err)
	}
	return epoch, nil
}

func (s *Store) LatestContextEpoch(ctx context.Context, sessionID, scopeKind, scopeID string) (ContextEpoch, bool, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT session_id, scope_kind, scope_id, epoch,
		       source_through_sequence, view, view_digest,
		       estimated_tokens, created_at
		FROM context_epochs
		WHERE session_id = ? AND scope_kind = ? AND scope_id = ?
		ORDER BY epoch DESC LIMIT 1`, sessionID, scopeKind, scopeID)
	epoch, err := scanContextEpoch(row)
	if isNotFound(err) {
		return ContextEpoch{}, false, nil
	}
	if err != nil {
		return ContextEpoch{}, false, err
	}
	return epoch, true, nil
}

func scanContextRecord(row scanner) (ContextRecord, error) {
	var record ContextRecord
	var kind, created string
	if err := row.Scan(
		&record.ID, &record.SessionID, &record.SourceSequence, &kind,
		&record.Actor, &record.CorrelationID, &record.CausationID, &record.Content,
		&record.Digest, &record.ByteCount, &created,
	); err != nil {
		if isNotFound(err) {
			return ContextRecord{}, fmt.Errorf("context record not found: %w", err)
		}
		return ContextRecord{}, fmt.Errorf("scan context record: %w", err)
	}
	record.Kind = event.Kind(kind)
	parsed, err := time.Parse(time.RFC3339Nano, created)
	if err != nil {
		return ContextRecord{}, fmt.Errorf("parse context record time: %w", err)
	}
	record.CreatedAt = parsed
	return record, nil
}

func scanContextEpoch(row scanner) (ContextEpoch, error) {
	var epoch ContextEpoch
	var view []byte
	var created string
	if err := row.Scan(
		&epoch.SessionID, &epoch.ScopeKind, &epoch.ScopeID, &epoch.Epoch,
		&epoch.SourceThroughSequence, &view, &epoch.ViewDigest,
		&epoch.EstimatedTokens, &created,
	); err != nil {
		return ContextEpoch{}, err
	}
	epoch.View = append(json.RawMessage(nil), view...)
	parsed, err := time.Parse(time.RFC3339Nano, created)
	if err != nil {
		return ContextEpoch{}, fmt.Errorf("parse context epoch time: %w", err)
	}
	epoch.CreatedAt = parsed
	return epoch, nil
}

func ContextReferenceForEvent(item event.Event) string {
	return contextReferencePrefix + url.PathEscape(item.ID)
}

func ContextSequenceLabel(sequence int64) string {
	return "event:" + strconv.FormatInt(sequence, 10)
}
