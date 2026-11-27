package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// DeliveredNotification records dedup state and Slack message metadata for a Multica inbox item.
type DeliveredNotification struct {
	SlackUserID    string
	InboxItemID    string
	DeliveryStatus DeliveryStatus
	IssueID        string
	SlackChannelID string
	SlackMessageTS string
	CardFallback   string
	CardBlocksJSON string
	MarkedReadAt   *time.Time
	CreatedAt      time.Time
}

const selectDeliveryColumns = `
	slack_user_id,
	inbox_item_id,
	delivery_status,
	issue_id,
	slack_channel_id,
	slack_message_ts,
	card_fallback,
	card_blocks_json,
	marked_read_at,
	created_at
`

// TryClaimDelivery attempts an atomic INSERT OR IGNORE claim for (slack_user_id, inbox_item_id).
// It returns true when this call inserted the row, or false if it was already claimed.
func (s *Store) TryClaimDelivery(ctx context.Context, rec DeliveredNotification) (bool, error) {
	createdAt := rec.CreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now().UTC()
	}

	res, err := s.db.ExecContext(ctx, `
		INSERT OR IGNORE INTO delivered_notifications (
			slack_user_id,
			inbox_item_id,
			delivery_status,
			issue_id,
			slack_channel_id,
			slack_message_ts,
			card_fallback,
			card_blocks_json,
			marked_read_at,
			created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`,
		rec.SlackUserID,
		rec.InboxItemID,
		string(rec.DeliveryStatus),
		nullString(rec.IssueID),
		nullString(rec.SlackChannelID),
		nullString(rec.SlackMessageTS),
		nullString(rec.CardFallback),
		nullString(rec.CardBlocksJSON),
		nullTime(rec.MarkedReadAt),
		formatTime(createdAt),
	)
	if err != nil {
		return false, fmt.Errorf("store: try claim delivery: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("store: try claim delivery rows affected: %w", err)
	}
	return affected > 0, nil
}

// UpdateDeliveryMessage records the Slack message coordinates and serialized card payload
// after chat.postMessage succeeds so read-state sync can call chat.update without im:history.
func (s *Store) UpdateDeliveryMessage(ctx context.Context, slackUserID, inboxItemID, channelID, messageTS, cardFallback, cardBlocksJSON string) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE delivered_notifications
		SET slack_channel_id = ?,
			slack_message_ts = ?,
			card_fallback = ?,
			card_blocks_json = ?
		WHERE slack_user_id = ? AND inbox_item_id = ?
	`,
		nullString(channelID),
		nullString(messageTS),
		nullString(cardFallback),
		nullString(cardBlocksJSON),
		slackUserID,
		inboxItemID,
	)
	if err != nil {
		return fmt.Errorf("store: update delivery message: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: update delivery message rows affected: %w", err)
	}
	if affected == 0 {
		return ErrNotFound
	}
	return nil
}

// ReleaseDeliveryClaim deletes a claimed delivery row when Slack posting fails transiently.
func (s *Store) ReleaseDeliveryClaim(ctx context.Context, slackUserID, inboxItemID string) error {
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM delivered_notifications WHERE slack_user_id = ? AND inbox_item_id = ?`,
		slackUserID, inboxItemID,
	); err != nil {
		return fmt.Errorf("store: release delivery claim: %w", err)
	}
	return nil
}

// GetUnreadDelivery returns a delivered, unread notification record for a user and inbox item.
func (s *Store) GetUnreadDelivery(ctx context.Context, slackUserID, inboxItemID string) (*DeliveredNotification, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT `+selectDeliveryColumns+`
		FROM delivered_notifications
		WHERE slack_user_id = ?
		  AND inbox_item_id = ?
		  AND delivery_status = 'delivered'
		  AND marked_read_at IS NULL
	`, slackUserID, inboxItemID)
	rec, err := scanDelivery(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: get unread delivery: %w", err)
	}
	return &rec, nil
}

// ListRecentUnreadDeliveries returns up to limit unread delivered notifications created at or after since.
func (s *Store) ListRecentUnreadDeliveries(ctx context.Context, slackUserID string, since time.Time, limit int) ([]DeliveredNotification, error) {
	if limit <= 0 {
		limit = -1
	}

	rows, err := s.db.QueryContext(ctx, `
		SELECT `+selectDeliveryColumns+`
		FROM delivered_notifications
		WHERE slack_user_id = ?
		  AND delivery_status = 'delivered'
		  AND marked_read_at IS NULL
		  AND created_at >= ?
		ORDER BY created_at DESC
		LIMIT ?
	`, slackUserID, formatTime(since), limit)
	if err != nil {
		return nil, fmt.Errorf("store: list recent unread deliveries: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var results []DeliveredNotification
	for rows.Next() {
		rec, err := scanDelivery(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan unread delivery: %w", err)
		}
		results = append(results, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate unread deliveries: %w", err)
	}
	return results, nil
}

// MarkDeliveryRead records marked_read_at for a single notification item.
func (s *Store) MarkDeliveryRead(ctx context.Context, slackUserID, inboxItemID string, readAt time.Time) error {
	if _, err := s.db.ExecContext(ctx, `
		UPDATE delivered_notifications
		SET marked_read_at = ?
		WHERE slack_user_id = ? AND inbox_item_id = ? AND marked_read_at IS NULL
	`, formatTime(readAt), slackUserID, inboxItemID); err != nil {
		return fmt.Errorf("store: mark delivery read: %w", err)
	}
	return nil
}

// MarkAllDeliveriesRead marks all unread delivered notifications for a user as read.
func (s *Store) MarkAllDeliveriesRead(ctx context.Context, slackUserID string, readAt time.Time) error {
	if _, err := s.db.ExecContext(ctx, `
		UPDATE delivered_notifications
		SET marked_read_at = ?
		WHERE slack_user_id = ? AND delivery_status = 'delivered' AND marked_read_at IS NULL
	`, formatTime(readAt), slackUserID); err != nil {
		return fmt.Errorf("store: mark all deliveries read: %w", err)
	}
	return nil
}

// PruneDeliveriesOlderThan removes delivery dedup records older than cutoff.
func (s *Store) PruneDeliveriesOlderThan(ctx context.Context, cutoff time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, `
		DELETE FROM delivered_notifications WHERE created_at < ?
	`, formatTime(cutoff))
	if err != nil {
		return 0, fmt.Errorf("store: prune deliveries: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("store: prune deliveries rows affected: %w", err)
	}
	return affected, nil
}

func scanDelivery(scanner rowScanner) (DeliveredNotification, error) {
	var (
		rec          DeliveredNotification
		status       string
		issueID      sql.NullString
		channelID    sql.NullString
		messageTS    sql.NullString
		cardFallback sql.NullString
		cardBlocks   sql.NullString
		markedRead   sql.NullString
		createdAtRaw string
	)

	err := scanner.Scan(
		&rec.SlackUserID,
		&rec.InboxItemID,
		&status,
		&issueID,
		&channelID,
		&messageTS,
		&cardFallback,
		&cardBlocks,
		&markedRead,
		&createdAtRaw,
	)
	if err != nil {
		return DeliveredNotification{}, err
	}

	rec.DeliveryStatus = DeliveryStatus(status)
	rec.IssueID = issueID.String
	rec.SlackChannelID = channelID.String
	rec.SlackMessageTS = messageTS.String
	rec.CardFallback = cardFallback.String
	rec.CardBlocksJSON = cardBlocks.String

	if markedRead.Valid && markedRead.String != "" {
		t, err := parseTime(markedRead.String)
		if err != nil {
			return DeliveredNotification{}, fmt.Errorf("parse marked_read_at: %w", err)
		}
		rec.MarkedReadAt = &t
	}
	createdAt, err := parseTime(createdAtRaw)
	if err != nil {
		return DeliveredNotification{}, fmt.Errorf("parse created_at: %w", err)
	}
	rec.CreatedAt = createdAt

	return rec, nil
}
