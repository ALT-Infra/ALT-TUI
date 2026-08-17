package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"
)

type CheckpointVersion struct {
	Key       string
	Version   int
	Value     []byte
	Digest    string
	CreatedAt time.Time
}

func (s *Store) Get(ctx context.Context, key string) ([]byte, bool, error) {
	var value []byte
	err := s.db.QueryRowContext(ctx, `SELECT value FROM checkpoints WHERE key = ?`, key).Scan(&value)
	if isNotFound(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("get checkpoint: %w", err)
	}
	return append([]byte(nil), value...), true, nil
}

func (s *Store) Set(ctx context.Context, key string, value []byte) error {
	s.appendMu.Lock()
	defer s.appendMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin checkpoint: %w", err)
	}
	defer tx.Rollback()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	digest := sha256.Sum256(value)
	digestText := hex.EncodeToString(digest[:])
	var latestDigest string
	err = tx.QueryRowContext(ctx, `
		SELECT digest FROM checkpoint_versions
		WHERE key = ? ORDER BY version DESC LIMIT 1`, key).Scan(&latestDigest)
	if err != nil && !isNotFound(err) {
		return fmt.Errorf("inspect latest checkpoint version: %w", err)
	}
	if latestDigest != digestText {
		var version int
		if err := tx.QueryRowContext(ctx, `
			SELECT COALESCE(MAX(version), 0) + 1
			FROM checkpoint_versions WHERE key = ?`, key).Scan(&version); err != nil {
			return fmt.Errorf("allocate checkpoint version: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO checkpoint_versions(key, version, value, digest, created_at)
			VALUES (?, ?, ?, ?, ?)`, key, version, value, digestText, now); err != nil {
			return fmt.Errorf("archive checkpoint: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO checkpoints(key, value, updated_at)
		VALUES (?, ?, ?)
		ON CONFLICT(key) DO UPDATE SET
			value = excluded.value,
			updated_at = excluded.updated_at`, key, value, now); err != nil {
		return fmt.Errorf("set checkpoint: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit checkpoint: %w", err)
	}
	return nil
}

func (s *Store) Delete(ctx context.Context, key string) error {
	s.appendMu.Lock()
	defer s.appendMu.Unlock()
	if _, err := s.db.ExecContext(ctx, `DELETE FROM checkpoints WHERE key = ?`, key); err != nil {
		return fmt.Errorf("delete checkpoint: %w", err)
	}
	return nil
}

// CheckpointVersions returns the exact append-only history of distinct states
// behind Eino's mutable checkpoint pointer. Rewriting identical bytes does not
// create provenance, while deleting a live checkpoint never deletes a changed
// state; the versions are recovery evidence, not active state.
func (s *Store) CheckpointVersions(ctx context.Context, key string) ([]CheckpointVersion, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT key, version, value, digest, created_at
		FROM checkpoint_versions WHERE key = ? ORDER BY version`, key)
	if err != nil {
		return nil, fmt.Errorf("list checkpoint versions: %w", err)
	}
	defer rows.Close()
	var result []CheckpointVersion
	for rows.Next() {
		var item CheckpointVersion
		var created string
		if err := rows.Scan(&item.Key, &item.Version, &item.Value, &item.Digest, &created); err != nil {
			return nil, fmt.Errorf("scan checkpoint version: %w", err)
		}
		item.CreatedAt, err = time.Parse(time.RFC3339Nano, created)
		if err != nil {
			return nil, fmt.Errorf("parse checkpoint version: %w", err)
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

func (s *Store) backfillCheckpointVersions(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, `
		SELECT c.key, c.value, c.updated_at
		FROM checkpoints c
		LEFT JOIN checkpoint_versions v ON v.key = c.key
		WHERE v.key IS NULL`)
	if err != nil {
		return fmt.Errorf("inspect checkpoint backfill: %w", err)
	}
	type legacyCheckpoint struct {
		key, updated string
		value        []byte
	}
	var missing []legacyCheckpoint
	for rows.Next() {
		var item legacyCheckpoint
		if err := rows.Scan(&item.key, &item.value, &item.updated); err != nil {
			rows.Close()
			return fmt.Errorf("scan checkpoint backfill: %w", err)
		}
		missing = append(missing, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("iterate checkpoint backfill: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close checkpoint backfill: %w", err)
	}
	if len(missing) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin checkpoint backfill: %w", err)
	}
	defer tx.Rollback()
	for _, item := range missing {
		digest := sha256.Sum256(item.value)
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO checkpoint_versions(key, version, value, digest, created_at)
			VALUES (?, 1, ?, ?, ?)`, item.key, item.value, hex.EncodeToString(digest[:]), item.updated); err != nil {
			return fmt.Errorf("backfill checkpoint %s: %w", item.key, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit checkpoint backfill: %w", err)
	}
	return nil
}
