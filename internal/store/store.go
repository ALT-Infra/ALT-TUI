package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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
	db, err := sql.Open("sqlite", sqliteDSN(path))
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

func sqliteDSN(path string) string {
	separator := "?"
	if strings.Contains(path, "?") {
		separator = "&"
	}
	// These pragmas are connection-local. Put them in the DSN so every pooled
	// connection enforces the same authority and durability contract.
	return path + separator +
		"_pragma=foreign_keys(1)" +
		"&_pragma=busy_timeout(5000)" +
		"&_pragma=synchronous(FULL)"
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
		`PRAGMA synchronous=FULL`,
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
		`CREATE TABLE IF NOT EXISTS artifacts (
				reference TEXT PRIMARY KEY,
				session_id TEXT NOT NULL,
				kind TEXT NOT NULL,
				mime_type TEXT NOT NULL,
				name TEXT NOT NULL DEFAULT '',
				digest TEXT NOT NULL,
				byte_count INTEGER NOT NULL,
				width INTEGER NOT NULL DEFAULT 0,
				height INTEGER NOT NULL DEFAULT 0,
				content BLOB NOT NULL,
				created_at TEXT NOT NULL,
				FOREIGN KEY (session_id) REFERENCES sessions(id) ON DELETE CASCADE
			)`,
		`CREATE INDEX IF NOT EXISTS artifacts_session
				ON artifacts(session_id, created_at)`,
		`CREATE INDEX IF NOT EXISTS artifacts_digest
				ON artifacts(digest)`,
		`CREATE TABLE IF NOT EXISTS context_records (
			id TEXT PRIMARY KEY,
			session_id TEXT NOT NULL,
			source_sequence INTEGER NOT NULL,
			kind TEXT NOT NULL,
			actor TEXT NOT NULL DEFAULT '',
			correlation_id TEXT NOT NULL DEFAULT '',
			causation_id TEXT NOT NULL DEFAULT '',
			content BLOB NOT NULL,
			digest TEXT NOT NULL,
			byte_count INTEGER NOT NULL,
			created_at TEXT NOT NULL,
			UNIQUE(session_id, source_sequence),
			FOREIGN KEY (session_id, source_sequence)
				REFERENCES events(session_id, sequence) ON DELETE CASCADE
		)`,
		`CREATE INDEX IF NOT EXISTS context_records_scope
			ON context_records(session_id, correlation_id, source_sequence)`,
		`CREATE VIRTUAL TABLE IF NOT EXISTS context_records_fts USING fts5(
			record_id UNINDEXED,
			session_id UNINDEXED,
			content,
			tokenize = 'unicode61'
		)`,
		`CREATE TRIGGER IF NOT EXISTS context_records_delete_fts
			AFTER DELETE ON context_records BEGIN
				DELETE FROM context_records_fts WHERE rowid = old.rowid;
			END`,
		`CREATE TABLE IF NOT EXISTS context_epochs (
			session_id TEXT NOT NULL,
			scope_kind TEXT NOT NULL,
			scope_id TEXT NOT NULL,
			epoch INTEGER NOT NULL,
			source_through_sequence INTEGER NOT NULL,
			view BLOB NOT NULL,
			view_digest TEXT NOT NULL,
			estimated_tokens INTEGER NOT NULL,
			created_at TEXT NOT NULL,
			PRIMARY KEY(session_id, scope_kind, scope_id, epoch),
			FOREIGN KEY (session_id) REFERENCES sessions(id) ON DELETE CASCADE
		)`,
		`CREATE INDEX IF NOT EXISTS context_epochs_latest
			ON context_epochs(session_id, scope_kind, scope_id, epoch DESC)`,
		`CREATE TABLE IF NOT EXISTS context_artifacts (
			reference TEXT PRIMARY KEY,
			session_id TEXT NOT NULL,
			owner TEXT NOT NULL,
			content BLOB NOT NULL,
			digest TEXT NOT NULL,
			byte_count INTEGER NOT NULL,
			created_at TEXT NOT NULL,
			FOREIGN KEY (session_id) REFERENCES sessions(id) ON DELETE CASCADE
		)`,
		`CREATE INDEX IF NOT EXISTS context_artifacts_scope
			ON context_artifacts(session_id, owner, created_at)`,
		`CREATE VIRTUAL TABLE IF NOT EXISTS context_artifacts_fts USING fts5(
			reference UNINDEXED,
			session_id UNINDEXED,
			content,
			tokenize = 'unicode61'
		)`,
		`CREATE TRIGGER IF NOT EXISTS context_artifacts_delete_fts
			AFTER DELETE ON context_artifacts BEGIN
				DELETE FROM context_artifacts_fts WHERE rowid = old.rowid;
			END`,
		`CREATE TABLE IF NOT EXISTS checkpoints (
			key TEXT PRIMARY KEY,
			value BLOB NOT NULL,
			updated_at TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS checkpoint_versions (
			key TEXT NOT NULL,
			version INTEGER NOT NULL,
			value BLOB NOT NULL,
			digest TEXT NOT NULL,
			created_at TEXT NOT NULL,
			PRIMARY KEY(key, version)
		)`,
		`CREATE INDEX IF NOT EXISTS checkpoint_versions_latest
			ON checkpoint_versions(key, version DESC)`,
		`CREATE TABLE IF NOT EXISTS settings (
			key TEXT PRIMARY KEY,
			value TEXT NOT NULL,
			updated_at TEXT NOT NULL
		)`,
	}
	for _, statement := range statements {
		if _, err := s.db.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("initialize sqlite: %w", err)
		}
	}
	if err := s.ensureContextRecordCausation(ctx); err != nil {
		return err
	}
	if err := s.backfillContextRecords(ctx); err != nil {
		return err
	}
	if err := s.backfillCheckpointVersions(ctx); err != nil {
		return err
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

func (s *Store) ensureContextRecordCausation(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, `PRAGMA table_info(context_records)`)
	if err != nil {
		return fmt.Errorf("inspect context record schema: %w", err)
	}
	found := false
	for rows.Next() {
		var ordinal int
		var name, dataType string
		var notNull, primaryKey int
		var defaultValue any
		if err := rows.Scan(&ordinal, &name, &dataType, &notNull, &defaultValue, &primaryKey); err != nil {
			rows.Close()
			return fmt.Errorf("scan context record schema: %w", err)
		}
		if name == "causation_id" {
			found = true
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("iterate context record schema: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close context record schema: %w", err)
	}
	if !found {
		if _, err := s.db.ExecContext(ctx, `ALTER TABLE context_records ADD COLUMN causation_id TEXT NOT NULL DEFAULT ''`); err != nil {
			return fmt.Errorf("add context causation metadata: %w", err)
		}
	}
	if _, err := s.db.ExecContext(ctx, `
		UPDATE context_records
		SET causation_id = COALESCE((
			SELECT e.causation_id FROM events e
			WHERE e.session_id = context_records.session_id
			  AND e.sequence = context_records.source_sequence
		), '')
		WHERE causation_id = ''`); err != nil {
		return fmt.Errorf("backfill context causation metadata: %w", err)
	}
	return nil
}

func (s *Store) Setting(ctx context.Context, key string) (string, bool, error) {
	if s == nil || s.db == nil {
		return "", false, fmt.Errorf("store is required")
	}
	var value string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, key).Scan(&value)
	if isNotFound(err) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("read setting %q: %w", key, err)
	}
	return value, true, nil
}

func (s *Store) SetSetting(ctx context.Context, key, value string) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("store is required")
	}
	if key == "" {
		return fmt.Errorf("setting key is required")
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO settings (key, value, updated_at) VALUES (?, ?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		key, value, time.Now().UTC().Format(time.RFC3339Nano),
	)
	if err != nil {
		return fmt.Errorf("write setting %q: %w", key, err)
	}
	return nil
}

func isNotFound(err error) bool {
	return errors.Is(err, sql.ErrNoRows)
}
