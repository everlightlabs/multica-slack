package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// CreateOAuthState stores a single-use OAuth CSRF state token bound to a Slack user ID.
func (s *Store) CreateOAuthState(ctx context.Context, state, slackUserID string, expiresAt time.Time) error {
	if state == "" {
		return errors.New("store: oauth state is required")
	}
	if slackUserID == "" {
		return errors.New("store: slack_user_id is required")
	}

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO oauth_states (state, slack_user_id, expires_at)
		VALUES (?, ?, ?)
	`, state, slackUserID, formatTime(expiresAt))
	if err != nil {
		return fmt.Errorf("store: create oauth state: %w", err)
	}
	return nil
}

// ConsumeOAuthState atomically deletes and validates an OAuth state token,
// returning ErrNotFound if the state does not exist or has expired.
func (s *Store) ConsumeOAuthState(ctx context.Context, state string, now time.Time) (string, error) {
	if state == "" {
		return "", ErrNotFound
	}

	var (
		slackUserID string
		expiresRaw  string
	)
	err := s.db.QueryRowContext(ctx, `
		DELETE FROM oauth_states
		WHERE state = ?
		RETURNING slack_user_id, expires_at
	`, state).Scan(&slackUserID, &expiresRaw)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("store: consume oauth state: %w", err)
	}

	expiresAt, err := parseTime(expiresRaw)
	if err != nil {
		return "", fmt.Errorf("store: parse oauth state expiry: %w", err)
	}
	if now.After(expiresAt) {
		return "", ErrNotFound
	}

	return slackUserID, nil
}

// PruneExpiredOAuthStates deletes OAuth state rows whose expires_at timestamp is before now.
func (s *Store) PruneExpiredOAuthStates(ctx context.Context, now time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, `
		DELETE FROM oauth_states
		WHERE expires_at < ?
	`, formatTime(now))
	if err != nil {
		return 0, fmt.Errorf("store: prune expired oauth states: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("store: prune expired oauth states rows affected: %w", err)
	}
	return n, nil
}
