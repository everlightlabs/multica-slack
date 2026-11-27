package store_test

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/everlightlabs/multica-slack/internal/crypto"
	"github.com/everlightlabs/multica-slack/internal/store"
	_ "modernc.org/sqlite"
)

func openTestStore(t *testing.T) (*store.Store, string) {
	t.Helper()

	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatalf("generate key: %v", err)
	}
	box, err := crypto.NewBox(key)
	if err != nil {
		t.Fatalf("new box: %v", err)
	}

	dbPath := filepath.Join(t.TempDir(), "multica-slack.db")
	st, err := store.Open(dbPath, box)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})
	return st, dbPath
}

func TestOpenPragmasAndPing(t *testing.T) {
	st, dbPath := openTestStore(t)
	ctx := context.Background()

	if err := st.Ping(ctx); err != nil {
		t.Fatalf("ping: %v", err)
	}

	rawDB, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open raw sqlite: %v", err)
	}
	defer func() { _ = rawDB.Close() }()

	var journalMode string
	if err := rawDB.QueryRow("PRAGMA journal_mode").Scan(&journalMode); err != nil {
		t.Fatalf("query journal_mode: %v", err)
	}
	if !strings.EqualFold(journalMode, "wal") {
		t.Fatalf("expected journal_mode=wal, got %q", journalMode)
	}

	_, err = st.GetUser(ctx, "U_MISSING")
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("expected ErrNotFound for missing user, got %v", err)
	}
}

func TestUsersEncryptionAndDisconnectPreservesPrefs(t *testing.T) {
	st, dbPath := openTestStore(t)
	ctx := context.Background()

	const rawPAT = "mul_secret_pat_12345"
	notifyAfter := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)

	err := st.UpsertMulticaConnection(ctx, store.UpsertMulticaParams{
		SlackUserID:          "U123",
		SlackTeamID:          "T123",
		MulticaUserID:        "mu-1",
		MulticaUserName:      "Alice Smith",
		MulticaWorkspaceID:   "ws-1",
		MulticaWorkspaceSlug: "acme",
		MulticaPAT:           rawPAT,
		NotifyAfter:          notifyAfter,
	})
	if err != nil {
		t.Fatalf("upsert multica connection: %v", err)
	}

	// Verify raw SQLite column stores encrypted value, never plaintext.
	rawDB, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open raw sqlite: %v", err)
	}
	defer func() { _ = rawDB.Close() }()

	var patEnc sql.NullString
	if err := rawDB.QueryRow("SELECT multica_pat_enc FROM users WHERE slack_user_id = ?", "U123").Scan(&patEnc); err != nil {
		t.Fatalf("query raw multica_pat_enc: %v", err)
	}
	if !patEnc.Valid || !strings.HasPrefix(patEnc.String, "v1:") {
		t.Fatalf("expected encrypted multica_pat_enc starting with v1:, got %+v", patEnc)
	}
	if strings.Contains(patEnc.String, rawPAT) {
		t.Fatalf("raw PAT leaked into database column")
	}

	user, err := st.GetUser(ctx, "U123")
	if err != nil {
		t.Fatalf("get user: %v", err)
	}
	if user.MulticaPAT != rawPAT {
		t.Fatalf("expected decrypted PAT %q, got %q", rawPAT, user.MulticaPAT)
	}
	if user.AuthStatus != store.AuthOK || !user.NotificationsEnabled {
		t.Fatalf("unexpected status/enabled: %+v", user)
	}

	// Custom prefs + Slack user token should survive UpsertMulticaConnection updates.
	customPrefs := `{"mentions":true,"assignments":false,"comments":true,"status_changes":false,"agent_activity":true,"system_alerts":true,"updates":true,"new_issues":false,"reactions":false}`
	if err := st.SetNotificationPrefs(ctx, "U123", customPrefs); err != nil {
		t.Fatalf("set notification prefs: %v", err)
	}
	if err := st.SetSlackUserToken(ctx, "U123", "T123", "xoxp-user-secret"); err != nil {
		t.Fatalf("set slack user token: %v", err)
	}
	if err := st.UpsertMulticaConnection(ctx, store.UpsertMulticaParams{
		SlackUserID:          "U123",
		SlackTeamID:          "T123",
		MulticaUserID:        "mu-1",
		MulticaUserName:      "Alice Smith",
		MulticaWorkspaceID:   "ws-2",
		MulticaWorkspaceSlug: "client-ws",
		MulticaPAT:           "mul_rotated_pat",
		NotifyAfter:          notifyAfter.Add(time.Minute),
	}); err != nil {
		t.Fatalf("re-upsert multica connection: %v", err)
	}

	user, err = st.GetUser(ctx, "U123")
	if err != nil {
		t.Fatalf("get user after re-upsert: %v", err)
	}
	if user.NotificationPrefsJSON != customPrefs {
		t.Fatalf("expected prefs preserved %q, got %q", customPrefs, user.NotificationPrefsJSON)
	}
	if user.SlackUserToken != "xoxp-user-secret" {
		t.Fatalf("expected slack user token preserved, got %q", user.SlackUserToken)
	}
	if user.MulticaPAT != "mul_rotated_pat" || user.MulticaWorkspaceSlug != "client-ws" {
		t.Fatalf("expected updated PAT and workspace, got %+v", user)
	}

	if err := st.DisconnectUser(ctx, "U123"); err != nil {
		t.Fatalf("disconnect user: %v", err)
	}

	// Verify DB columns are NULL on disconnect while prefs remain intact.
	var (
		dbPatEnc   sql.NullString
		dbSlackEnc sql.NullString
		dbMultica  sql.NullString
		dbWSID     sql.NullString
		dbWSSlug   sql.NullString
		dbAuth     string
		dbPrefs    string
	)
	if err := rawDB.QueryRow(`
		SELECT multica_pat_enc, slack_user_token_enc, multica_user_id, multica_workspace_id, multica_workspace_slug, auth_status, notification_prefs_json
		FROM users WHERE slack_user_id = ?`, "U123").
		Scan(&dbPatEnc, &dbSlackEnc, &dbMultica, &dbWSID, &dbWSSlug, &dbAuth, &dbPrefs); err != nil {
		t.Fatalf("query disconnected user: %v", err)
	}
	if dbPatEnc.Valid || dbSlackEnc.Valid || dbMultica.Valid || dbWSID.Valid || dbWSSlug.Valid {
		t.Fatalf("expected multica/token columns to be NULL, got pat=%v slack=%v mu=%v ws=%v slug=%v",
			dbPatEnc.Valid, dbSlackEnc.Valid, dbMultica.Valid, dbWSID.Valid, dbWSSlug.Valid)
	}
	if dbAuth != string(store.AuthDisconnected) {
		t.Fatalf("expected auth_status=disconnected, got %q", dbAuth)
	}
	if dbPrefs != customPrefs {
		t.Fatalf("expected notification_prefs_json intact %q, got %q", customPrefs, dbPrefs)
	}
}

func TestSetSlackUserTokenStandaloneRow(t *testing.T) {
	st, dbPath := openTestStore(t)
	ctx := context.Background()

	const rawToken = "xoxp-standalone-token-999"
	if err := st.SetSlackUserToken(ctx, "U_OAUTH_ONLY", "T999", rawToken); err != nil {
		t.Fatalf("set slack user token standalone: %v", err)
	}

	rawDB, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open raw sqlite: %v", err)
	}
	defer func() { _ = rawDB.Close() }()

	var enc string
	if err := rawDB.QueryRow("SELECT slack_user_token_enc FROM users WHERE slack_user_id = ?", "U_OAUTH_ONLY").Scan(&enc); err != nil {
		t.Fatalf("query slack_user_token_enc: %v", err)
	}
	if !strings.HasPrefix(enc, "v1:") || strings.Contains(enc, rawToken) {
		t.Fatalf("expected encrypted slack_user_token_enc, got %q", enc)
	}

	user, err := st.GetUser(ctx, "U_OAUTH_ONLY")
	if err != nil {
		t.Fatalf("get standalone user: %v", err)
	}
	if user.SlackUserToken != rawToken {
		t.Fatalf("expected decrypted token %q, got %q", rawToken, user.SlackUserToken)
	}
	if user.MulticaPAT != "" || user.AuthStatus != store.AuthDisconnected {
		t.Fatalf("expected disconnected OAuth-only user, got %+v", user)
	}

	active, err := st.ListActiveNotificationUsers(ctx)
	if err != nil {
		t.Fatalf("list active users: %v", err)
	}
	if len(active) != 0 {
		t.Fatalf("expected 0 active notification users, got %d", len(active))
	}
}

func TestUserLifecycleHelpersAndCounts(t *testing.T) {
	st, _ := openTestStore(t)
	ctx := context.Background()

	now := time.Date(2026, 11, 26, 14, 0, 0, 0, time.UTC)
	if err := st.UpsertMulticaConnection(ctx, store.UpsertMulticaParams{
		SlackUserID:          "U1",
		SlackTeamID:          "T1",
		MulticaUserID:        "m1",
		MulticaUserName:      "Alice",
		MulticaWorkspaceID:   "w1",
		MulticaWorkspaceSlug: "acme",
		MulticaPAT:           "mul_1",
		NotifyAfter:          now,
	}); err != nil {
		t.Fatalf("upsert U1: %v", err)
	}
	if err := st.UpsertMulticaConnection(ctx, store.UpsertMulticaParams{
		SlackUserID:          "U2",
		SlackTeamID:          "T1",
		MulticaUserID:        "m2",
		MulticaUserName:      "Bob",
		MulticaWorkspaceID:   "w1",
		MulticaWorkspaceSlug: "acme",
		MulticaPAT:           "mul_2",
		NotifyAfter:          now,
	}); err != nil {
		t.Fatalf("upsert U2: %v", err)
	}

	if err := st.SetDMChannel(ctx, "U1", "D100"); err != nil {
		t.Fatalf("set dm channel: %v", err)
	}
	if err := st.SetAuthStatus(ctx, "U2", store.AuthFailed); err != nil {
		t.Fatalf("set auth failed: %v", err)
	}

	linked, failed, err := st.Counts(ctx)
	if err != nil {
		t.Fatalf("counts: %v", err)
	}
	if linked != 2 || failed != 1 {
		t.Fatalf("expected linked=2, failed=1, got linked=%d failed=%d", linked, failed)
	}

	active, err := st.ListActiveNotificationUsers(ctx)
	if err != nil {
		t.Fatalf("list active: %v", err)
	}
	if len(active) != 1 || active[0].SlackUserID != "U1" || active[0].SlackDMChannelID != "D100" {
		t.Fatalf("unexpected active users: %+v", active)
	}

	// Pause U1 (nil notifyAfter keeps watermark), then resume with new watermark.
	if err := st.SetNotificationsEnabled(ctx, "U1", false, nil); err != nil {
		t.Fatalf("pause U1: %v", err)
	}
	active, err = st.ListActiveNotificationUsers(ctx)
	if err != nil || len(active) != 0 {
		t.Fatalf("expected 0 active after pause, got %d (%v)", len(active), err)
	}

	resumedAt := now.Add(10 * time.Minute)
	if err := st.SetNotificationsEnabled(ctx, "U1", true, &resumedAt); err != nil {
		t.Fatalf("resume U1: %v", err)
	}
	if err := st.SwitchWorkspace(ctx, "U1", "w2", "beta", resumedAt.Add(time.Minute)); err != nil {
		t.Fatalf("switch workspace: %v", err)
	}
	u1, err := st.GetUser(ctx, "U1")
	if err != nil {
		t.Fatalf("get U1: %v", err)
	}
	if u1.MulticaWorkspaceID != "w2" || u1.MulticaWorkspaceSlug != "beta" || !u1.NotificationsEnabled {
		t.Fatalf("unexpected U1 after switch: %+v", u1)
	}
}

func TestDeliveryDedupClaimRollbackAndPrune(t *testing.T) {
	st, _ := openTestStore(t)
	ctx := context.Background()

	baseTime := time.Date(2026, 11, 26, 15, 0, 0, 0, time.UTC)

	rec := store.DeliveredNotification{
		SlackUserID:    "U1",
		InboxItemID:    "item-1",
		DeliveryStatus: store.DeliveryDelivered,
		IssueID:        "issue-1",
		CreatedAt:      baseTime,
	}

	claimed, err := st.TryClaimDelivery(ctx, rec)
	if err != nil || !claimed {
		t.Fatalf("first TryClaimDelivery: claimed=%v err=%v", claimed, err)
	}
	claimed, err = st.TryClaimDelivery(ctx, rec)
	if err != nil || claimed {
		t.Fatalf("duplicate TryClaimDelivery: claimed=%v err=%v", claimed, err)
	}

	if err := st.ReleaseDeliveryClaim(ctx, "U1", "item-1"); err != nil {
		t.Fatalf("release claim: %v", err)
	}
	claimed, err = st.TryClaimDelivery(ctx, rec)
	if err != nil || !claimed {
		t.Fatalf("re-claim after release: claimed=%v err=%v", claimed, err)
	}

	// Ruling 1: UpdateDeliveryMessage stores channel, ts, fallback, and blocks JSON.
	const blocksJSON = `[{"type":"section","text":{"type":"mrkdwn","text":"ACME-101"}}]`
	if err := st.UpdateDeliveryMessage(ctx, "U1", "item-1", "D100", "1732633200.000100", "ACME-101 fallback", blocksJSON); err != nil {
		t.Fatalf("update delivery message: %v", err)
	}

	got, err := st.GetUnreadDelivery(ctx, "U1", "item-1")
	if err != nil {
		t.Fatalf("get unread delivery: %v", err)
	}
	if got.SlackChannelID != "D100" || got.SlackMessageTS != "1732633200.000100" ||
		got.CardFallback != "ACME-101 fallback" || got.CardBlocksJSON != blocksJSON {
		t.Fatalf("unexpected unread delivery fields: %+v", got)
	}

	// Insert additional deliveries across timestamps to test since + limit + pruning.
	items := []struct {
		id        string
		status    store.DeliveryStatus
		createdAt time.Time
		read      bool
	}{
		{"item-old-31d", store.DeliveryDelivered, baseTime.Add(-31 * 24 * time.Hour), false},
		{"item-5d", store.DeliveryDelivered, baseTime.Add(-5 * 24 * time.Hour), false},
		{"item-2h", store.DeliveryDelivered, baseTime.Add(-2 * time.Hour), false},
		{"item-1h", store.DeliveryDelivered, baseTime.Add(-1 * time.Hour), false},
		{"item-seeded", store.DeliverySeeded, baseTime.Add(-30 * time.Minute), false},
		{"item-read", store.DeliveryDelivered, baseTime.Add(-10 * time.Minute), true},
	}

	for _, it := range items {
		ok, err := st.TryClaimDelivery(ctx, store.DeliveredNotification{
			SlackUserID:    "U1",
			InboxItemID:    it.id,
			DeliveryStatus: it.status,
			CreatedAt:      it.createdAt,
		})
		if err != nil || !ok {
			t.Fatalf("claim %s: ok=%v err=%v", it.id, ok, err)
		}
		if it.status == store.DeliveryDelivered {
			if err := st.UpdateDeliveryMessage(ctx, "U1", it.id, "D100", "ts-"+it.id, "fb-"+it.id, `[{"type":"divider"}]`); err != nil {
				t.Fatalf("update %s: %v", it.id, err)
			}
		}
		if it.read {
			if err := st.MarkDeliveryRead(ctx, "U1", it.id, baseTime); err != nil {
				t.Fatalf("mark read %s: %v", it.id, err)
			}
		}
	}

	// ListRecentUnreadDeliveries respects since and limit (newest first).
	recent, err := st.ListRecentUnreadDeliveries(ctx, "U1", baseTime.Add(-6*24*time.Hour), 2)
	if err != nil {
		t.Fatalf("list recent unread: %v", err)
	}
	if len(recent) != 2 {
		t.Fatalf("expected 2 recent deliveries, got %d", len(recent))
	}
	if recent[0].InboxItemID != "item-1" || recent[1].InboxItemID != "item-1h" {
		t.Fatalf("unexpected recent ordering: %+v", []string{recent[0].InboxItemID, recent[1].InboxItemID})
	}
	if recent[0].CardFallback != "ACME-101 fallback" || recent[0].CardBlocksJSON != blocksJSON {
		t.Fatalf("expected card fields populated in list, got %+v", recent[0])
	}

	// Prune older than 30 days: deletes item-old-31d, keeps item-5d and newer.
	pruned, err := st.PruneDeliveriesOlderThan(ctx, baseTime.Add(-30*24*time.Hour))
	if err != nil {
		t.Fatalf("prune deliveries: %v", err)
	}
	if pruned != 1 {
		t.Fatalf("expected 1 pruned row, got %d", pruned)
	}
	if _, err := st.GetUnreadDelivery(ctx, "U1", "item-old-31d"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("expected item-old-31d to be pruned, got %v", err)
	}
	if _, err := st.GetUnreadDelivery(ctx, "U1", "item-5d"); err != nil {
		t.Fatalf("expected item-5d to remain, got %v", err)
	}

	// MarkAllDeliveriesRead marks all remaining delivered unread items read.
	if err := st.MarkAllDeliveriesRead(ctx, "U1", baseTime.Add(time.Minute)); err != nil {
		t.Fatalf("mark all read: %v", err)
	}
	remaining, err := st.ListRecentUnreadDeliveries(ctx, "U1", baseTime.Add(-10*24*time.Hour), 10)
	if err != nil {
		t.Fatalf("list remaining: %v", err)
	}
	if len(remaining) != 0 {
		t.Fatalf("expected 0 unread deliveries after MarkAllDeliveriesRead, got %d", len(remaining))
	}
	if _, err := st.GetUnreadDelivery(ctx, "U1", "item-1"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("expected ErrNotFound for read delivery, got %v", err)
	}
}

func TestOAuthStateSingleUseAndExpiry(t *testing.T) {
	st, _ := openTestStore(t)
	ctx := context.Background()

	now := time.Date(2026, 11, 26, 16, 0, 0, 0, time.UTC)

	if err := st.CreateOAuthState(ctx, "state-valid", "U1", now.Add(10*time.Minute)); err != nil {
		t.Fatalf("create valid oauth state: %v", err)
	}
	uid, err := st.ConsumeOAuthState(ctx, "state-valid", now.Add(2*time.Minute))
	if err != nil || uid != "U1" {
		t.Fatalf("first consume: uid=%q err=%v", uid, err)
	}
	if _, err := st.ConsumeOAuthState(ctx, "state-valid", now.Add(3*time.Minute)); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("replay consume should return ErrNotFound, got %v", err)
	}

	if err := st.CreateOAuthState(ctx, "state-expired", "U2", now.Add(5*time.Minute)); err != nil {
		t.Fatalf("create expired oauth state: %v", err)
	}
	if _, err := st.ConsumeOAuthState(ctx, "state-expired", now.Add(6*time.Minute)); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("expired state should return ErrNotFound, got %v", err)
	}

	// PruneExpiredOAuthStates deletes expired rows and preserves live rows.
	if err := st.CreateOAuthState(ctx, "state-old-1", "U3", now.Add(-2*time.Minute)); err != nil {
		t.Fatalf("create state-old-1: %v", err)
	}
	if err := st.CreateOAuthState(ctx, "state-old-2", "U4", now.Add(-1*time.Second)); err != nil {
		t.Fatalf("create state-old-2: %v", err)
	}
	if err := st.CreateOAuthState(ctx, "state-live", "U5", now.Add(5*time.Minute)); err != nil {
		t.Fatalf("create state-live: %v", err)
	}

	pruned, err := st.PruneExpiredOAuthStates(ctx, now)
	if err != nil {
		t.Fatalf("prune expired oauth states: %v", err)
	}
	if pruned != 2 {
		t.Fatalf("expected 2 pruned oauth states, got %d", pruned)
	}
	if _, err := st.ConsumeOAuthState(ctx, "state-old-1", now.Add(-3*time.Minute)); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("expected state-old-1 deleted by prune, got %v", err)
	}
	uid, err = st.ConsumeOAuthState(ctx, "state-live", now)
	if err != nil || uid != "U5" {
		t.Fatalf("expected state-live preserved for U5, got uid=%q err=%v", uid, err)
	}
}
