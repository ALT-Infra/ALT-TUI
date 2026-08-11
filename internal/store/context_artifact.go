package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"
)

type ContextArtifact struct {
	Reference string
	SessionID string
	Owner     string
	Content   []byte
	Digest    string
	ByteCount int
	CreatedAt time.Time
}

// ArchiveContextArtifact indexes exact bytes offloaded from an Eino agent's
// working messages. A stable reference is immutable: replaying the same write
// is idempotent, while different bytes at that reference are rejected.
func (s *Store) ArchiveContextArtifact(ctx context.Context, artifact ContextArtifact) error {
	artifact.Reference = strings.TrimSpace(artifact.Reference)
	artifact.SessionID = strings.TrimSpace(artifact.SessionID)
	artifact.Owner = strings.TrimSpace(artifact.Owner)
	if artifact.Reference == "" || artifact.SessionID == "" || artifact.Owner == "" {
		return fmt.Errorf("context artifact reference, session, and owner are required")
	}
	digest := sha256.Sum256(artifact.Content)
	artifact.Digest = hex.EncodeToString(digest[:])
	artifact.ByteCount = len(artifact.Content)
	artifact.CreatedAt = time.Now().UTC()

	s.appendMu.Lock()
	defer s.appendMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin context artifact: %w", err)
	}
	defer tx.Rollback()
	var existing string
	err = tx.QueryRowContext(ctx, `SELECT digest FROM context_artifacts WHERE reference = ?`, artifact.Reference).Scan(&existing)
	if err == nil {
		if existing != artifact.Digest {
			return fmt.Errorf("context artifact reference %s is immutable", artifact.Reference)
		}
		return nil
	}
	if !isNotFound(err) {
		return fmt.Errorf("inspect context artifact: %w", err)
	}
	result, err := tx.ExecContext(ctx, `
		INSERT INTO context_artifacts(reference, session_id, owner, content, digest, byte_count, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		artifact.Reference, artifact.SessionID, artifact.Owner, artifact.Content,
		artifact.Digest, artifact.ByteCount, artifact.CreatedAt.Format(time.RFC3339Nano))
	if err != nil {
		return fmt.Errorf("archive context artifact: %w", err)
	}
	rowID, err := result.LastInsertId()
	if err != nil {
		return fmt.Errorf("resolve context artifact row: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO context_artifacts_fts(rowid, reference, session_id, content)
		VALUES (?, ?, ?, ?)`, rowID, artifact.Reference, artifact.SessionID, string(artifact.Content)); err != nil {
		return fmt.Errorf("index context artifact: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit context artifact: %w", err)
	}
	return nil
}

func (s *Store) ContextArtifactInScope(ctx context.Context, scope ContextScope, reference string) (ContextArtifact, error) {
	predicate, args, err := contextArtifactScopePredicate(scope, "a")
	if err != nil {
		return ContextArtifact{}, err
	}
	args = append([]any{strings.TrimSpace(reference)}, args...)
	var artifact ContextArtifact
	var created string
	err = s.db.QueryRowContext(ctx, `
		SELECT reference, session_id, owner, content, digest, byte_count, created_at
		FROM context_artifacts a WHERE a.reference = ? AND `+predicate, args...).Scan(
		&artifact.Reference, &artifact.SessionID, &artifact.Owner, &artifact.Content,
		&artifact.Digest, &artifact.ByteCount, &created)
	if err != nil {
		return ContextArtifact{}, fmt.Errorf("context artifact not found: %w", err)
	}
	artifact.CreatedAt, err = time.Parse(time.RFC3339Nano, created)
	if err != nil {
		return ContextArtifact{}, fmt.Errorf("parse context artifact time: %w", err)
	}
	return artifact, nil
}

func (s *Store) SearchContextArtifactsInScope(ctx context.Context, scope ContextScope, query string, limit int) ([]ContextMatch, error) {
	if limit <= 0 || limit > 100 {
		return nil, fmt.Errorf("context search limit must be within [1,100]")
	}
	match, err := contextMatchExpression(query)
	if err != nil {
		return nil, err
	}
	predicate, args, err := contextArtifactScopePredicate(scope, "a")
	if err != nil {
		return nil, err
	}
	args = append([]any{match}, args...)
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, `
		SELECT a.reference, a.session_id, a.owner,
		       snippet(context_artifacts_fts, 2, '', '', ' … ', 24),
		       bm25(context_artifacts_fts)
		FROM context_artifacts_fts
		JOIN context_artifacts a ON a.rowid = context_artifacts_fts.rowid
		WHERE context_artifacts_fts MATCH ? AND `+predicate+`
		ORDER BY bm25(context_artifacts_fts), a.created_at DESC LIMIT ?`, args...)
	if err != nil {
		return nil, fmt.Errorf("search context artifacts: %w", err)
	}
	defer rows.Close()
	var matches []ContextMatch
	for rows.Next() {
		var match ContextMatch
		if err := rows.Scan(&match.Reference, &match.SessionID, &match.Actor, &match.Preview, &match.Rank); err != nil {
			return nil, fmt.Errorf("scan context artifact match: %w", err)
		}
		match.Kind = "tool.output.archive"
		matches = append(matches, match)
	}
	return matches, rows.Err()
}

func contextArtifactScopePredicate(scope ContextScope, alias string) (string, []any, error) {
	sessions := uniqueContextValues(scope.SessionIDs)
	if len(sessions) == 0 {
		return "", nil, fmt.Errorf("context scope requires at least one session")
	}
	if alias != "" {
		alias += "."
	}
	var args []any
	marks := make([]string, len(sessions))
	for index, value := range sessions {
		marks[index] = "?"
		args = append(args, value)
	}
	predicate := alias + "session_id IN (" + strings.Join(marks, ",") + ")"
	if scope.IncludeAllSessionRecords {
		return predicate, args, nil
	}
	owners := uniqueContextValues(scope.Owners)
	references := uniqueContextValues(scope.ArtifactReferences)
	var grants []string
	if len(owners) > 0 {
		marks = make([]string, len(owners))
		for index, value := range owners {
			marks[index] = "?"
			args = append(args, value)
		}
		grants = append(grants, alias+"owner IN ("+strings.Join(marks, ",")+")")
	}
	if len(references) > 0 {
		marks = make([]string, len(references))
		for index, value := range references {
			marks[index] = "?"
			args = append(args, value)
		}
		grants = append(grants, alias+"reference IN ("+strings.Join(marks, ",")+")")
	}
	if len(grants) == 0 {
		return predicate + " AND 0", args, nil
	}
	return predicate + " AND (" + strings.Join(grants, " OR ") + ")", args, nil
}

func mergeContextMatches(limit int, groups ...[]ContextMatch) []ContextMatch {
	var merged []ContextMatch
	for _, group := range groups {
		merged = append(merged, group...)
	}
	sort.SliceStable(merged, func(i, j int) bool { return merged[i].Rank < merged[j].Rank })
	if len(merged) > limit {
		merged = merged[:limit]
	}
	return merged
}
