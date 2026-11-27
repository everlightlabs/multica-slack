package notify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/everlightlabs/multica-slack/internal/multica"
	"github.com/everlightlabs/multica-slack/internal/prefs"
	"github.com/everlightlabs/multica-slack/internal/store"
	"github.com/slack-go/slack"
)

const (
	defaultBatchUpdateDelay = 100 * time.Millisecond
	catchUpMaxAge           = 24 * time.Hour
	batchReadMaxItems       = 20
	batchReadWindow         = 7 * 24 * time.Hour
)

// SlackDMClient abstracts the Slack DM API operations used by Manager.
type SlackDMClient interface {
	OpenDM(ctx context.Context, slackUserID string) (channelID string, err error)
	PostMessage(ctx context.Context, channelID, fallbackText string, blocks []slack.Block) (ts string, err error)
	UpdateMessage(ctx context.Context, channelID, ts, fallbackText string, blocks []slack.Block) error
}

// ManagerConfig configures the notification manager and underlying Multica WS streams.
type ManagerConfig struct {
	MulticaAPIURL    string
	MulticaAppURL    string
	Version          string
	BatchUpdateDelay time.Duration // defaults to 100ms when 0; negative disables delay
	WSPingInterval   time.Duration
	WSPongTimeout    time.Duration
	WSAuthTimeout    time.Duration
	WSInitialBackoff time.Duration
	WSMaxBackoff     time.Duration
}

type userStream struct {
	cancel    context.CancelFunc
	done      chan struct{}
	bgWG      sync.WaitGroup
	connected bool
}

// Manager owns per-user Multica WebSocket streams, reconnect catch-up, no-backfill seeding,
// Slack DM delivery, and read-state synchronization.
type Manager struct {
	cfg              ManagerConfig
	batchUpdateDelay time.Duration
	store            *store.Store
	mc               *multica.Client
	cache            *multica.MetadataCache
	dm               SlackDMClient
	logger           *slog.Logger
	now              func() time.Time

	mu            sync.Mutex
	stopped       bool
	baseCtx       context.Context
	baseCancel    context.CancelFunc
	streams       map[string]*userStream
	onStateChange func(slackUserID string, connected bool)
	wg            sync.WaitGroup

	authMu sync.Mutex
}

// NewManager constructs a notification Manager.
func NewManager(cfg ManagerConfig, st *store.Store, mc *multica.Client, cache *multica.MetadataCache, dm SlackDMClient, logger *slog.Logger) *Manager {
	delay := cfg.BatchUpdateDelay
	if delay == 0 {
		delay = defaultBatchUpdateDelay
	} else if delay < 0 {
		delay = 0
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Manager{
		cfg:              cfg,
		batchUpdateDelay: delay,
		store:            st,
		mc:               mc,
		cache:            cache,
		dm:               dm,
		logger:           logger,
		now:              time.Now,
		streams:          make(map[string]*userStream),
	}
}

// SetOnStateChange registers an optional callback invoked whenever a user's
// WebSocket connection transitions between connected and disconnected.
func (m *Manager) SetOnStateChange(fn func(slackUserID string, connected bool)) {
	m.mu.Lock()
	m.onStateChange = fn
	m.mu.Unlock()
}

// Start launches WebSocket notification streams for all active users.
func (m *Manager) Start(ctx context.Context) error {
	m.mu.Lock()
	m.stopped = false
	if m.baseCtx == nil {
		m.baseCtx, m.baseCancel = context.WithCancel(ctx)
	}
	m.mu.Unlock()

	users, err := m.store.ListActiveNotificationUsers(ctx)
	if err != nil {
		return fmt.Errorf("notify: list active users: %w", err)
	}
	for _, user := range users {
		if user.MulticaPAT == "" || user.MulticaWorkspaceID == "" {
			continue
		}
		m.startUserStream(user)
	}
	return nil
}

// Stop cancels all active user streams and waits for every stream goroutine to exit.
func (m *Manager) Stop() {
	m.mu.Lock()
	m.stopped = true
	if m.baseCancel != nil {
		m.baseCancel()
		m.baseCancel = nil
		m.baseCtx = nil
	}
	for _, us := range m.streams {
		us.cancel()
	}
	m.mu.Unlock()

	m.wg.Wait()

	m.mu.Lock()
	clear(m.streams)
	m.mu.Unlock()
}

// ConnectUser snapshots all current unread inbox items as seeded (preventing backfill),
// advances notify_after, and starts/restarts the user's Multica WS stream.
func (m *Manager) ConnectUser(ctx context.Context, slackUserID string) error {
	user, err := m.store.GetUser(ctx, slackUserID)
	if err != nil {
		return fmt.Errorf("notify: connect user %s: %w", slackUserID, err)
	}
	if user.MulticaPAT == "" || user.MulticaWorkspaceID == "" {
		return fmt.Errorf("notify: connect user %s: missing Multica PAT or workspace", slackUserID)
	}

	m.stopUserStream(slackUserID)

	now := m.now().UTC()
	if err := m.snapshotUnread(ctx, user); err != nil {
		return fmt.Errorf("notify: connect snapshot %s: %w", slackUserID, err)
	}

	if err := m.store.SetNotificationsEnabled(ctx, slackUserID, user.NotificationsEnabled, &now); err != nil {
		return fmt.Errorf("notify: advance notify_after %s: %w", slackUserID, err)
	}
	user.NotifyAfter = &now

	if user.NotificationsEnabled && user.AuthStatus == store.AuthOK {
		m.startUserStream(*user)
	}
	return nil
}

// DisconnectUser stops the user's active WebSocket stream if one is running.
func (m *Manager) DisconnectUser(slackUserID string) {
	m.stopUserStream(slackUserID)
}

// PauseUser disables notifications in SQLite and cleanly closes the user's WebSocket stream.
func (m *Manager) PauseUser(ctx context.Context, slackUserID string) error {
	if err := m.store.SetNotificationsEnabled(ctx, slackUserID, false, nil); err != nil {
		return fmt.Errorf("notify: pause user %s: %w", slackUserID, err)
	}
	m.stopUserStream(slackUserID)
	return nil
}

// ResumeUser snapshots current unread inbox items as seeded, advances notify_after to now,
// enables notifications in SQLite, and starts the user's WebSocket stream.
func (m *Manager) ResumeUser(ctx context.Context, slackUserID string) error {
	user, err := m.store.GetUser(ctx, slackUserID)
	if err != nil {
		return fmt.Errorf("notify: resume user %s: %w", slackUserID, err)
	}

	m.stopUserStream(slackUserID)

	now := m.now().UTC()
	if user.MulticaPAT != "" && user.MulticaWorkspaceID != "" && user.AuthStatus == store.AuthOK {
		if err := m.snapshotUnread(ctx, user); err != nil {
			return fmt.Errorf("notify: resume snapshot %s: %w", slackUserID, err)
		}
	}

	if err := m.store.SetNotificationsEnabled(ctx, slackUserID, true, &now); err != nil {
		return fmt.Errorf("notify: enable notifications %s: %w", slackUserID, err)
	}
	user.NotificationsEnabled = true
	user.NotifyAfter = &now

	if user.MulticaPAT != "" && user.MulticaWorkspaceID != "" && user.AuthStatus == store.AuthOK {
		m.startUserStream(*user)
	}
	return nil
}

// SwitchUserWorkspace stops the user's current WS stream, updates their active workspace and
// notify_after watermark, snapshots unread items in the new workspace, and starts a new WS stream.
func (m *Manager) SwitchUserWorkspace(ctx context.Context, slackUserID, workspaceID, workspaceSlug string) error {
	m.stopUserStream(slackUserID)

	user, err := m.store.GetUser(ctx, slackUserID)
	if err != nil {
		return fmt.Errorf("notify: switch workspace get user %s: %w", slackUserID, err)
	}

	now := m.now().UTC()
	if err := m.store.SwitchWorkspace(ctx, slackUserID, workspaceID, workspaceSlug, now); err != nil {
		return fmt.Errorf("notify: switch workspace %s: %w", slackUserID, err)
	}
	user.MulticaWorkspaceID = workspaceID
	user.MulticaWorkspaceSlug = workspaceSlug
	user.NotifyAfter = &now

	if user.MulticaPAT != "" && workspaceID != "" && user.AuthStatus == store.AuthOK {
		if err := m.snapshotUnread(ctx, user); err != nil {
			return fmt.Errorf("notify: switch workspace snapshot %s: %w", slackUserID, err)
		}
		if user.NotificationsEnabled {
			m.startUserStream(*user)
		}
	}
	return nil
}

// IsConnected reports whether the user currently has an authenticated Multica WS connection.
func (m *Manager) IsConnected(slackUserID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	us := m.streams[slackUserID]
	return us != nil && us.connected
}

// ConnectedCount returns the number of users with active authenticated Multica WS connections.
func (m *Manager) ConnectedCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	count := 0
	for _, us := range m.streams {
		if us.connected {
			count++
		}
	}
	return count
}

func (m *Manager) startUserStream(user store.User) {
	var (
		streamCtx    context.Context
		streamCancel context.CancelFunc
		us           *userStream
	)
	for {
		m.mu.Lock()
		if m.stopped {
			m.mu.Unlock()
			return
		}
		if prev, ok := m.streams[user.SlackUserID]; ok {
			delete(m.streams, user.SlackUserID)
			m.mu.Unlock()
			prev.cancel()
			<-prev.done
			continue
		}
		if m.baseCtx == nil {
			m.baseCtx, m.baseCancel = context.WithCancel(context.Background())
		}
		streamCtx, streamCancel = context.WithCancel(m.baseCtx)
		us = &userStream{
			cancel: streamCancel,
			done:   make(chan struct{}),
		}
		m.streams[user.SlackUserID] = us
		m.wg.Add(1)
		m.mu.Unlock()
		break
	}

	wsCfg := multica.WSConfig{
		APIURL:         m.cfg.MulticaAPIURL,
		WorkspaceID:    user.MulticaWorkspaceID,
		Token:          user.MulticaPAT,
		Version:        m.cfg.Version,
		PingInterval:   m.cfg.WSPingInterval,
		PongTimeout:    m.cfg.WSPongTimeout,
		AuthTimeout:    m.cfg.WSAuthTimeout,
		InitialBackoff: m.cfg.WSInitialBackoff,
		MaxBackoff:     m.cfg.WSMaxBackoff,
	}

	slackUserID := user.SlackUserID
	go func() {
		defer func() {
			streamCancel()
			us.bgWG.Wait()
			m.mu.Lock()
			if m.streams[slackUserID] == us {
				us.connected = false
				delete(m.streams, slackUserID)
			}
			m.mu.Unlock()
			close(us.done)
			m.wg.Done()
		}()

		multica.RunUserStream(streamCtx, wsCfg, multica.WSCallbacks{
			OnStateChange: func(connected bool) {
				var cb func(string, bool)
				changed := false
				m.mu.Lock()
				if m.streams[slackUserID] == us && us.connected != connected {
					us.connected = connected
					changed = true
					cb = m.onStateChange
				}
				m.mu.Unlock()
				if changed && cb != nil {
					cb(slackUserID, connected)
				}
			},
			OnConnected: func(ctx context.Context) {
				m.handleConnected(ctx, slackUserID, streamCancel)
			},
			OnEvent: func(ctx context.Context, env multica.WSEnvelope) {
				m.handleEvent(ctx, streamCtx, us, slackUserID, env)
			},
			OnAuthFailed: func(ctx context.Context, reason string) {
				m.handleAuthFailed(ctx, slackUserID, reason)
			},
		})
	}()
}

func (m *Manager) stopUserStream(slackUserID string) {
	m.mu.Lock()
	us, ok := m.streams[slackUserID]
	if ok {
		delete(m.streams, slackUserID)
	}
	m.mu.Unlock()

	if !ok {
		return
	}
	us.cancel()
	<-us.done
}

func (m *Manager) snapshotUnread(ctx context.Context, user *store.User) error {
	items, err := m.mc.ListInbox(ctx, user.MulticaPAT, user.MulticaWorkspaceID)
	if err != nil {
		return err
	}
	now := m.now().UTC()
	for _, item := range items {
		if item.ID == "" || item.Read || item.Archived {
			continue
		}
		if _, err := m.store.TryClaimDelivery(ctx, store.DeliveredNotification{
			SlackUserID:    user.SlackUserID,
			InboxItemID:    item.ID,
			DeliveryStatus: store.DeliverySeeded,
			IssueID:        item.IssueID,
			CreatedAt:      now,
		}); err != nil {
			return err
		}
	}
	return nil
}

func (m *Manager) handleConnected(ctx context.Context, slackUserID string, cancelStream context.CancelFunc) {
	user, err := m.store.GetUser(ctx, slackUserID)
	if err != nil {
		m.logger.Warn("catch-up load user failed", slog.String("slack_user_id", slackUserID), slog.String("error", err.Error()))
		return
	}
	if !user.NotificationsEnabled || user.AuthStatus != store.AuthOK || user.MulticaPAT == "" || user.MulticaWorkspaceID == "" {
		return
	}

	items, err := m.mc.ListInbox(ctx, user.MulticaPAT, user.MulticaWorkspaceID)
	if err != nil {
		if errors.Is(err, multica.ErrUnauthorized) {
			m.handleAuthFailed(ctx, slackUserID, "unauthorized during inbox catch-up")
			if cancelStream != nil {
				cancelStream()
			}
			return
		}
		m.logger.Warn("catch-up list inbox failed",
			slog.String("slack_user_id", slackUserID),
			slog.String("workspace_id", user.MulticaWorkspaceID),
			slog.String("error", err.Error()),
		)
		return
	}

	cutoff := m.now().UTC().Add(-catchUpMaxAge)
	if user.NotifyAfter != nil && user.NotifyAfter.After(cutoff) {
		cutoff = user.NotifyAfter.UTC()
	}

	eligible := make([]multica.InboxItem, 0, len(items))
	for _, item := range items {
		if item.ID == "" || item.Read || item.Archived {
			continue
		}
		if item.CreatedAt.IsZero() || item.CreatedAt.Before(cutoff) {
			continue
		}
		eligible = append(eligible, item)
	}

	sort.SliceStable(eligible, func(i, j int) bool {
		return eligible[i].CreatedAt.Before(eligible[j].CreatedAt)
	})

	for _, item := range eligible {
		if ctx.Err() != nil {
			return
		}
		m.deliverInboxItem(ctx, user, item)
	}
}

func (m *Manager) handleEvent(ctx, streamCtx context.Context, us *userStream, slackUserID string, env multica.WSEnvelope) {
	switch env.Type {
	case "inbox:new":
		item, ok := decodeInboxNew(env.Payload)
		if !ok {
			m.logger.Warn("failed to decode inbox:new payload", slog.String("slack_user_id", slackUserID))
			return
		}
		if item.Read || item.Archived {
			return
		}
		user, err := m.store.GetUser(ctx, slackUserID)
		if err != nil {
			m.logger.Warn("inbox:new load user failed", slog.String("slack_user_id", slackUserID), slog.String("error", err.Error()))
			return
		}
		if !user.NotificationsEnabled || user.AuthStatus != store.AuthOK {
			return
		}
		m.deliverInboxItem(ctx, user, item)

	case "inbox:read", "inbox:archived":
		var payload struct {
			ItemID string `json:"item_id"`
		}
		if err := json.Unmarshal(env.Payload, &payload); err != nil {
			m.logger.Warn("failed to decode read/archived payload",
				slog.String("slack_user_id", slackUserID),
				slog.String("event_type", env.Type),
				slog.String("error", err.Error()),
			)
			return
		}
		if payload.ItemID != "" {
			m.handleItemRead(ctx, slackUserID, payload.ItemID)
		}

	case "inbox:batch-read":
		m.handleBatchRead(ctx, streamCtx, us, slackUserID)
	}
}

func decodeInboxNew(raw json.RawMessage) (multica.InboxItem, bool) {
	if len(raw) == 0 {
		return multica.InboxItem{}, false
	}
	var wrapped struct {
		Item multica.InboxItem `json:"item"`
	}
	if err := json.Unmarshal(raw, &wrapped); err == nil && wrapped.Item.ID != "" {
		return wrapped.Item, true
	}
	var direct multica.InboxItem
	if err := json.Unmarshal(raw, &direct); err == nil && direct.ID != "" {
		return direct, true
	}
	return multica.InboxItem{}, false
}

func (m *Manager) deliverInboxItem(ctx context.Context, user *store.User, item multica.InboxItem) {
	if item.ID == "" {
		return
	}
	if item.WorkspaceID == "" {
		item.WorkspaceID = user.MulticaWorkspaceID
	}

	userPrefs := prefs.Parse(user.NotificationPrefsJSON)
	if !prefs.ShouldDeliver(item.Type, userPrefs) {
		if _, err := m.store.TryClaimDelivery(ctx, store.DeliveredNotification{
			SlackUserID:    user.SlackUserID,
			InboxItemID:    item.ID,
			DeliveryStatus: store.DeliverySkipped,
			IssueID:        item.IssueID,
			CreatedAt:      m.now().UTC(),
		}); err != nil {
			m.logger.Warn("record skipped delivery failed",
				slog.String("slack_user_id", user.SlackUserID),
				slog.String("inbox_item_id", item.ID),
				slog.String("error", err.Error()),
			)
		}
		return
	}

	claimed, err := m.store.TryClaimDelivery(ctx, store.DeliveredNotification{
		SlackUserID:    user.SlackUserID,
		InboxItemID:    item.ID,
		DeliveryStatus: store.DeliveryDelivered,
		IssueID:        item.IssueID,
		CreatedAt:      m.now().UTC(),
	})
	if err != nil {
		m.logger.Warn("claim delivery failed",
			slog.String("slack_user_id", user.SlackUserID),
			slog.String("inbox_item_id", item.ID),
			slog.String("error", err.Error()),
		)
		return
	}
	if !claimed {
		return
	}

	dmChannelID, err := m.ensureDMChannel(ctx, user)
	if err != nil {
		_ = m.store.ReleaseDeliveryClaim(ctx, user.SlackUserID, item.ID)
		m.logger.Warn("open DM channel failed",
			slog.String("slack_user_id", user.SlackUserID),
			slog.String("inbox_item_id", item.ID),
			slog.String("error", err.Error()),
		)
		return
	}

	var meta multica.WorkspaceMeta
	if m.cache != nil && user.MulticaPAT != "" && item.WorkspaceID != "" {
		if fetched, metaErr := m.cache.WorkspaceMetadata(ctx, user.MulticaPAT, item.WorkspaceID); metaErr != nil {
			m.logger.Warn("workspace metadata fetch failed; sending notification without enrichment",
				slog.String("slack_user_id", user.SlackUserID),
				slog.String("workspace_id", item.WorkspaceID),
				slog.String("error", metaErr.Error()),
			)
		} else {
			meta = fetched
		}
	}

	fallback, blocks := FormatNotificationBlocks(m.cfg.MulticaAppURL, user.MulticaWorkspaceSlug, item, meta, false)
	cardBlocksJSON, err := MarshalBlocks(blocks)
	if err != nil {
		m.logger.Warn("marshal notification blocks failed",
			slog.String("slack_user_id", user.SlackUserID),
			slog.String("inbox_item_id", item.ID),
			slog.String("error", err.Error()),
		)
	}

	ts, err := m.dm.PostMessage(ctx, dmChannelID, fallback, blocks)
	if err != nil {
		if releaseErr := m.store.ReleaseDeliveryClaim(ctx, user.SlackUserID, item.ID); releaseErr != nil {
			m.logger.Warn("release delivery claim failed",
				slog.String("slack_user_id", user.SlackUserID),
				slog.String("inbox_item_id", item.ID),
				slog.String("error", releaseErr.Error()),
			)
		}
		m.logger.Warn("post Slack DM failed",
			slog.String("slack_user_id", user.SlackUserID),
			slog.String("inbox_item_id", item.ID),
			slog.String("error", err.Error()),
		)
		return
	}

	if err := m.store.UpdateDeliveryMessage(ctx, user.SlackUserID, item.ID, dmChannelID, ts, fallback, cardBlocksJSON); err != nil {
		m.logger.Warn("update delivery message metadata failed",
			slog.String("slack_user_id", user.SlackUserID),
			slog.String("inbox_item_id", item.ID),
			slog.String("error", err.Error()),
		)
	}
}

func (m *Manager) ensureDMChannel(ctx context.Context, user *store.User) (string, error) {
	if strings.TrimSpace(user.SlackDMChannelID) != "" {
		return user.SlackDMChannelID, nil
	}
	ch, err := m.dm.OpenDM(ctx, user.SlackUserID)
	if err != nil {
		return "", err
	}
	user.SlackDMChannelID = ch
	if err := m.store.SetDMChannel(ctx, user.SlackUserID, ch); err != nil {
		m.logger.Warn("cache DM channel ID failed",
			slog.String("slack_user_id", user.SlackUserID),
			slog.String("error", err.Error()),
		)
	}
	return ch, nil
}

func (m *Manager) handleItemRead(ctx context.Context, slackUserID, itemID string) {
	rec, err := m.store.GetUnreadDelivery(ctx, slackUserID, itemID)
	if errors.Is(err, store.ErrNotFound) {
		return
	}
	if err != nil {
		m.logger.Warn("get unread delivery failed",
			slog.String("slack_user_id", slackUserID),
			slog.String("inbox_item_id", itemID),
			slog.String("error", err.Error()),
		)
		return
	}

	m.updateDeliveryCardRead(ctx, *rec)

	if err := m.store.MarkDeliveryRead(ctx, slackUserID, itemID, m.now().UTC()); err != nil {
		m.logger.Warn("mark delivery read failed",
			slog.String("slack_user_id", slackUserID),
			slog.String("inbox_item_id", itemID),
			slog.String("error", err.Error()),
		)
	}
}

func (m *Manager) handleBatchRead(ctx, streamCtx context.Context, us *userStream, slackUserID string) {
	now := m.now().UTC()
	since := now.Add(-batchReadWindow)

	recs, err := m.store.ListRecentUnreadDeliveries(ctx, slackUserID, since, batchReadMaxItems)
	if err != nil {
		m.logger.Warn("list recent unread deliveries for batch-read failed",
			slog.String("slack_user_id", slackUserID),
			slog.String("error", err.Error()),
		)
		return
	}

	if err := m.store.MarkAllDeliveriesRead(ctx, slackUserID, now); err != nil {
		m.logger.Warn("mark all deliveries read failed",
			slog.String("slack_user_id", slackUserID),
			slog.String("error", err.Error()),
		)
	}

	if len(recs) == 0 {
		return
	}

	m.wg.Add(1)
	if us != nil {
		us.bgWG.Add(1)
	}
	go func() {
		defer m.wg.Done()
		if us != nil {
			defer us.bgWG.Done()
		}
		m.runBatchReadUpdates(streamCtx, recs)
	}()
}

func (m *Manager) runBatchReadUpdates(ctx context.Context, recs []store.DeliveredNotification) {
	for i, rec := range recs {
		if ctx.Err() != nil {
			return
		}
		if i > 0 && m.batchUpdateDelay > 0 {
			timer := time.NewTimer(m.batchUpdateDelay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
		}
		m.updateDeliveryCardRead(ctx, rec)
	}
}

func (m *Manager) updateDeliveryCardRead(ctx context.Context, rec store.DeliveredNotification) {
	if rec.SlackChannelID == "" || rec.SlackMessageTS == "" {
		return
	}
	blocks, err := UnmarshalBlocks(rec.CardBlocksJSON)
	if err != nil {
		m.logger.Warn("unmarshal persisted card blocks failed",
			slog.String("slack_user_id", rec.SlackUserID),
			slog.String("inbox_item_id", rec.InboxItemID),
			slog.String("error", err.Error()),
		)
		blocks = nil
	}
	if len(blocks) == 0 && rec.CardFallback != "" {
		blocks = []slack.Block{
			slack.NewSectionBlock(
				slack.NewTextBlockObject(slack.MarkdownType, escapeSlackMrkdwn(rec.CardFallback), false, false),
				nil,
				nil,
			),
		}
	}
	updatedBlocks := MarkBlocksRead(blocks)
	if err := m.dm.UpdateMessage(ctx, rec.SlackChannelID, rec.SlackMessageTS, rec.CardFallback, updatedBlocks); err != nil {
		m.logger.Warn("update Slack DM read state failed",
			slog.String("slack_user_id", rec.SlackUserID),
			slog.String("inbox_item_id", rec.InboxItemID),
			slog.String("error", err.Error()),
		)
	}
}

func (m *Manager) handleAuthFailed(ctx context.Context, slackUserID, reason string) {
	if ctx.Err() != nil {
		return
	}

	m.authMu.Lock()
	user, err := m.store.GetUser(ctx, slackUserID)
	if err != nil {
		m.authMu.Unlock()
		m.logger.Warn("auth failure load user failed",
			slog.String("slack_user_id", slackUserID),
			slog.String("error", err.Error()),
		)
		return
	}
	if user.AuthStatus != store.AuthOK {
		m.authMu.Unlock()
		return
	}
	if err := m.store.SetAuthStatus(ctx, slackUserID, store.AuthFailed); err != nil {
		m.authMu.Unlock()
		m.logger.Warn("set auth_failed status failed",
			slog.String("slack_user_id", slackUserID),
			slog.String("error", err.Error()),
		)
		return
	}
	m.authMu.Unlock()

	m.logger.Warn("Multica WS authentication failed; halting stream and notifying user",
		slog.String("slack_user_id", slackUserID),
		slog.String("reason", reason),
	)

	dmChannelID, err := m.ensureDMChannel(ctx, user)
	if err != nil {
		m.logger.Warn("open DM for auth failure prompt failed",
			slog.String("slack_user_id", slackUserID),
			slog.String("error", err.Error()),
		)
		return
	}
	fallback, blocks := formatAuthFailedDM()
	if _, err := m.dm.PostMessage(ctx, dmChannelID, fallback, blocks); err != nil {
		m.logger.Warn("post auth failure prompt DM failed",
			slog.String("slack_user_id", slackUserID),
			slog.String("error", err.Error()),
		)
	}
}
