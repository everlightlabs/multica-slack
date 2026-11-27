package notify

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/everlightlabs/multica-slack/internal/crypto"
	"github.com/everlightlabs/multica-slack/internal/multica"
	"github.com/everlightlabs/multica-slack/internal/prefs"
	"github.com/everlightlabs/multica-slack/internal/store"
	"github.com/slack-go/slack"
)

type postedDM struct {
	ChannelID string
	TS        string
	Fallback  string
	Blocks    []slack.Block
}

type updatedDM struct {
	ChannelID string
	TS        string
	Fallback  string
	Blocks    []slack.Block
}

type mockSlackDM struct {
	mu           sync.Mutex
	openCalls    int
	posts        []postedDM
	updates      []updatedDM
	failNextPost int
	postErr      error
	onUpdate     func(ctx context.Context, channelID, ts string) error
	postCh       chan postedDM
	updateCh     chan updatedDM
	tsSeq        int
}

func newMockSlackDM() *mockSlackDM {
	return &mockSlackDM{
		postCh:   make(chan postedDM, 64),
		updateCh: make(chan updatedDM, 64),
	}
}

func (m *mockSlackDM) OpenDM(_ context.Context, slackUserID string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.openCalls++
	return "D_" + slackUserID, nil
}

func (m *mockSlackDM) PostMessage(_ context.Context, channelID, fallbackText string, blocks []slack.Block) (string, error) {
	m.mu.Lock()
	if m.failNextPost > 0 {
		m.failNextPost--
		err := m.postErr
		if err == nil {
			err = errors.New("slack transient error")
		}
		m.mu.Unlock()
		return "", err
	}
	m.tsSeq++
	ts := fmt.Sprintf("1700000000.%06d", m.tsSeq)
	rec := postedDM{
		ChannelID: channelID,
		TS:        ts,
		Fallback:  fallbackText,
		Blocks:    blocks,
	}
	m.posts = append(m.posts, rec)
	m.mu.Unlock()

	select {
	case m.postCh <- rec:
	default:
	}
	return ts, nil
}

func (m *mockSlackDM) UpdateMessage(ctx context.Context, channelID, ts, fallbackText string, blocks []slack.Block) error {
	m.mu.Lock()
	hook := m.onUpdate
	m.mu.Unlock()
	if hook != nil {
		if err := hook(ctx, channelID, ts); err != nil {
			return err
		}
	}

	rec := updatedDM{
		ChannelID: channelID,
		TS:        ts,
		Fallback:  fallbackText,
		Blocks:    blocks,
	}
	m.mu.Lock()
	m.updates = append(m.updates, rec)
	m.mu.Unlock()

	select {
	case m.updateCh <- rec:
	default:
	}
	return nil
}

func (m *mockSlackDM) postCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.posts)
}

func (m *mockSlackDM) updateCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.updates)
}

func (m *mockSlackDM) getPosts() []postedDM {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]postedDM, len(m.posts))
	copy(out, m.posts)
	return out
}

type fakeMulticaServer struct {
	srv *httptest.Server

	mu            sync.Mutex
	inboxItems    []multica.InboxItem
	onListInbox   func(callNum int)
	inboxCalls    int
	authFail      bool
	wsAttempts    atomic.Int32
	wsOut         chan any
	wsDrop        chan struct{}
	connected     chan struct{}
}

func newFakeMulticaServer(t *testing.T) *fakeMulticaServer {
	t.Helper()
	f := &fakeMulticaServer{
		wsOut:     make(chan any, 32),
		wsDrop:    make(chan struct{}, 8),
		connected: make(chan struct{}, 16),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/inbox", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.inboxCalls++
		callNum := f.inboxCalls
		cb := f.onListInbox
		items := make([]multica.InboxItem, len(f.inboxItems))
		copy(items, f.inboxItems)
		f.mu.Unlock()
		if cb != nil {
			cb(callNum)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(items)
	})
	mux.HandleFunc("/api/workspaces/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]multica.Member{
			{ID: "mem-1", UserID: "user-alice", Name: "Alice"},
		})
	})
	mux.HandleFunc("/api/agents", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]multica.Agent{
			{ID: "agent-1", Name: "Polaris"},
		})
	})
	mux.HandleFunc("/api/squads", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]multica.Squad{})
	})
	mux.HandleFunc("/api/projects", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]multica.Project{
			{ID: "proj-1", Name: "Core Platform"},
		})
	})
	mux.HandleFunc("/api/issue-statuses", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]multica.IssueStatus{
			{ID: "st-1", Key: "in_progress", Name: "In Progress", Category: "started"},
		})
	})
	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		f.wsAttempts.Add(1)
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.CloseNow() }()

		var authFrame json.RawMessage
		if err := wsjson.Read(r.Context(), conn, &authFrame); err != nil {
			return
		}

		f.mu.Lock()
		fail := f.authFail
		f.mu.Unlock()
		if fail {
			_ = wsjson.Write(r.Context(), conn, map[string]any{
				"type":    "auth_error",
				"payload": map[string]any{"message": "revoked token"},
			})
			_ = conn.Close(websocket.StatusNormalClosure, "revoked")
			return
		}

		if err := wsjson.Write(r.Context(), conn, map[string]any{"type": "auth_ack"}); err != nil {
			return
		}
		select {
		case f.connected <- struct{}{}:
		default:
		}

		readDone := make(chan struct{})
		go func() {
			defer close(readDone)
			for {
				var env multica.WSEnvelope
				if err := wsjson.Read(r.Context(), conn, &env); err != nil {
					return
				}
				if env.Type == "ping" {
					_ = wsjson.Write(r.Context(), conn, map[string]any{"type": "pong"})
				}
			}
		}()

		for {
			select {
			case <-r.Context().Done():
				return
			case <-readDone:
				return
			case <-f.wsDrop:
				_ = conn.Close(websocket.StatusGoingAway, "simulated drop")
				return
			case msg := <-f.wsOut:
				if err := wsjson.Write(r.Context(), conn, msg); err != nil {
					return
				}
			}
		}
	})

	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeMulticaServer) setInbox(items []multica.InboxItem) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.inboxItems = items
}

func (f *fakeMulticaServer) sendEvent(eventType string, payload any) {
	f.wsOut <- map[string]any{
		"type":    eventType,
		"payload": payload,
	}
}

func (f *fakeMulticaServer) dropWS() {
	select {
	case f.wsDrop <- struct{}{}:
	default:
	}
}

func openTestStore(t *testing.T) *store.Store {
	t.Helper()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatalf("rand key: %v", err)
	}
	box, err := crypto.NewBox(key)
	if err != nil {
		t.Fatalf("new box: %v", err)
	}
	dbPath := filepath.Join(t.TempDir(), "test.db")
	st, err := store.Open(dbPath, box)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func waitForCondition(t *testing.T, timeout time.Duration, desc string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", desc)
}

func findButtons(blocks []slack.Block) []*slack.ButtonBlockElement {
	var out []*slack.ButtonBlockElement
	for _, b := range blocks {
		ab, ok := b.(*slack.ActionBlock)
		if !ok || ab == nil || ab.Elements == nil {
			continue
		}
		for _, el := range ab.Elements.ElementSet {
			if btn, ok := el.(*slack.ButtonBlockElement); ok {
				out = append(out, btn)
			}
		}
	}
	return out
}

func hasContextText(blocks []slack.Block, substr string) int {
	count := 0
	for _, b := range blocks {
		cb, ok := b.(*slack.ContextBlock)
		if !ok || cb == nil {
			continue
		}
		for _, el := range cb.ContextElements.Elements {
			if txt, ok := el.(*slack.TextBlockObject); ok && strings.Contains(txt.Text, substr) {
				count++
			}
		}
	}
	return count
}

func TestFormatNotificationBlocksAndMarkBlocksRead(t *testing.T) {
	longBody := strings.Repeat("a", 350) + " <script>&</script>"
	item := multica.InboxItem{
		ID:              "inbox-1",
		WorkspaceID:     "ws-1",
		Type:            "mentioned",
		Title:           "Fix A & B <urgent>",
		Body:            longBody,
		IssueID:         "issue-uuid-1",
		IssueIdentifier: "ACME-42",
		IssueStatus:     "in_progress",
		IssuePriority:   "high",
		IssueProjectID:  "proj-1",
		ActorType:       "member",
		ActorID:         "user-alice",
	}
	meta := multica.WorkspaceMeta{
		MembersByUserID: map[string]multica.Member{"user-alice": {UserID: "user-alice", Name: "Alice"}},
		ProjectsByID:    map[string]multica.Project{"proj-1": {ID: "proj-1", Name: "Core"}},
		StatusesByKey:   map[string]multica.IssueStatus{"in_progress": {Key: "in_progress", Name: "In Progress"}},
	}

	fallback, blocks := FormatNotificationBlocks("https://multica.ai", "acme", item, meta, false)
	if fallback == "" {
		t.Fatal("expected non-empty fallback")
	}

	// Verify escaping in header and body truncation <= 300 chars.
	var sectionTexts []string
	for _, b := range blocks {
		if sb, ok := b.(*slack.SectionBlock); ok && sb.Text != nil {
			sectionTexts = append(sectionTexts, sb.Text.Text)
		}
	}
	if len(sectionTexts) < 2 {
		t.Fatalf("expected header + body sections, got %d", len(sectionTexts))
	}
	if !strings.Contains(sectionTexts[0], "Fix A &amp; B &lt;urgent&gt;") {
		t.Fatalf("expected escaped title in header, got %q", sectionTexts[0])
	}
	if len([]rune(sectionTexts[1])) > 300 {
		t.Fatalf("expected truncated body quote <= 300 runes, got %d", len([]rune(sectionTexts[1])))
	}
	if !strings.HasSuffix(sectionTexts[1], "...") {
		t.Fatalf("expected truncated body quote to end with ..., got %q", sectionTexts[1])
	}

	// Verify buttons and value encodings.
	btns := findButtons(blocks)
	if len(btns) != 3 {
		t.Fatalf("expected 3 buttons (Open, Mark Read, Reply), got %d", len(btns))
	}
	var gotOpen, gotMarkRead, gotReply *slack.ButtonBlockElement
	for _, btn := range btns {
		switch {
		case btn.URL != "":
			gotOpen = btn
		case btn.ActionID == ActionMarkRead:
			gotMarkRead = btn
		case btn.ActionID == ActionOpenReply:
			gotReply = btn
		}
	}
	if gotOpen == nil || gotOpen.URL != "https://multica.ai/acme/issues/ACME-42" {
		t.Fatalf("unexpected Open button: %+v", gotOpen)
	}
	if gotMarkRead == nil || gotMarkRead.Value != "inbox-1|ws-1" {
		t.Fatalf("unexpected Mark Read button: %+v", gotMarkRead)
	}
	if gotReply == nil || gotReply.Value != "issue-uuid-1|ACME-42|inbox-1|ws-1" {
		t.Fatalf("unexpected Reply button: %+v", gotReply)
	}

	// Roundtrip JSON + MarkBlocksRead idempotency.
	raw, err := MarshalBlocks(blocks)
	if err != nil {
		t.Fatalf("MarshalBlocks: %v", err)
	}
	decoded, err := UnmarshalBlocks(raw)
	if err != nil {
		t.Fatalf("UnmarshalBlocks: %v", err)
	}
	readOnce := MarkBlocksRead(decoded)
	for _, btn := range findButtons(readOnce) {
		if btn.ActionID == ActionMarkRead {
			t.Fatal("expected Mark Read button to be stripped")
		}
	}
	if len(findButtons(readOnce)) != 2 {
		t.Fatalf("expected Open and Reply buttons to remain, got %d", len(findButtons(readOnce)))
	}
	if got := hasContextText(readOnce, "✓ Read"); got != 1 {
		t.Fatalf("expected 1 ✓ Read context element, got %d", got)
	}

	// Calling MarkBlocksRead twice must not duplicate ✓ Read.
	readTwice := MarkBlocksRead(readOnce)
	if got := hasContextText(readTwice, "✓ Read"); got != 1 {
		t.Fatalf("expected MarkBlocksRead to be idempotent (1 ✓ Read), got %d", got)
	}

	// FormatNotificationBlocks(..., true) must match MarkBlocksRead output.
	_, directReadBlocks := FormatNotificationBlocks("https://multica.ai", "acme", item, meta, true)
	directJSON, err := MarshalBlocks(directReadBlocks)
	if err != nil {
		t.Fatalf("MarshalBlocks(directRead): %v", err)
	}
	transformedJSON, err := MarshalBlocks(readOnce)
	if err != nil {
		t.Fatalf("MarshalBlocks(readOnce): %v", err)
	}
	if directJSON != transformedJSON {
		t.Fatalf("expected FormatNotificationBlocks(..., true) to match MarkBlocksRead:\ndirect:      %s\ntransformed: %s", directJSON, transformedJSON)
	}

	// Short body must escape &, <, > in Slack mrkdwn.
	escapeItem := item
	escapeItem.Body = "Check A & B <urgent> > done"
	_, escBlocks := FormatNotificationBlocks("https://multica.ai", "acme", escapeItem, meta, false)
	var escBody string
	for _, b := range escBlocks {
		if sb, ok := b.(*slack.SectionBlock); ok && sb.Text != nil && strings.HasPrefix(sb.Text.Text, "> ") {
			escBody = sb.Text.Text
		}
	}
	if escBody != "> Check A &amp; B &lt;urgent&gt; &gt; done" {
		t.Fatalf("expected escaped body quote, got %q", escBody)
	}

	// Item without IssueID must omit Reply button; if actions block becomes empty on MarkBlocksRead, drop it.
	noIssueItem := multica.InboxItem{
		ID:          "inbox-sys",
		WorkspaceID: "ws-1",
		Type:        "autopilot_paused",
		Title:       "Autopilot paused",
	}
	_, sysBlocks := FormatNotificationBlocks("", "", noIssueItem, meta, false)
	for _, btn := range findButtons(sysBlocks) {
		if btn.ActionID == ActionOpenReply {
			t.Fatal("expected Reply button omitted when IssueID is empty")
		}
	}
	sysRead := MarkBlocksRead(sysBlocks)
	for _, b := range sysRead {
		if _, ok := b.(*slack.ActionBlock); ok {
			t.Fatal("expected empty ActionBlock to be dropped by MarkBlocksRead")
		}
	}
	if got := hasContextText(sysRead, "✓ Read"); got != 1 {
		t.Fatalf("expected ✓ Read context element on system card, got %d", got)
	}
}

func TestConnectSnapshotsUnreadWithoutFlooding(t *testing.T) {
	st := openTestStore(t)
	fake := newFakeMulticaServer(t)
	dm := newMockSlackDM()
	mc := multica.NewClient(fake.srv.URL, fake.srv.Client())
	cache := multica.NewMetadataCache(mc, time.Minute)

	now := time.Now().UTC()
	fake.setInbox([]multica.InboxItem{
		{ID: "pre-1", WorkspaceID: "ws-1", Type: "mentioned", Title: "Old 1", CreatedAt: now.Add(-10 * time.Minute)},
		{ID: "pre-2", WorkspaceID: "ws-1", Type: "issue_assigned", Title: "Old 2", CreatedAt: now.Add(-5 * time.Minute)},
		{ID: "pre-3", WorkspaceID: "ws-1", Type: "new_comment", Title: "Old 3", CreatedAt: now.Add(-1 * time.Minute)},
	})

	ctx := context.Background()
	if err := st.UpsertMulticaConnection(ctx, store.UpsertMulticaParams{
		SlackUserID:          "U1",
		SlackTeamID:          "T1",
		MulticaUserID:        "mu-1",
		MulticaUserName:      "Alice",
		MulticaWorkspaceID:   "ws-1",
		MulticaWorkspaceSlug: "acme",
		MulticaPAT:           "mul_valid",
		NotifyAfter:          now.Add(-time.Hour),
	}); err != nil {
		t.Fatalf("upsert user: %v", err)
	}

	mgr := NewManager(ManagerConfig{
		MulticaAPIURL:    fake.srv.URL,
		MulticaAppURL:    "https://multica.ai",
		Version:          "test",
		BatchUpdateDelay: time.Millisecond,
		WSInitialBackoff: 10 * time.Millisecond,
		WSMaxBackoff:     50 * time.Millisecond,
	}, st, mc, cache, dm, testLogger())
	defer mgr.Stop()

	if err := mgr.ConnectUser(ctx, "U1"); err != nil {
		t.Fatalf("ConnectUser: %v", err)
	}

	select {
	case <-fake.connected:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for WS connect")
	}
	waitForCondition(t, 2*time.Second, "IsConnected(U1)", func() bool {
		return mgr.IsConnected("U1") && mgr.ConnectedCount() == 1
	})

	// All 3 pre-existing items must be seeded (TryClaimDelivery returns false) and 0 DMs sent.
	for _, id := range []string{"pre-1", "pre-2", "pre-3"} {
		claimed, err := st.TryClaimDelivery(ctx, store.DeliveredNotification{
			SlackUserID:    "U1",
			InboxItemID:    id,
			DeliveryStatus: store.DeliveryDelivered,
		})
		if err != nil {
			t.Fatalf("TryClaimDelivery(%s): %v", id, err)
		}
		if claimed {
			t.Fatalf("expected %s to already be seeded in delivered_notifications", id)
		}
		// Ensure it is not counted as an unread 'delivered' row (it must be 'seeded').
		if _, err := st.GetUnreadDelivery(ctx, "U1", id); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("expected seeded item %s not returned by GetUnreadDelivery, got err=%v", id, err)
		}
	}
	if dm.postCount() != 0 {
		t.Fatalf("expected 0 DMs after connect snapshot, got %d", dm.postCount())
	}

	// Send a live inbox:new event afterwards -> 1 DM with Open, Mark Read, Reply buttons.
	fake.sendEvent("inbox:new", map[string]any{
		"item": multica.InboxItem{
			ID:              "live-1",
			WorkspaceID:     "ws-1",
			Type:            "mentioned",
			Title:           "Deploy Slack App",
			Body:            "Can you review this?",
			IssueID:         "iss-100",
			IssueIdentifier: "ACME-100",
			IssueStatus:     "in_progress",
			IssuePriority:   "high",
			IssueProjectID:  "proj-1",
			ActorType:       "member",
			ActorID:         "user-alice",
			CreatedAt:       time.Now().UTC(),
		},
	})

	select {
	case msg := <-dm.postCh:
		btns := findButtons(msg.Blocks)
		if len(btns) != 3 {
			t.Fatalf("expected 3 buttons on live DM, got %d", len(btns))
		}
		var hasOpen, hasMarkRead, hasReply bool
		for _, btn := range btns {
			if btn.URL != "" {
				hasOpen = true
			}
			if btn.ActionID == ActionMarkRead && btn.Value == "live-1|ws-1" {
				hasMarkRead = true
			}
			if btn.ActionID == ActionOpenReply && btn.Value == "iss-100|ACME-100|live-1|ws-1" {
				hasReply = true
			}
		}
		if !hasOpen || !hasMarkRead || !hasReply {
			t.Fatalf("missing expected buttons: open=%v markRead=%v reply=%v", hasOpen, hasMarkRead, hasReply)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for live DM")
	}

	// Verify DM channel ID was cached on user and card_blocks_json persisted.
	u, err := st.GetUser(ctx, "U1")
	if err != nil {
		t.Fatalf("GetUser: %v", err)
	}
	if u.SlackDMChannelID != "D_U1" {
		t.Fatalf("expected cached SlackDMChannelID D_U1, got %q", u.SlackDMChannelID)
	}
	rec, err := st.GetUnreadDelivery(ctx, "U1", "live-1")
	if err != nil {
		t.Fatalf("GetUnreadDelivery(live-1): %v", err)
	}
	if rec.CardBlocksJSON == "" || rec.CardFallback == "" {
		t.Fatalf("expected CardBlocksJSON and CardFallback stored, got fallback=%q blocks=%q", rec.CardFallback, rec.CardBlocksJSON)
	}
}

func TestPauseResumeAndMutedGroupNoBackfill(t *testing.T) {
	st := openTestStore(t)
	fake := newFakeMulticaServer(t)
	dm := newMockSlackDM()
	mc := multica.NewClient(fake.srv.URL, fake.srv.Client())
	cache := multica.NewMetadataCache(mc, time.Minute)

	ctx := context.Background()
	now := time.Now().UTC()
	if err := st.UpsertMulticaConnection(ctx, store.UpsertMulticaParams{
		SlackUserID:          "U2",
		SlackTeamID:          "T1",
		MulticaUserID:        "mu-2",
		MulticaUserName:      "Bob",
		MulticaWorkspaceID:   "ws-1",
		MulticaWorkspaceSlug: "acme",
		MulticaPAT:           "mul_valid",
		NotifyAfter:          now.Add(-time.Minute),
	}); err != nil {
		t.Fatalf("upsert user: %v", err)
	}

	mgr := NewManager(ManagerConfig{
		MulticaAPIURL:    fake.srv.URL,
		MulticaAppURL:    "https://multica.ai",
		Version:          "test",
		BatchUpdateDelay: time.Millisecond,
		WSInitialBackoff: 15 * time.Millisecond,
		WSMaxBackoff:     50 * time.Millisecond,
	}, st, mc, cache, dm, testLogger())
	defer mgr.Stop()

	if err := mgr.ConnectUser(ctx, "U2"); err != nil {
		t.Fatalf("ConnectUser: %v", err)
	}
	select {
	case <-fake.connected:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for connect")
	}

	// (a) Default prefs have reactions=false. Send reaction_added over WS.
	reactionItem := multica.InboxItem{
		ID:          "react-1",
		WorkspaceID: "ws-1",
		Type:        "reaction_added",
		Title:       "Reacted with thumbsup",
		CreatedAt:   time.Now().UTC(),
	}
	fake.setInbox([]multica.InboxItem{reactionItem})
	fake.sendEvent("inbox:new", map[string]any{"item": reactionItem})

	// Wait until reaction_added is recorded as skipped in delivered_notifications.
	waitForCondition(t, 2*time.Second, "reaction_added skipped in DB", func() bool {
		claimed, err := st.TryClaimDelivery(ctx, store.DeliveredNotification{
			SlackUserID:    "U2",
			InboxItemID:    "react-1",
			DeliveryStatus: store.DeliveryDelivered,
		})
		if err != nil {
			return false
		}
		if claimed {
			_ = st.ReleaseDeliveryClaim(ctx, "U2", "react-1")
			return false
		}
		return true
	})
	if dm.postCount() != 0 {
		t.Fatalf("expected 0 DMs for muted reaction_added, got %d", dm.postCount())
	}

	// Enable reactions and trigger reconnect catch-up -> still 0 DMs for react-1.
	p := prefs.Default()
	p[prefs.GroupReactions] = true
	prefsBytes, _ := json.Marshal(p)
	if err := st.SetNotificationPrefs(ctx, "U2", string(prefsBytes)); err != nil {
		t.Fatalf("SetNotificationPrefs: %v", err)
	}
	fake.dropWS()
	select {
	case <-fake.connected:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for reconnect after enabling reactions")
	}
	time.Sleep(80 * time.Millisecond)
	if dm.postCount() != 0 {
		t.Fatalf("expected 0 DMs after toggling reactions ON and reconnecting, got %d", dm.postCount())
	}

	// (b) Pause user, 2 mentions arrive in /api/inbox, ResumeUser -> both seeded, 0 DMs.
	if err := mgr.PauseUser(ctx, "U2"); err != nil {
		t.Fatalf("PauseUser: %v", err)
	}
	if mgr.IsConnected("U2") {
		t.Fatal("expected user disconnected after PauseUser")
	}

	pausedTime := time.Now().UTC()
	fake.setInbox([]multica.InboxItem{
		{ID: "paused-m1", WorkspaceID: "ws-1", Type: "mentioned", Title: "Mention 1", CreatedAt: pausedTime},
		{ID: "paused-m2", WorkspaceID: "ws-1", Type: "mentioned", Title: "Mention 2", CreatedAt: pausedTime},
	})

	if err := mgr.ResumeUser(ctx, "U2"); err != nil {
		t.Fatalf("ResumeUser: %v", err)
	}
	select {
	case <-fake.connected:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for reconnect after ResumeUser")
	}
	time.Sleep(80 * time.Millisecond)

	if dm.postCount() != 0 {
		t.Fatalf("expected 0 DMs after ResumeUser (no backfill), got %d", dm.postCount())
	}
	for _, id := range []string{"paused-m1", "paused-m2"} {
		claimed, err := st.TryClaimDelivery(ctx, store.DeliveredNotification{
			SlackUserID:    "U2",
			InboxItemID:    id,
			DeliveryStatus: store.DeliveryDelivered,
		})
		if err != nil {
			t.Fatalf("TryClaimDelivery(%s): %v", id, err)
		}
		if claimed {
			t.Fatalf("expected %s to be seeded during ResumeUser", id)
		}
	}
}

func TestReconnectCatchUpAndTransientSlackRollback(t *testing.T) {
	st := openTestStore(t)
	fake := newFakeMulticaServer(t)
	dm := newMockSlackDM()
	mc := multica.NewClient(fake.srv.URL, fake.srv.Client())
	cache := multica.NewMetadataCache(mc, time.Minute)

	ctx := context.Background()
	now := time.Now().UTC()
	if err := st.UpsertMulticaConnection(ctx, store.UpsertMulticaParams{
		SlackUserID:          "U3",
		SlackTeamID:          "T1",
		MulticaUserID:        "mu-3",
		MulticaUserName:      "Alice",
		MulticaWorkspaceID:   "ws-1",
		MulticaWorkspaceSlug: "acme",
		MulticaPAT:           "mul_valid",
		NotifyAfter:          now.Add(-15 * time.Minute),
	}); err != nil {
		t.Fatalf("upsert user: %v", err)
	}

	// Eligible item (5m ago, after notify_after) + ineligible item (30m ago, before notify_after).
	fake.setInbox([]multica.InboxItem{
		{
			ID:              "catchup-1",
			WorkspaceID:     "ws-1",
			Type:            "mentioned",
			Title:           "Missed while offline",
			IssueID:         "iss-3",
			IssueIdentifier: "ACME-3",
			CreatedAt:       now.Add(-5 * time.Minute),
		},
		{
			ID:          "too-old",
			WorkspaceID: "ws-1",
			Type:        "mentioned",
			Title:       "Before watermark",
			CreatedAt:   now.Add(-30 * time.Minute),
		},
	})

	// Fail the first PostMessage attempt transiently.
	dm.mu.Lock()
	dm.failNextPost = 1
	dm.mu.Unlock()

	mgr := NewManager(ManagerConfig{
		MulticaAPIURL:    fake.srv.URL,
		MulticaAppURL:    "https://multica.ai",
		Version:          "test",
		BatchUpdateDelay: time.Millisecond,
		WSInitialBackoff: 15 * time.Millisecond,
		WSMaxBackoff:     50 * time.Millisecond,
	}, st, mc, cache, dm, testLogger())
	defer mgr.Stop()

	if err := mgr.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	select {
	case <-fake.connected:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for initial connect")
	}

	// Wait briefly for catch-up attempt (which fails PostMessage and releases claim).
	time.Sleep(100 * time.Millisecond)
	if dm.postCount() != 0 {
		t.Fatalf("expected 0 successful DMs after transient failure, got %d", dm.postCount())
	}

	// Drop WS to trigger reconnect catch-up -> must retry catchup-1 and succeed once!
	fake.dropWS()
	select {
	case <-fake.connected:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for reconnect")
	}

	select {
	case msg := <-dm.postCh:
		if !strings.Contains(msg.Fallback, "ACME-3") {
			t.Fatalf("expected catchup-1 fallback containing ACME-3, got %q", msg.Fallback)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for retried catch-up DM")
	}

	// Trigger one more reconnect -> catchup-1 must NOT be delivered a second time.
	fake.dropWS()
	select {
	case <-fake.connected:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for second reconnect")
	}
	time.Sleep(80 * time.Millisecond)

	if dm.postCount() != 1 {
		t.Fatalf("expected exactly 1 delivered DM across retries/reconnects, got %d", dm.postCount())
	}
}

func TestReadArchivedAndBatchReadSync(t *testing.T) {
	st := openTestStore(t)
	fake := newFakeMulticaServer(t)
	dm := newMockSlackDM()
	mc := multica.NewClient(fake.srv.URL, fake.srv.Client())
	cache := multica.NewMetadataCache(mc, time.Minute)

	ctx := context.Background()
	now := time.Now().UTC()
	if err := st.UpsertMulticaConnection(ctx, store.UpsertMulticaParams{
		SlackUserID:          "U4",
		SlackTeamID:          "T1",
		MulticaUserID:        "mu-4",
		MulticaUserName:      "Alice",
		MulticaWorkspaceID:   "ws-1",
		MulticaWorkspaceSlug: "acme",
		MulticaPAT:           "mul_valid",
		NotifyAfter:          now,
	}); err != nil {
		t.Fatalf("upsert user: %v", err)
	}

	mgr := NewManager(ManagerConfig{
		MulticaAPIURL:    fake.srv.URL,
		MulticaAppURL:    "https://multica.ai",
		Version:          "test",
		BatchUpdateDelay: time.Millisecond,
		WSInitialBackoff: 15 * time.Millisecond,
		WSMaxBackoff:     50 * time.Millisecond,
	}, st, mc, cache, dm, testLogger())
	defer mgr.Stop()

	if err := mgr.ConnectUser(ctx, "U4"); err != nil {
		t.Fatalf("ConnectUser: %v", err)
	}
	select {
	case <-fake.connected:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for WS connect")
	}

	// Deliver 2 items over WS and verify DM channel is opened once and cached thereafter.
	for _, id := range []string{"read-1", "arch-2"} {
		fake.sendEvent("inbox:new", map[string]any{
			"item": multica.InboxItem{
				ID:              id,
				WorkspaceID:     "ws-1",
				Type:            "mentioned",
				Title:           "Item " + id,
				IssueID:         "iss-" + id,
				IssueIdentifier: "ACME-9",
				CreatedAt:       time.Now().UTC(),
			},
		})
		select {
		case <-dm.postCh:
		case <-time.After(3 * time.Second):
			t.Fatalf("timed out waiting for DM for %s", id)
		}
	}
	dm.mu.Lock()
	openCalls := dm.openCalls
	dm.mu.Unlock()
	if openCalls != 1 {
		t.Fatalf("expected OpenDM called once and cached on user, got %d", openCalls)
	}

	// Send inbox:read and inbox:archived -> both update message to ✓ Read and mark DB read.
	fake.sendEvent("inbox:read", map[string]any{"item_id": "read-1", "recipient_id": "mu-4"})
	fake.sendEvent("inbox:archived", map[string]any{"item_id": "arch-2", "recipient_id": "mu-4"})

	for i := 0; i < 2; i++ {
		select {
		case upd := <-dm.updateCh:
			for _, btn := range findButtons(upd.Blocks) {
				if btn.ActionID == ActionMarkRead {
					t.Fatalf("expected Mark Read button removed on update %+v", upd)
				}
			}
			if got := hasContextText(upd.Blocks, "✓ Read"); got != 1 {
				t.Fatalf("expected ✓ Read on updated DM, got count=%d", got)
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("timed out waiting for UpdateMessage %d", i+1)
		}
	}

	waitForCondition(t, 2*time.Second, "read-1 and arch-2 marked read in DB", func() bool {
		_, err1 := st.GetUnreadDelivery(ctx, "U4", "read-1")
		_, err2 := st.GetUnreadDelivery(ctx, "U4", "arch-2")
		return errors.Is(err1, store.ErrNotFound) && errors.Is(err2, store.ErrNotFound)
	})

	// Seed 25 recent unread delivered notifications (within 7d) + 3 older ones (10d ago).
	sampleFallback, sampleBlocks := FormatNotificationBlocks("https://multica.ai", "acme", multica.InboxItem{
		ID:          "sample",
		WorkspaceID: "ws-1",
		Type:        "mentioned",
		Title:       "Batch item",
	}, multica.WorkspaceMeta{}, false)
	sampleJSON, err := MarshalBlocks(sampleBlocks)
	if err != nil {
		t.Fatalf("MarshalBlocks: %v", err)
	}

	for i := 0; i < 25; i++ {
		id := fmt.Sprintf("batch-recent-%02d", i)
		ok, err := st.TryClaimDelivery(ctx, store.DeliveredNotification{
			SlackUserID:    "U4",
			InboxItemID:    id,
			DeliveryStatus: store.DeliveryDelivered,
			SlackChannelID: "D_U4",
			SlackMessageTS: fmt.Sprintf("1700001000.%06d", i),
			CardFallback:   sampleFallback,
			CardBlocksJSON: sampleJSON,
			CreatedAt:      now.Add(-time.Duration(i+1) * time.Minute),
		})
		if err != nil || !ok {
			t.Fatalf("seed recent %s: ok=%v err=%v", id, ok, err)
		}
	}
	for i := 0; i < 3; i++ {
		id := fmt.Sprintf("batch-old-%02d", i)
		ok, err := st.TryClaimDelivery(ctx, store.DeliveredNotification{
			SlackUserID:    "U4",
			InboxItemID:    id,
			DeliveryStatus: store.DeliveryDelivered,
			SlackChannelID: "D_U4",
			SlackMessageTS: fmt.Sprintf("1690000000.%06d", i),
			CardFallback:   sampleFallback,
			CardBlocksJSON: sampleJSON,
			CreatedAt:      now.Add(-10 * 24 * time.Hour),
		})
		if err != nil || !ok {
			t.Fatalf("seed old %s: ok=%v err=%v", id, ok, err)
		}
	}

	beforeBatchUpdates := dm.updateCount()
	fake.sendEvent("inbox:batch-read", map[string]any{"recipient_id": "mu-4", "count": 28})

	// Wait for all DB rows to be marked read and all 20 async Slack DM updates to complete.
	waitForCondition(t, 4*time.Second, "all deliveries marked read after batch-read", func() bool {
		remaining, err := st.ListRecentUnreadDeliveries(ctx, "U4", now.Add(-30*24*time.Hour), 100)
		return err == nil && len(remaining) == 0
	})
	waitForCondition(t, 4*time.Second, "20 async UpdateMessage calls for batch-read", func() bool {
		return dm.updateCount()-beforeBatchUpdates >= 20
	})

	batchUpdates := dm.updateCount() - beforeBatchUpdates
	if batchUpdates != 20 {
		t.Fatalf("expected exactly 20 UpdateMessage calls for batch-read, got %d", batchUpdates)
	}
}

func TestAuthFailureSendsExactlyOneDM(t *testing.T) {
	st := openTestStore(t)
	fake := newFakeMulticaServer(t)
	fake.authFail = true
	dm := newMockSlackDM()
	mc := multica.NewClient(fake.srv.URL, fake.srv.Client())
	cache := multica.NewMetadataCache(mc, time.Minute)

	ctx := context.Background()
	if err := st.UpsertMulticaConnection(ctx, store.UpsertMulticaParams{
		SlackUserID:          "U5",
		SlackTeamID:          "T1",
		MulticaUserID:        "mu-5",
		MulticaUserName:      "Alice",
		MulticaWorkspaceID:   "ws-1",
		MulticaWorkspaceSlug: "acme",
		MulticaPAT:           "mul_revoked",
		NotifyAfter:          time.Now().UTC(),
	}); err != nil {
		t.Fatalf("upsert user: %v", err)
	}

	mgr := NewManager(ManagerConfig{
		MulticaAPIURL:    fake.srv.URL,
		MulticaAppURL:    "https://multica.ai",
		Version:          "test",
		BatchUpdateDelay: time.Millisecond,
		WSInitialBackoff: 10 * time.Millisecond,
		WSMaxBackoff:     20 * time.Millisecond,
	}, st, mc, cache, dm, testLogger())
	defer mgr.Stop()

	if err := mgr.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	select {
	case msg := <-dm.postCh:
		if !strings.Contains(strings.ToLower(msg.Fallback), "pat") &&
			!strings.Contains(strings.ToLower(msg.Fallback), "token") {
			t.Fatalf("expected reconnect prompt mentioning PAT/token, got %q", msg.Fallback)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for auth failure DM")
	}

	waitForCondition(t, 2*time.Second, "auth_status=auth_failed", func() bool {
		u, err := st.GetUser(ctx, "U5")
		return err == nil && u.AuthStatus == store.AuthFailed
	})

	// Wait a bit to confirm WS does NOT retry and no second DM is sent.
	time.Sleep(100 * time.Millisecond)
	if attempts := fake.wsAttempts.Load(); attempts != 1 {
		t.Fatalf("expected exactly 1 WS attempt (retries halted), got %d", attempts)
	}
	if got := dm.postCount(); got != 1 {
		t.Fatalf("expected exactly 1 reconnect DM, got %d", got)
	}

	// Even if handleAuthFailed were invoked again, it must NOT send a duplicate DM.
	mgr.handleAuthFailed(ctx, "U5", "duplicate trigger")
	if got := dm.postCount(); got != 1 {
		t.Fatalf("expected no duplicate auth-failure DM, got %d", got)
	}
}

func TestSwitchUserWorkspaceAndDisconnect(t *testing.T) {
	st := openTestStore(t)
	fake := newFakeMulticaServer(t)
	dm := newMockSlackDM()
	mc := multica.NewClient(fake.srv.URL, fake.srv.Client())
	cache := multica.NewMetadataCache(mc, time.Minute)

	ctx := context.Background()
	now := time.Now().UTC()
	if err := st.UpsertMulticaConnection(ctx, store.UpsertMulticaParams{
		SlackUserID:          "U6",
		SlackTeamID:          "T1",
		MulticaUserID:        "mu-6",
		MulticaUserName:      "Alice",
		MulticaWorkspaceID:   "ws-old",
		MulticaWorkspaceSlug: "old-ws",
		MulticaPAT:           "mul_valid",
		NotifyAfter:          now.Add(-time.Hour),
	}); err != nil {
		t.Fatalf("upsert user: %v", err)
	}

	mgr := NewManager(ManagerConfig{
		MulticaAPIURL:    fake.srv.URL,
		MulticaAppURL:    "https://multica.ai",
		Version:          "test",
		BatchUpdateDelay: time.Millisecond,
	}, st, mc, cache, dm, testLogger())
	defer mgr.Stop()

	if err := mgr.ConnectUser(ctx, "U6"); err != nil {
		t.Fatalf("ConnectUser: %v", err)
	}
	select {
	case <-fake.connected:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for initial WS connect")
	}

	// Pre-populate unread items in the new workspace before switching.
	fake.setInbox([]multica.InboxItem{
		{ID: "ws2-old-1", WorkspaceID: "ws-new", Type: "mentioned", Title: "Existing WS2 item", CreatedAt: now.Add(-2 * time.Minute)},
	})

	if err := mgr.SwitchUserWorkspace(ctx, "U6", "ws-new", "new-ws"); err != nil {
		t.Fatalf("SwitchUserWorkspace: %v", err)
	}
	select {
	case <-fake.connected:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for WS connect on new workspace")
	}
	time.Sleep(50 * time.Millisecond)

	if dm.postCount() != 0 {
		t.Fatalf("expected 0 DMs after SwitchUserWorkspace snapshot, got %d", dm.postCount())
	}
	u, err := st.GetUser(ctx, "U6")
	if err != nil {
		t.Fatalf("GetUser: %v", err)
	}
	if u.MulticaWorkspaceID != "ws-new" || u.MulticaWorkspaceSlug != "new-ws" {
		t.Fatalf("expected workspace updated to ws-new/new-ws, got %s/%s", u.MulticaWorkspaceID, u.MulticaWorkspaceSlug)
	}

	mgr.DisconnectUser("U6")
	if mgr.IsConnected("U6") || mgr.ConnectedCount() != 0 {
		t.Fatalf("expected user disconnected after DisconnectUser, connected=%v count=%d", mgr.IsConnected("U6"), mgr.ConnectedCount())
	}
}

func TestBatchReadDoesNotBlockWSReaderOrPong(t *testing.T) {
	st := openTestStore(t)
	fake := newFakeMulticaServer(t)
	dm := newMockSlackDM()
	mc := multica.NewClient(fake.srv.URL, fake.srv.Client())
	cache := multica.NewMetadataCache(mc, time.Minute)

	ctx := context.Background()
	now := time.Now().UTC()
	if err := st.UpsertMulticaConnection(ctx, store.UpsertMulticaParams{
		SlackUserID:          "U7",
		SlackTeamID:          "T1",
		MulticaUserID:        "mu-7",
		MulticaUserName:      "Alice",
		MulticaWorkspaceID:   "ws-1",
		MulticaWorkspaceSlug: "acme",
		MulticaPAT:           "mul_valid",
		NotifyAfter:          now,
	}); err != nil {
		t.Fatalf("upsert user: %v", err)
	}

	sampleFallback, sampleBlocks := FormatNotificationBlocks("https://multica.ai", "acme", multica.InboxItem{
		ID:          "sample",
		WorkspaceID: "ws-1",
		Type:        "mentioned",
		Title:       "Slow batch item",
	}, multica.WorkspaceMeta{}, false)
	sampleJSON, err := MarshalBlocks(sampleBlocks)
	if err != nil {
		t.Fatalf("MarshalBlocks: %v", err)
	}

	// Seed 5 recent + 3 older unread deliveries in SQLite.
	for i := 0; i < 5; i++ {
		if ok, err := st.TryClaimDelivery(ctx, store.DeliveredNotification{
			SlackUserID:    "U7",
			InboxItemID:    fmt.Sprintf("slow-recent-%d", i),
			DeliveryStatus: store.DeliveryDelivered,
			SlackChannelID: "D_U7",
			SlackMessageTS: fmt.Sprintf("1700002000.%06d", i),
			CardFallback:   sampleFallback,
			CardBlocksJSON: sampleJSON,
			CreatedAt:      now.Add(-time.Duration(i+1) * time.Minute),
		}); err != nil || !ok {
			t.Fatalf("seed recent %d: ok=%v err=%v", i, ok, err)
		}
	}
	for i := 0; i < 3; i++ {
		if ok, err := st.TryClaimDelivery(ctx, store.DeliveredNotification{
			SlackUserID:    "U7",
			InboxItemID:    fmt.Sprintf("slow-old-%d", i),
			DeliveryStatus: store.DeliveryDelivered,
			SlackChannelID: "D_U7",
			SlackMessageTS: fmt.Sprintf("1690002000.%06d", i),
			CardFallback:   sampleFallback,
			CardBlocksJSON: sampleJSON,
			CreatedAt:      now.Add(-10 * 24 * time.Hour),
		}); err != nil || !ok {
			t.Fatalf("seed old %d: ok=%v err=%v", i, ok, err)
		}
	}

	// Block UpdateMessage until test releases or streamCtx is cancelled.
	updateStarted := make(chan struct{}, 1)
	dm.mu.Lock()
	dm.onUpdate = func(callCtx context.Context, _, _ string) error {
		select {
		case updateStarted <- struct{}{}:
		default:
		}
		<-callCtx.Done()
		return callCtx.Err()
	}
	dm.mu.Unlock()

	mgr := NewManager(ManagerConfig{
		MulticaAPIURL:    fake.srv.URL,
		MulticaAppURL:    "https://multica.ai",
		Version:          "test",
		BatchUpdateDelay: time.Millisecond,
		WSPingInterval:   30 * time.Millisecond,
		WSPongTimeout:    120 * time.Millisecond,
		WSInitialBackoff: 15 * time.Millisecond,
		WSMaxBackoff:     50 * time.Millisecond,
	}, st, mc, cache, dm, testLogger())

	if err := mgr.ConnectUser(ctx, "U7"); err != nil {
		t.Fatalf("ConnectUser: %v", err)
	}
	select {
	case <-fake.connected:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for WS connect")
	}

	// Trigger inbox:batch-read whose Slack UpdateMessage blocks indefinitely.
	fake.sendEvent("inbox:batch-read", map[string]any{"recipient_id": "mu-7", "count": 8})

	select {
	case <-updateStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for async batch-read UpdateMessage to start")
	}

	// All 8 DB rows must already be marked read up front while UpdateMessage is still blocked.
	remaining, err := st.ListRecentUnreadDeliveries(ctx, "U7", now.Add(-30*24*time.Hour), 100)
	if err != nil {
		t.Fatalf("ListRecentUnreadDeliveries: %v", err)
	}
	if len(remaining) != 0 {
		t.Fatalf("expected all 8 DB rows marked read up front, got %d remaining", len(remaining))
	}

	// Wait longer than WSPongTimeout (120ms) while UpdateMessage is blocked:
	// WS must stay healthy (no disconnect/reconnect) and process a new inbox:new event immediately.
	time.Sleep(220 * time.Millisecond)
	if attempts := fake.wsAttempts.Load(); attempts != 1 {
		t.Fatalf("expected WS to stay connected without pong timeout reconnect, got %d attempts", attempts)
	}
	if !mgr.IsConnected("U7") {
		t.Fatal("expected user stream to remain connected during slow batch-read updates")
	}

	fake.sendEvent("inbox:new", map[string]any{
		"item": multica.InboxItem{
			ID:              "live-during-batch",
			WorkspaceID:     "ws-1",
			Type:            "mentioned",
			Title:           "Arrived during slow batch-read",
			IssueID:         "iss-7",
			IssueIdentifier: "ACME-7",
			CreatedAt:       time.Now().UTC(),
		},
	})
	select {
	case msg := <-dm.postCh:
		if !strings.Contains(msg.Fallback, "ACME-7") {
			t.Fatalf("expected live DM for ACME-7, got %q", msg.Fallback)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("subsequent inbox:new was blocked by slow batch-read UpdateMessage")
	}

	// Stop() must cancel streamCtx and exit cleanly without hanging on the blocked UpdateMessage.
	stopped := make(chan struct{})
	go func() {
		mgr.Stop()
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("mgr.Stop() hung waiting for blocked batch-read goroutine")
	}
}

func TestConcurrentConnectUserAndStop(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	fake := newFakeMulticaServer(t)
	dm := newMockSlackDM()

	mc := multica.NewClient(fake.srv.URL, fake.srv.Client())
	cache := multica.NewMetadataCache(mc, time.Minute)

	for i := 0; i < 4; i++ {
		uid := fmt.Sprintf("U_RACE_%d", i)
		if err := st.UpsertMulticaConnection(ctx, store.UpsertMulticaParams{
			SlackUserID:          uid,
			SlackTeamID:          "T1",
			MulticaUserID:        fmt.Sprintf("mu-%d", i),
			MulticaUserName:      "User",
			MulticaWorkspaceID:   "ws-1",
			MulticaWorkspaceSlug: "acme",
			MulticaPAT:           "mul_ok",
			NotifyAfter:          time.Now().UTC(),
		}); err != nil {
			t.Fatalf("upsert %s: %v", uid, err)
		}
	}

	mgr := NewManager(ManagerConfig{
		MulticaAPIURL:    fake.srv.URL,
		MulticaAppURL:    "https://multica.ai",
		Version:          "test",
		BatchUpdateDelay: -1,
		WSInitialBackoff: 10 * time.Millisecond,
		WSMaxBackoff:     50 * time.Millisecond,
	}, st, mc, cache, dm, testLogger())

	if err := mgr.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		uid := fmt.Sprintf("U_RACE_%d", i)
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 5; j++ {
				_ = mgr.ConnectUser(ctx, uid)
			}
		}()
	}

	time.Sleep(15 * time.Millisecond)
	mgr.Stop()
	wg.Wait()

	// After Stop(), ConnectUser must not spawn a new stream or recreate baseCtx.
	if err := mgr.ConnectUser(ctx, "U_RACE_0"); err != nil {
		t.Fatalf("ConnectUser after Stop: %v", err)
	}
	if got := mgr.ConnectedCount(); got != 0 {
		t.Fatalf("expected 0 connected streams after Stop, got %d", got)
	}

	// Start() resets stopped so streams can be started again cleanly.
	if err := mgr.Start(ctx); err != nil {
		t.Fatalf("restart Start: %v", err)
	}
	waitForCondition(t, 2*time.Second, "restarted streams", func() bool {
		return mgr.ConnectedCount() > 0
	})
	mgr.Stop()
}

func TestConnectAndResumeCaptureNotifyAfterBeforeSnapshot(t *testing.T) {
	for _, tc := range []struct {
		name string
		call func(mgr *Manager, ctx context.Context, uid string) error
	}{
		{
			name: "ConnectUser",
			call: func(mgr *Manager, ctx context.Context, uid string) error {
				return mgr.ConnectUser(ctx, uid)
			},
		},
		{
			name: "ResumeUser",
			call: func(mgr *Manager, ctx context.Context, uid string) error {
				return mgr.ResumeUser(ctx, uid)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			st := openTestStore(t)
			fake := newFakeMulticaServer(t)
			dm := newMockSlackDM()

			mc := multica.NewClient(fake.srv.URL, fake.srv.Client())
			cache := multica.NewMetadataCache(mc, time.Minute)

			var clockMu sync.Mutex
			now := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)

			if err := st.UpsertMulticaConnection(ctx, store.UpsertMulticaParams{
				SlackUserID:          "U_WIN",
				SlackTeamID:          "T1",
				MulticaUserID:        "mu-win",
				MulticaUserName:      "Alice",
				MulticaWorkspaceID:   "ws-1",
				MulticaWorkspaceSlug: "acme",
				MulticaPAT:           "mul_ok",
				NotifyAfter:          now.Add(-time.Hour),
			}); err != nil {
				t.Fatalf("upsert user: %v", err)
			}

			// First /api/inbox call (snapshotUnread) returns pre-existing item only,
			// then an item is created at t+500ms and clock advances to t+1s before snapshot returns.
			fake.setInbox([]multica.InboxItem{
				{
					ID:              "pre-existing",
					WorkspaceID:     "ws-1",
					Type:            "mentioned",
					Title:           "Old unread",
					IssueID:         "iss-old",
					IssueIdentifier: "ACME-10",
					CreatedAt:       now.Add(-5 * time.Minute),
				},
			})
			fake.mu.Lock()
			fake.onListInbox = func(callNum int) {
				if callNum == 1 {
					clockMu.Lock()
					createdDuringSnapshot := now.Add(500 * time.Millisecond)
					now = now.Add(time.Second)
					clockMu.Unlock()

					fake.mu.Lock()
					fake.inboxItems = append(fake.inboxItems, multica.InboxItem{
						ID:              "during-snapshot",
						WorkspaceID:     "ws-1",
						Type:            "mentioned",
						Title:           "Arrived during snapshot round-trip",
						IssueID:         "iss-mid",
						IssueIdentifier: "ACME-11",
						CreatedAt:       createdDuringSnapshot,
					})
					fake.mu.Unlock()
				}
			}
			fake.mu.Unlock()

			mgr := NewManager(ManagerConfig{
				MulticaAPIURL:    fake.srv.URL,
				MulticaAppURL:    "https://multica.ai",
				Version:          "test",
				BatchUpdateDelay: -1,
				WSInitialBackoff: 10 * time.Millisecond,
				WSMaxBackoff:     50 * time.Millisecond,
			}, st, mc, cache, dm, testLogger())
			mgr.now = func() time.Time {
				clockMu.Lock()
				defer clockMu.Unlock()
				return now
			}
			t.Cleanup(mgr.Stop)

			if err := tc.call(mgr, ctx, "U_WIN"); err != nil {
				t.Fatalf("%s failed: %v", tc.name, err)
			}

			select {
			case msg := <-dm.postCh:
				if !strings.Contains(msg.Fallback, "ACME-11") {
					t.Fatalf("expected catch-up DM for ACME-11, got %q", msg.Fallback)
				}
			case <-time.After(3 * time.Second):
				t.Fatalf("timed out waiting for mid-snapshot item ACME-11 to be caught up")
			}

			if len(dm.getPosts()) != 1 {
				t.Fatalf("expected exactly 1 DM (pre-existing seeded, mid-snapshot delivered), got %d", len(dm.getPosts()))
			}
		})
	}
}

func TestFormatterRuneSafetyAndURLEscaping(t *testing.T) {
	if got := humanizeToken("über_dringend"); got != "Über Dringend" {
		t.Fatalf("humanizeToken multibyte: got %q, want %q", got, "Über Dringend")
	}
	if got := escapeSlackURL("https://multica.ai/ws/issues/ACME-1?a=1&b=2>x|y"); got != "https://multica.ai/ws/issues/ACME-1?a=1&amp;b=2%3Ex%7Cy" {
		t.Fatalf("escapeSlackURL: got %q", got)
	}
}
