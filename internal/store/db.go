package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/everlightlabs/multica-slack/internal/crypto"
	_ "modernc.org/sqlite"
)

const (
	timestampLayout = "2006-01-02T15:04:05.000000000Z"

	schemaSQL = `
CREATE TABLE IF NOT EXISTS users (
	slack_user_id TEXT PRIMARY KEY,
	slack_team_id TEXT NOT NULL,
	slack_dm_channel_id TEXT,
	multica_user_id TEXT,
	multica_user_name TEXT,
	multica_workspace_id TEXT,
	multica_workspace_slug TEXT,
	multica_pat_enc TEXT,
	slack_user_token_enc TEXT,
	notification_prefs_json TEXT NOT NULL,
	notifications_enabled INTEGER NOT NULL DEFAULT 1,
	auth_status TEXT NOT NULL DEFAULT 'disconnected',
	notify_after TEXT,
	updated_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS delivered_notifications (
	slack_user_id TEXT NOT NULL,
	inbox_item_id TEXT NOT NULL,
	delivery_status TEXT NOT NULL,
	issue_id TEXT,
	slack_channel_id TEXT,
	slack_message_ts TEXT,
	card_fallback TEXT,
	card_blocks_json TEXT,
	marked_read_at TEXT,
	created_at TEXT NOT NULL,
	PRIMARY KEY (slack_user_id, inbox_item_id)
);

CREATE INDEX IF NOT EXISTS idx_delivered_notifications_unread
	ON delivered_notifications (slack_user_id, delivery_status, marked_read_at, created_at);

CREATE TABLE IF NOT EXISTS oauth_states (
	state TEXT PRIMARY KEY,
	slack_user_id TEXT NOT NULL,
	expires_at TEXT NOT NULL
);
`
)

// ErrNotFound is returned when a requested record does not exist or has expired.
var ErrNotFound = errors.New("store: not found")

// AuthStatus represents the health of a user's Multica credentials.
type AuthStatus string

const (
	AuthOK           AuthStatus = "ok"
	AuthFailed       AuthStatus = "auth_failed"
	AuthDisconnected AuthStatus = "disconnected"
)

// DeliveryStatus represents how an inbox notification item was handled.
type DeliveryStatus string

const (
	DeliveryDelivered DeliveryStatus = "delivered"
	DeliverySkipped   DeliveryStatus = "skipped"
	DeliverySeeded    DeliveryStatus = "seeded"
)

// Store wraps SQLite persistence and field-level AES-256-GCM encryption.
type Store struct {
	db  *sql.DB
	box *crypto.Box
}

// Open initializes the SQLite database at path with WAL mode and runs migrations.
func Open(path string, box *crypto.Box) (*Store, error) {
	if path == "" {
		return nil, errors.New("store: path is required")
	}
	if box == nil {
		return nil, errors.New("store: crypto box is required")
	}

	if path != ":memory:" {
		if dir := filepath.Dir(path); dir != "" && dir != "." {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return nil, fmt.Errorf("store: create db dir: %w", err)
			}
		}
	}

	dsn := "file:" + path + "?_pragma=journal_mode(WAL)&_pragma=foreign_keys(ON)&_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("store: open sqlite: %w", err)
	}
	db.SetMaxOpenConns(1)

	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: ping sqlite: %w", err)
	}

	if _, err := db.Exec(schemaSQL); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: migrate schema: %w", err)
	}

	return &Store{db: db, box: box}, nil
}

// Ping verifies database connectivity.
func (s *Store) Ping(ctx context.Context) error {
	if s == nil || s.db == nil {
		return errors.New("store: uninitialized store")
	}
	return s.db.PingContext(ctx)
}

// Close checkpoints the SQLite WAL and closes the underlying database handle.
func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	_, ckptErr := s.db.Exec("PRAGMA wal_checkpoint(TRUNCATE)")
	closeErr := s.db.Close()
	return errors.Join(ckptErr, closeErr)
}

type rowScanner interface {
	Scan(dest ...any) error
}

func formatTime(t time.Time) string {
	return t.UTC().Format(timestampLayout)
}

func parseTime(raw string) (time.Time, error) {
	return time.Parse(time.RFC3339Nano, raw)
}

func nullString(s string) sql.NullString {
	if s == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: s, Valid: true}
}

func nullTime(t *time.Time) sql.NullString {
	if t == nil || t.IsZero() {
		return sql.NullString{}
	}
	return sql.NullString{String: formatTime(*t), Valid: true}
}
