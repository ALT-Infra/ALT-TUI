package store

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
	"time"

	"altv1/internal/event"
)

func (s *Store) Append(ctx context.Context, sessionID string, draft event.Draft) (event.Event, error) {
	s.appendMu.Lock()
	defer s.appendMu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return event.Event{}, fmt.Errorf("begin event transaction: %w", err)
	}
	defer tx.Rollback()

	var sequence int64
	if err := tx.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(sequence), 0) + 1 FROM events WHERE session_id = ?`,
		sessionID,
	).Scan(&sequence); err != nil {
		return event.Event{}, fmt.Errorf("allocate event sequence: %w", err)
	}
	now := time.Now().UTC()
	item, err := draft.Materialize(sessionID, sequence, now)
	if err != nil {
		return event.Event{}, err
	}
	if err := insertEvent(ctx, tx, item); err != nil {
		return event.Event{}, err
	}
	if err := applySessionProjection(ctx, tx, item); err != nil {
		return event.Event{}, err
	}
	if err := tx.Commit(); err != nil {
		return event.Event{}, fmt.Errorf("commit event: %w", err)
	}
	s.publish(item)
	return item, nil
}

func insertEvent(ctx context.Context, tx *sql.Tx, item event.Event) error {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO events(
			session_id, sequence, id, at, kind, actor,
			correlation_id, causation_id, data
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		item.SessionID,
		item.Sequence,
		item.ID,
		item.At.Format(time.RFC3339Nano),
		item.Kind,
		item.Actor,
		item.CorrelationID,
		item.CausationID,
		[]byte(item.Data),
	)
	if err != nil {
		return fmt.Errorf("insert event: %w", err)
	}
	return nil
}

func applySessionProjection(ctx context.Context, tx *sql.Tx, item event.Event) error {
	status := ""
	leadID := ""
	final := ""
	switch item.Kind {
	case event.LeadSelected:
		data, err := event.Decode[event.LeadSelectedData](item)
		if err != nil {
			return err
		}
		leadID = data.LeadID
	case event.FinalCompleted:
		data, err := event.Decode[event.FinalCompletedData](item)
		if err != nil {
			return err
		}
		status = string(SessionCompleted)
		final = data.Answer
	case event.SessionFailed:
		status = string(SessionFailed)
	case event.SessionCancelled:
		status = string(SessionCancelled)
	}

	query := `UPDATE sessions SET updated_at = ?`
	args := []any{item.At.Format(time.RFC3339Nano)}
	if leadID != "" {
		query += `, lead_id = ?`
		args = append(args, leadID)
	}
	if status != "" {
		query += `, status = ?`
		args = append(args, status)
	}
	if final != "" {
		query += `, final_answer = ?`
		args = append(args, final)
	}
	query += ` WHERE id = ?`
	args = append(args, item.SessionID)
	result, err := tx.ExecContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("update session projection: %w", err)
	}
	affected, _ := result.RowsAffected()
	if affected != 1 {
		return fmt.Errorf("session %s not found", item.SessionID)
	}
	return nil
}

func (s *Store) Events(ctx context.Context, sessionID string, afterSequence int64) ([]event.Event, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, session_id, sequence, at, kind, actor,
		       correlation_id, causation_id, data
		FROM events
		WHERE session_id = ? AND sequence > ?
		ORDER BY sequence`,
		sessionID, afterSequence,
	)
	if err != nil {
		return nil, fmt.Errorf("query events: %w", err)
	}
	defer rows.Close()

	var result []event.Event
	for rows.Next() {
		item, err := scanEvent(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

func (s *Store) LastSequence(ctx context.Context, sessionID string) (int64, error) {
	var sequence int64
	if err := s.db.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(sequence), 0) FROM events WHERE session_id = ?`,
		sessionID,
	).Scan(&sequence); err != nil {
		return 0, fmt.Errorf("query last event sequence: %w", err)
	}
	return sequence, nil
}

func scanEvent(row scanner) (event.Event, error) {
	var item event.Event
	var at string
	var kind string
	var data []byte
	if err := row.Scan(
		&item.ID,
		&item.SessionID,
		&item.Sequence,
		&at,
		&kind,
		&item.Actor,
		&item.CorrelationID,
		&item.CausationID,
		&data,
	); err != nil {
		return event.Event{}, fmt.Errorf("scan event: %w", err)
	}
	item.At, _ = time.Parse(time.RFC3339Nano, at)
	item.Kind = event.Kind(kind)
	item.Data = append(item.Data[:0], data...)
	return item, nil
}

func (s *Store) Subscribe(ctx context.Context, sessionID string, afterSequence int64) (<-chan event.Event, func(), error) {
	s.appendMu.Lock()
	history, err := s.Events(ctx, sessionID, afterSequence)
	if err != nil {
		s.appendMu.Unlock()
		return nil, nil, err
	}
	ch := make(chan event.Event)
	subscription := newSubscriber(history)
	s.subMu.Lock()
	if s.subs == nil {
		s.subMu.Unlock()
		s.appendMu.Unlock()
		return nil, nil, fmt.Errorf("store is closed")
	}
	if s.subs[sessionID] == nil {
		s.subs[sessionID] = make(map[*subscriber]struct{})
	}
	s.subs[sessionID][subscription] = struct{}{}
	s.subMu.Unlock()
	s.appendMu.Unlock()

	go func() {
		defer close(ch)
		ticker := time.NewTicker(200 * time.Millisecond)
		defer ticker.Stop()
		lastSequence := afterSequence
		emit := func(item event.Event) bool {
			if item.Sequence <= lastSequence {
				return true
			}
			select {
			case ch <- item:
				lastSequence = item.Sequence
				return true
			case <-ctx.Done():
				return false
			case <-subscription.done:
				return false
			}
		}
		for {
			if item, ok := subscription.pop(); ok {
				if emit(item) {
					continue
				}
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-subscription.done:
				return
			case <-subscription.signal:
			case <-ticker.C:
				items, err := s.Events(ctx, sessionID, lastSequence)
				if err != nil {
					return
				}
				for _, item := range items {
					if !emit(item) {
						return
					}
				}
			}
		}
	}()

	var once sync.Once
	unsubscribe := func() {
		once.Do(func() {
			s.subMu.Lock()
			if subscribers := s.subs[sessionID]; subscribers != nil {
				delete(subscribers, subscription)
				if len(subscribers) == 0 {
					delete(s.subs, sessionID)
				}
			}
			s.subMu.Unlock()
			subscription.close()
		})
	}
	return ch, unsubscribe, nil
}

func (s *Store) publish(item event.Event) {
	s.subMu.Lock()
	defer s.subMu.Unlock()
	for subscriber := range s.subs[item.SessionID] {
		subscriber.enqueue(item)
	}
}
