package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/everlightlabs/multica-slack/internal/crypto"
)

// DefaultNotificationPrefsJSON enables high-signal notification groups and mutes noisy ones.
const DefaultNotificationPrefsJSON = `{"mentions":true,"assignments":true,"comments":true,"status_changes":true,"agent_activity":true,"system_alerts":true,"updates":false,"new_issues":false,"reactions":false}`

// User holds a persisted Slack-to-Multica user record with decrypted secrets in memory.
type User struct {
	SlackUserID           string
	SlackTeamID           string
	SlackDMChannelID      string
	MulticaUserID         string
	MulticaUserName       string
	MulticaWorkspaceID    string
	MulticaWorkspaceSlug  string
	MulticaPAT            string
	SlackUserToken        string
	NotificationPrefsJSON string
	NotificationsEnabled  bool
	AuthStatus            AuthStatus
	NotifyAfter           *time.Time
	UpdatedAt             time.Time
}

// UpsertMulticaParams carries fields updated when a user links or updates a Multica PAT.
type UpsertMulticaParams struct {
	SlackUserID          string
	SlackTeamID          string
	MulticaUserID        string
	MulticaUserName      string
	MulticaWorkspaceID   string
	MulticaWorkspaceSlug string
	MulticaPAT           string
	NotifyAfter          time.Time
}

const selectUserColumns = `
	slack_user_id,
	slack_team_id,
	slack_dm_channel_id,
	multica_user_id,
	multica_user_name,
	multica_workspace_id,
	multica_workspace_slug,
	multica_pat_enc,
	slack_user_token_enc,
	notification_prefs_json,
	notifications_enabled,
	auth_status,
	notify_after,
	updated_at
`

// GetUser fetches a user by Slack user ID, decrypting stored credentials.
func (s *Store) GetUser(ctx context.Context, slackUserID string) (*User, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+selectUserColumns+` FROM users WHERE slack_user_id = ?`, slackUserID)
	u, err := scanUser(row, s.box)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: get user %s: %w", slackUserID, err)
	}
	return &u, nil
}

// ListActiveNotificationUsers returns all users with an active Multica PAT and enabled notifications.
func (s *Store) ListActiveNotificationUsers(ctx context.Context) ([]User, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+selectUserColumns+`
		FROM users
		WHERE multica_pat_enc IS NOT NULL
		  AND auth_status = 'ok'
		  AND notifications_enabled = 1
		ORDER BY slack_user_id
	`)
	if err != nil {
		return nil, fmt.Errorf("store: list active notification users: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var users []User
	for rows.Next() {
		u, err := scanUser(rows, s.box)
		if err != nil {
			return nil, fmt.Errorf("store: scan active user: %w", err)
		}
		users = append(users, u)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate active users: %w", err)
	}
	return users, nil
}

// Counts returns aggregate counts of linked users and users with failed Multica auth.
func (s *Store) Counts(ctx context.Context) (linkedUsers, authFailed int, err error) {
	err = s.db.QueryRowContext(ctx, `
		SELECT
			COALESCE(SUM(CASE WHEN multica_pat_enc IS NOT NULL AND auth_status != 'disconnected' THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN auth_status = 'auth_failed' THEN 1 ELSE 0 END), 0)
		FROM users
	`).Scan(&linkedUsers, &authFailed)
	if err != nil {
		return 0, 0, fmt.Errorf("store: user counts: %w", err)
	}
	return linkedUsers, authFailed, nil
}

// UpsertMulticaConnection inserts or updates a user's Multica PAT and identity,
// preserving existing notification preferences and Slack user OAuth tokens.
func (s *Store) UpsertMulticaConnection(ctx context.Context, params UpsertMulticaParams) error {
	if params.SlackUserID == "" {
		return errors.New("store: slack_user_id is required")
	}
	if params.MulticaPAT == "" {
		return errors.New("store: multica_pat is required")
	}

	patEnc, err := s.box.Seal(params.MulticaPAT)
	if err != nil {
		return fmt.Errorf("store: encrypt multica PAT: %w", err)
	}

	now := formatTime(time.Now().UTC())
	var notifyAfter sql.NullString
	if !params.NotifyAfter.IsZero() {
		notifyAfter = sql.NullString{String: formatTime(params.NotifyAfter), Valid: true}
	}

	_, err = s.db.ExecContext(ctx, `
		INSERT INTO users (
			slack_user_id,
			slack_team_id,
			multica_user_id,
			multica_user_name,
			multica_workspace_id,
			multica_workspace_slug,
			multica_pat_enc,
			notification_prefs_json,
			notifications_enabled,
			auth_status,
			notify_after,
			updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, 1, 'ok', ?, ?)
		ON CONFLICT(slack_user_id) DO UPDATE SET
			slack_team_id = COALESCE(NULLIF(excluded.slack_team_id, ''), users.slack_team_id),
			multica_user_id = excluded.multica_user_id,
			multica_user_name = excluded.multica_user_name,
			multica_workspace_id = excluded.multica_workspace_id,
			multica_workspace_slug = excluded.multica_workspace_slug,
			multica_pat_enc = excluded.multica_pat_enc,
			auth_status = 'ok',
			notify_after = COALESCE(excluded.notify_after, users.notify_after),
			updated_at = excluded.updated_at
	`,
		params.SlackUserID,
		params.SlackTeamID,
		nullString(params.MulticaUserID),
		nullString(params.MulticaUserName),
		nullString(params.MulticaWorkspaceID),
		nullString(params.MulticaWorkspaceSlug),
		patEnc,
		DefaultNotificationPrefsJSON,
		notifyAfter,
		now,
	)
	if err != nil {
		return fmt.Errorf("store: upsert multica connection: %w", err)
	}
	return nil
}

// SetSlackUserToken encrypts and stores a user's Slack xoxp- token, creating a minimal
// disconnected row if the user has not linked a Multica PAT yet.
func (s *Store) SetSlackUserToken(ctx context.Context, slackUserID, slackTeamID, userToken string) error {
	if slackUserID == "" {
		return errors.New("store: slack_user_id is required")
	}
	if userToken == "" {
		return errors.New("store: slack user token is required")
	}

	tokenEnc, err := s.box.Seal(userToken)
	if err != nil {
		return fmt.Errorf("store: encrypt slack user token: %w", err)
	}

	now := formatTime(time.Now().UTC())
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO users (
			slack_user_id,
			slack_team_id,
			slack_user_token_enc,
			notification_prefs_json,
			notifications_enabled,
			auth_status,
			updated_at
		) VALUES (?, ?, ?, ?, 1, 'disconnected', ?)
		ON CONFLICT(slack_user_id) DO UPDATE SET
			slack_team_id = COALESCE(NULLIF(excluded.slack_team_id, ''), users.slack_team_id),
			slack_user_token_enc = excluded.slack_user_token_enc,
			updated_at = excluded.updated_at
	`, slackUserID, slackTeamID, tokenEnc, DefaultNotificationPrefsJSON, now)
	if err != nil {
		return fmt.Errorf("store: set slack user token: %w", err)
	}
	return nil
}

// SetDMChannel persists the user's Slack app DM channel ID.
func (s *Store) SetDMChannel(ctx context.Context, slackUserID, channelID string) error {
	return s.execUpdateUser(ctx,
		`UPDATE users SET slack_dm_channel_id = ?, updated_at = ? WHERE slack_user_id = ?`,
		nullString(channelID), formatTime(time.Now().UTC()), slackUserID,
	)
}

// SetAuthStatus updates a user's Multica credential health status.
func (s *Store) SetAuthStatus(ctx context.Context, slackUserID string, status AuthStatus) error {
	switch status {
	case AuthOK, AuthFailed, AuthDisconnected:
	default:
		return fmt.Errorf("store: invalid auth status %q", status)
	}
	return s.execUpdateUser(ctx,
		`UPDATE users SET auth_status = ?, updated_at = ? WHERE slack_user_id = ?`,
		string(status), formatTime(time.Now().UTC()), slackUserID,
	)
}

// SetNotificationsEnabled toggles notifications and optionally advances notify_after.
func (s *Store) SetNotificationsEnabled(ctx context.Context, slackUserID string, enabled bool, notifyAfter *time.Time) error {
	enabledInt := 0
	if enabled {
		enabledInt = 1
	}
	now := formatTime(time.Now().UTC())
	if notifyAfter != nil {
		return s.execUpdateUser(ctx,
			`UPDATE users SET notifications_enabled = ?, notify_after = ?, updated_at = ? WHERE slack_user_id = ?`,
			enabledInt, formatTime(*notifyAfter), now, slackUserID,
		)
	}
	return s.execUpdateUser(ctx,
		`UPDATE users SET notifications_enabled = ?, updated_at = ? WHERE slack_user_id = ?`,
		enabledInt, now, slackUserID,
	)
}

// SetNotificationPrefs updates the user's JSON notification preferences.
func (s *Store) SetNotificationPrefs(ctx context.Context, slackUserID, prefsJSON string) error {
	return s.execUpdateUser(ctx,
		`UPDATE users SET notification_prefs_json = ?, updated_at = ? WHERE slack_user_id = ?`,
		prefsJSON, formatTime(time.Now().UTC()), slackUserID,
	)
}

// SwitchWorkspace updates the active notification workspace and advances notify_after.
func (s *Store) SwitchWorkspace(ctx context.Context, slackUserID, workspaceID, workspaceSlug string, notifyAfter time.Time) error {
	return s.execUpdateUser(ctx,
		`UPDATE users SET multica_workspace_id = ?, multica_workspace_slug = ?, notify_after = ?, updated_at = ? WHERE slack_user_id = ?`,
		nullString(workspaceID), nullString(workspaceSlug), formatTime(notifyAfter), formatTime(time.Now().UTC()), slackUserID,
	)
}

// DisconnectUser clears stored tokens and Multica identity columns while preserving prefs.
func (s *Store) DisconnectUser(ctx context.Context, slackUserID string) error {
	return s.execUpdateUser(ctx, `
		UPDATE users
		SET multica_pat_enc = NULL,
			slack_user_token_enc = NULL,
			multica_user_id = NULL,
			multica_user_name = NULL,
			multica_workspace_id = NULL,
			multica_workspace_slug = NULL,
			notify_after = NULL,
			auth_status = 'disconnected',
			updated_at = ?
		WHERE slack_user_id = ?
	`, formatTime(time.Now().UTC()), slackUserID)
}

func (s *Store) execUpdateUser(ctx context.Context, query string, args ...any) error {
	res, err := s.db.ExecContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("store: update user: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: update user rows affected: %w", err)
	}
	if affected == 0 {
		return ErrNotFound
	}
	return nil
}

func scanUser(scanner rowScanner, box *crypto.Box) (User, error) {
	var (
		u             User
		dmChannel     sql.NullString
		multicaUID    sql.NullString
		multicaName   sql.NullString
		workspaceID   sql.NullString
		workspaceSlug sql.NullString
		patEnc        sql.NullString
		slackTokEnc   sql.NullString
		enabledInt    int
		authStatus    string
		notifyAfter   sql.NullString
		updatedAtRaw  string
	)

	err := scanner.Scan(
		&u.SlackUserID,
		&u.SlackTeamID,
		&dmChannel,
		&multicaUID,
		&multicaName,
		&workspaceID,
		&workspaceSlug,
		&patEnc,
		&slackTokEnc,
		&u.NotificationPrefsJSON,
		&enabledInt,
		&authStatus,
		&notifyAfter,
		&updatedAtRaw,
	)
	if err != nil {
		return User{}, err
	}

	u.SlackDMChannelID = dmChannel.String
	u.MulticaUserID = multicaUID.String
	u.MulticaUserName = multicaName.String
	u.MulticaWorkspaceID = workspaceID.String
	u.MulticaWorkspaceSlug = workspaceSlug.String
	u.NotificationsEnabled = enabledInt != 0
	u.AuthStatus = AuthStatus(authStatus)

	if patEnc.Valid && patEnc.String != "" {
		pat, err := box.Open(patEnc.String)
		if err != nil {
			return User{}, fmt.Errorf("decrypt multica_pat_enc: %w", err)
		}
		u.MulticaPAT = pat
	}
	if slackTokEnc.Valid && slackTokEnc.String != "" {
		tok, err := box.Open(slackTokEnc.String)
		if err != nil {
			return User{}, fmt.Errorf("decrypt slack_user_token_enc: %w", err)
		}
		u.SlackUserToken = tok
	}
	if notifyAfter.Valid && notifyAfter.String != "" {
		t, err := parseTime(notifyAfter.String)
		if err != nil {
			return User{}, fmt.Errorf("parse notify_after: %w", err)
		}
		u.NotifyAfter = &t
	}
	updatedAt, err := parseTime(updatedAtRaw)
	if err != nil {
		return User{}, fmt.Errorf("parse updated_at: %w", err)
	}
	u.UpdatedAt = updatedAt

	return u, nil
}
