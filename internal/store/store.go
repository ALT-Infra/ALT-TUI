package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"altv1/internal/event"

	"github.com/google/uuid"
	_ "modernc.org/sqlite"
)

type Store struct {
	db       *sql.DB
	appendMu sync.Mutex
	subMu    sync.Mutex
	subs     map[string]map[*subscriber]struct{}
}

type subscriber struct {
	mu     sync.Mutex
	queue  []event.Event
	signal chan struct{}
	done   chan struct{}
	once   sync.Once
	closed bool
}

func newSubscriber(history []event.Event) *subscriber {
	return &subscriber{
		queue:  append([]event.Event(nil), history...),
		signal: make(chan struct{}, 1),
		done:   make(chan struct{}),
	}
}

func (s *subscriber) enqueue(item event.Event) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.queue = append(s.queue, item)
	s.mu.Unlock()
	select {
	case s.signal <- struct{}{}:
	default:
	}
}

func (s *subscriber) pop() (event.Event, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.queue) == 0 {
		return event.Event{}, false
	}
	item := s.queue[0]
	s.queue[0] = event.Event{}
	s.queue = s.queue[1:]
	return item, true
}

func (s *subscriber) close() {
	s.once.Do(func() {
		s.mu.Lock()
		s.closed = true
		s.mu.Unlock()
		close(s.done)
	})
}

func Open(ctx context.Context, path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create data directory: %w", err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	db.SetMaxOpenConns(8)
	db.SetMaxIdleConns(4)
	db.SetConnMaxIdleTime(5 * time.Minute)

	s := &Store{db: db, subs: make(map[string]map[*subscriber]struct{})}
	if err := s.initialize(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func OpenMemory(ctx context.Context) (*Store, error) {
	return Open(ctx, "file:alt-v1-test-"+uuid.NewString()+"?mode=memory&cache=shared")
}

func (s *Store) Close() error {
	s.subMu.Lock()
	for _, subscribers := range s.subs {
		for subscription := range subscribers {
			subscription.close()
		}
	}
	s.subs = nil
	s.subMu.Unlock()
	return s.db.Close()
}

func (s *Store) DB() *sql.DB {
	return s.db
}

func (s *Store) initialize(ctx context.Context) error {
	statements := []string{
		`PRAGMA journal_mode=WAL`,
		`PRAGMA synchronous=NORMAL`,
		`PRAGMA foreign_keys=ON`,
		`PRAGMA busy_timeout=5000`,
		`CREATE TABLE IF NOT EXISTS profile_revisions (
			profile_id TEXT NOT NULL,
			revision INTEGER NOT NULL,
			digest TEXT NOT NULL,
			name TEXT NOT NULL,
			source BLOB NOT NULL,
			created_at TEXT NOT NULL,
			PRIMARY KEY (profile_id, revision),
			UNIQUE (digest)
		)`,
		`CREATE TABLE IF NOT EXISTS sessions (
			id TEXT PRIMARY KEY,
			conversation_id TEXT NOT NULL,
			profile_id TEXT NOT NULL,
			profile_revision INTEGER NOT NULL,
			profile_digest TEXT NOT NULL,
			title TEXT NOT NULL DEFAULT '',
			task TEXT NOT NULL,
			workspace TEXT NOT NULL DEFAULT '',
			lead_id TEXT,
			status TEXT NOT NULL,
			final_answer TEXT,
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL,
			FOREIGN KEY (profile_id, profile_revision)
				REFERENCES profile_revisions(profile_id, revision)
		)`,
		`CREATE INDEX IF NOT EXISTS sessions_status_updated
			ON sessions(status, updated_at DESC)`,
		`CREATE TABLE IF NOT EXISTS events (
			session_id TEXT NOT NULL,
			sequence INTEGER NOT NULL,
			id TEXT NOT NULL UNIQUE,
			at TEXT NOT NULL,
			kind TEXT NOT NULL,
			actor TEXT NOT NULL DEFAULT '',
			correlation_id TEXT NOT NULL DEFAULT '',
			causation_id TEXT NOT NULL DEFAULT '',
			data BLOB NOT NULL,
			PRIMARY KEY (session_id, sequence),
			FOREIGN KEY (session_id) REFERENCES sessions(id) ON DELETE CASCADE
		)`,
		`CREATE INDEX IF NOT EXISTS events_session_kind
			ON events(session_id, kind, sequence)`,
		`CREATE TABLE IF NOT EXISTS checkpoints (
			key TEXT PRIMARY KEY,
			value BLOB NOT NULL,
			updated_at TEXT NOT NULL
		)`,
	}
	for _, statement := range statements {
		if _, err := s.db.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("initialize sqlite: %w", err)
		}
	}
	if _, err := s.db.ExecContext(ctx, `
		CREATE INDEX IF NOT EXISTS sessions_conversation_created
		ON sessions(conversation_id, created_at)`); err != nil {
		return fmt.Errorf("index session conversations: %w", err)
	}
	var mode string
	if err := s.db.QueryRowContext(ctx, `PRAGMA journal_mode`).Scan(&mode); err != nil {
		return fmt.Errorf("verify sqlite journal mode: %w", err)
	}
	if mode != "wal" && mode != "memory" {
		return fmt.Errorf("sqlite WAL unavailable: journal_mode=%s", mode)
	}
	return nil
}

func isNotFound(err error) bool {
	return errors.Is(err, sql.ErrNoRows)
}
