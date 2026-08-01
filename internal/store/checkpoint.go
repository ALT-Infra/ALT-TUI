package store

import (
	"context"
	"fmt"
	"time"
)

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
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO checkpoints(key, value, updated_at)
		VALUES (?, ?, ?)
		ON CONFLICT(key) DO UPDATE SET
			value = excluded.value,
			updated_at = excluded.updated_at`,
		key, value, time.Now().UTC().Format(time.RFC3339Nano),
	)
	if err != nil {
		return fmt.Errorf("set checkpoint: %w", err)
	}
	return nil
}

func (s *Store) Delete(ctx context.Context, key string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM checkpoints WHERE key = ?`, key); err != nil {
		return fmt.Errorf("delete checkpoint: %w", err)
	}
	return nil
}
