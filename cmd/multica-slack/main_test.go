package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/everlightlabs/multica-slack/internal/config"
	"github.com/everlightlabs/multica-slack/internal/crypto"
	"github.com/everlightlabs/multica-slack/internal/multica"
	"github.com/everlightlabs/multica-slack/internal/store"
)

const (
	testSigningSecret = "smoke-signing-secret"
	testBotToken      = "xoxb-smoke-bot-token"
	testClientID      = "smoke-client-id"
	testClientSecret  = "smoke-client-secret"
	testTeamID        = "T_SMOKE"
	testUserID        = "U_SMOKE"
	testPAT           = "mul_smoke_pat_12345"
)

type recordedSlackCall struct {
	Path  string
	Token string
	Form  url.Values
	Body  string
}

func TestEndToEndSmokeAndGracefulShutdown(t *testing.T) {
	openDMCh := make(chan recordedSlackCall, 4)
	postMsgCh := make(chan recordedSlackCall, 4)
	unfurlCh := make(chan recordedSlackCall, 4)

	mockSlack := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(raw))
		_ = r.ParseForm()

		tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if tok == "" {
			tok = r.FormValue("token")
		}
		call := recordedSlackCall{
			Path:  r.URL.Path,
			Token: tok,
			Form:  r.Form,
			Body:  string(raw),
		}

		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/conversations.open":
			openDMCh <- call
			_, _ = w.Write([]byte(`{"ok":true,"channel":{"id":"D_SMOKE"}}`))
		case "/chat.postMessage":
			postMsgCh <- call
			_, _ = w.Write([]byte(`{"ok":true,"channel":"D_SMOKE","ts":"1700000001.000100"}`))
		case "/chat.unfurl":
			unfurlCh <- call
			_, _ = w.Write([]byte(`{"ok":true}`))
		default:
			_, _ = w.Write([]byte(`{"ok":true}`))
		}
	}))
	defer mockSlack.Close()

	var (
		wsMu     sync.Mutex
		wsConn   *websocket.Conn
		wsReady  = make(chan struct{}, 1)
	)

	mockMultica := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/ws" {
			conn, err := websocket.Accept(w, r, nil)
			if err != nil {
				return
			}
			defer func() { _ = conn.CloseNow() }()

			var authFrame struct {
				Type    string `json:"type"`
				Payload struct {
					Token string `json:"token"`
				} `json:"payload"`
			}
			if err := wsjson.Read(r.Context(), conn, &authFrame); err != nil {
				return
			}
			if authFrame.Type != "auth" || authFrame.Payload.Token != testPAT {
				_ = wsjson.Write(r.Context(), conn, map[string]any{"type": "auth_error", "error": "unauthorized"})
				return
			}
			if err := wsjson.Write(r.Context(), conn, map[string]any{"type": "auth_ack"}); err != nil {
				return
			}

			wsMu.Lock()
			wsConn = conn
			wsMu.Unlock()
			select {
			case wsReady <- struct{}{}:
			default:
			}

			for {
				var frame map[string]any
				if err := wsjson.Read(r.Context(), conn, &frame); err != nil {
					return
				}
				if frame["type"] == "ping" {
					_ = wsjson.Write(r.Context(), conn, map[string]any{"type": "pong"})
				}
			}
		}

		if r.Header.Get("Authorization") != "Bearer "+testPAT {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"unauthorized"}`))
			return
		}

		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/workspaces":
			_ = json.NewEncoder(w).Encode([]multica.Workspace{
				{ID: "ws-acme", Name: "Acme Corp", Slug: "acme"},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/api/inbox":
			_, _ = w.Write([]byte(`[]`))
		case r.Method == http.MethodGet && r.URL.Path == "/api/workspaces/ws-acme/members":
			_ = json.NewEncoder(w).Encode([]multica.Member{
				{ID: "mem-1", UserID: "mc-actor-1", Name: "Bob"},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/api/agents":
			_, _ = w.Write([]byte(`[]`))
		case r.Method == http.MethodGet && r.URL.Path == "/api/squads":
			_, _ = w.Write([]byte(`[]`))
		case r.Method == http.MethodGet && r.URL.Path == "/api/projects":
			_ = json.NewEncoder(w).Encode([]multica.Project{
				{ID: "proj-1", Name: "Core"},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/api/issue-statuses":
			_ = json.NewEncoder(w).Encode([]multica.IssueStatus{
				{ID: "st-1", Key: "in_progress", Name: "In Progress", Category: "started"},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/api/issues/ACME-42":
			_ = json.NewEncoder(w).Encode(multica.Issue{
				ID:             "issue-uuid-3581",
				Identifier:     "ACME-42",
				Title:          "Wire main entrypoint and CI",
				Description:    "End-to-end Slack unfurling and DM notifications.",
				Status:         "in_progress",
				StatusCategory: "started",
				StatusName:     "In Progress",
				Priority:       "high",
				ProjectID:      "proj-1",
				UpdatedAt:      time.Now().UTC().Add(-2 * time.Minute),
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer mockMultica.Close()

	dataDir := t.TempDir()
	encKey := make([]byte, 32)
	for i := range encKey {
		encKey[i] = byte(i + 10)
	}

	cfg := config.Config{
		Port:                        0,
		PublicBaseURL:               "https://multica-slack.example.com",
		DataDir:                     dataDir,
		EncryptionKey:               encKey,
		SlackBotToken:               testBotToken,
		SlackSigningSecret:          testSigningSecret,
		SlackClientID:               testClientID,
		SlackClientSecret:           testClientSecret,
		SlackAllowedTeamID:          testTeamID,
		MulticaAPIURL:               mockMultica.URL,
		MulticaAppURL:               "https://multica.ai",
		MulticaDefaultWorkspaceSlug: "acme",
		Version:                     "test-v1",
	}

	type readyInfo struct {
		addr string
		app  *app
	}
	readyCh := make(chan readyInfo, 1)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	runErrCh := make(chan error, 1)
	go func() {
		runErrCh <- runWithOptions(ctx, cfg, logger, runOptions{
			slackAPIURL: mockSlack.URL + "/",
			onReady: func(addr string, a *app) {
				readyCh <- readyInfo{addr: addr, app: a}
			},
		})
	}()

	var ready readyInfo
	select {
	case ready = <-readyCh:
	case err := <-runErrCh:
		t.Fatalf("runWithOptions exited prematurely: %v", err)
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for server ready")
	}

	baseURL := "http://" + ready.addr
	testHTTPClient := &http.Client{
		Timeout:   5 * time.Second,
		Transport: &http.Transport{DisableKeepAlives: true},
	}
	defer testHTTPClient.CloseIdleConnections()

	// 1. Assert /healthz and /readyz return 200.
	resp, err := testHTTPClient.Get(baseURL + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz failed: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	if err := resp.Body.Close(); err != nil {
		t.Fatalf("close /healthz body: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /healthz status = %d, want 200", resp.StatusCode)
	}

	resp, err = testHTTPClient.Get(baseURL + "/readyz")
	if err != nil {
		t.Fatalf("GET /readyz failed: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	if err := resp.Body.Close(); err != nil {
		t.Fatalf("close /readyz body: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /readyz status = %d, want 200", resp.StatusCode)
	}

	// 2. Link a user via Store -> trigger ConnectUser -> push inbox:new -> assert Slack DM arrives.
	err = ready.app.store.UpsertMulticaConnection(ctx, store.UpsertMulticaParams{
		SlackUserID:          testUserID,
		SlackTeamID:          testTeamID,
		MulticaUserID:        "mc-user-1",
		MulticaUserName:      "Alice Smith",
		MulticaWorkspaceID:   "ws-acme",
		MulticaWorkspaceSlug: "acme",
		MulticaPAT:           testPAT,
		NotifyAfter:          time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("UpsertMulticaConnection failed: %v", err)
	}

	if err := ready.app.notifyMgr.ConnectUser(ctx, testUserID); err != nil {
		t.Fatalf("ConnectUser failed: %v", err)
	}

	select {
	case <-wsReady:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for Multica WS auth_ack")
	}

	inboxItem := multica.InboxItem{
		ID:              "inbox-smoke-1",
		WorkspaceID:     "ws-acme",
		Type:            "mentioned",
		Title:           "mentioned you on ACME-42",
		Body:            "Please review the final PR",
		CreatedAt:       time.Now().UTC(),
		IssueID:         "issue-uuid-3581",
		IssueIdentifier: "ACME-42",
		IssueStatus:     "in_progress",
		IssuePriority:   "high",
		IssueProjectID:  "proj-1",
		ActorType:       "member",
		ActorID:         "mc-actor-1",
	}
	itemRaw, err := json.Marshal(map[string]any{"item": inboxItem})
	if err != nil {
		t.Fatalf("marshal inbox:new payload: %v", err)
	}

	wsMu.Lock()
	activeWS := wsConn
	wsMu.Unlock()
	if err := wsjson.Write(ctx, activeWS, multica.WSEnvelope{
		Type:    "inbox:new",
		Payload: itemRaw,
	}); err != nil {
		t.Fatalf("write inbox:new WS frame: %v", err)
	}

	select {
	case call := <-openDMCh:
		if call.Token != testBotToken {
			t.Fatalf("conversations.open token = %q, want bot token", call.Token)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for conversations.open")
	}

	select {
	case call := <-postMsgCh:
		if call.Form.Get("channel") != "D_SMOKE" {
			t.Fatalf("chat.postMessage channel = %q, want D_SMOKE", call.Form.Get("channel"))
		}
		if !strings.Contains(call.Form.Get("text"), "ACME-42") && !strings.Contains(call.Form.Get("blocks"), "ACME-42") {
			t.Fatalf("chat.postMessage missing ACME-42 reference: text=%q blocks=%q", call.Form.Get("text"), call.Form.Get("blocks"))
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for chat.postMessage DM")
	}

	// 3. Send signed link_shared POST -> assert chat.unfurl arrives.
	linkSharedBody, err := json.Marshal(map[string]any{
		"type":    "event_callback",
		"team_id": testTeamID,
		"event": map[string]any{
			"type":       "link_shared",
			"user":       testUserID,
			"channel":    "C_SMOKE",
			"message_ts": "1700000010.000100",
			"unfurl_id":  "C_SMOKE-1700000010.000100-abc",
			"source":     "composer",
			"links": []map[string]string{
				{"domain": "multica.ai", "url": "https://multica.ai/acme/issues/ACME-42"},
			},
		},
	})
	if err != nil {
		t.Fatalf("marshal link_shared body: %v", err)
	}

	req := signedSlackRequest(t, baseURL+"/slack/events", linkSharedBody)
	evResp, err := testHTTPClient.Do(req)
	if err != nil {
		t.Fatalf("POST /slack/events failed: %v", err)
	}
	_, _ = io.Copy(io.Discard, evResp.Body)
	if err := evResp.Body.Close(); err != nil {
		t.Fatalf("close /slack/events body: %v", err)
	}
	if evResp.StatusCode != http.StatusOK {
		t.Fatalf("POST /slack/events status = %d, want 200", evResp.StatusCode)
	}

	select {
	case call := <-unfurlCh:
		if !strings.Contains(call.Form.Get("unfurls"), "ACME-42") {
			t.Fatalf("chat.unfurl missing ACME-42: %q", call.Form.Get("unfurls"))
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for chat.unfurl")
	}

	// 4. Cancel ctx -> assert run exits nil within 2s and WAL is checkpointed.
	cancel()
	select {
	case err := <-runErrCh:
		if err != nil {
			t.Fatalf("runWithOptions returned error on shutdown: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("runWithOptions did not shut down within 2s")
	}

	walPath := filepath.Join(dataDir, "multica-slack.db-wal")
	if info, err := os.Stat(walPath); err == nil && info.Size() != 0 {
		t.Fatalf("expected checkpointed WAL (absent or 0 bytes), got %d bytes", info.Size())
	}
}

func TestRunRejectsInvalidConfigFast(t *testing.T) {
	err := run(context.Background(), config.Config{
		Port:          0,
		DataDir:       t.TempDir(),
		EncryptionKey: []byte("short"),
	}, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	if err == nil {
		t.Fatal("expected error for invalid encryption key, got nil")
	}
}

func TestPruneLoopPrunesOldDeliveries(t *testing.T) {
	encKey := make([]byte, 32)
	for i := range encKey {
		encKey[i] = byte(i + 1)
	}
	box, err := crypto.NewBox(encKey)
	if err != nil {
		t.Fatalf("NewBox: %v", err)
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"), box)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer func() { _ = st.Close() }()

	now := time.Now().UTC()
	if _, err := st.TryClaimDelivery(context.Background(), store.DeliveredNotification{
		SlackUserID:    "U1",
		InboxItemID:    "old-item",
		DeliveryStatus: store.DeliveryDelivered,
		CreatedAt:      now.Add(-31 * 24 * time.Hour),
	}); err != nil {
		t.Fatalf("claim old delivery: %v", err)
	}
	if _, err := st.TryClaimDelivery(context.Background(), store.DeliveredNotification{
		SlackUserID:    "U1",
		InboxItemID:    "fresh-item",
		DeliveryStatus: store.DeliveryDelivered,
		CreatedAt:      now,
	}); err != nil {
		t.Fatalf("claim fresh delivery: %v", err)
	}
	if err := st.CreateOAuthState(context.Background(), "expired-oauth-state", "U1", now.Add(-time.Minute)); err != nil {
		t.Fatalf("create expired oauth state: %v", err)
	}
	if err := st.CreateOAuthState(context.Background(), "fresh-oauth-state", "U1", now.Add(10*time.Minute)); err != nil {
		t.Fatalf("create fresh oauth state: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		runPruneLoop(ctx, st, 10*time.Millisecond, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	}()

	deadline := time.Now().Add(1 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := st.GetUnreadDelivery(context.Background(), "U1", "old-item"); err == store.ErrNotFound {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	cancel()
	<-done

	if _, err := st.GetUnreadDelivery(context.Background(), "U1", "old-item"); err != store.ErrNotFound {
		t.Fatalf("expected old-item to be pruned (ErrNotFound), got %v", err)
	}
	if _, err := st.GetUnreadDelivery(context.Background(), "U1", "fresh-item"); err != nil {
		t.Fatalf("expected fresh-item to remain, got %v", err)
	}
	if _, err := st.ConsumeOAuthState(context.Background(), "expired-oauth-state", now.Add(-2*time.Minute)); err != store.ErrNotFound {
		t.Fatalf("expected expired-oauth-state to be pruned, got %v", err)
	}
	if uid, err := st.ConsumeOAuthState(context.Background(), "fresh-oauth-state", now); err != nil || uid != "U1" {
		t.Fatalf("expected fresh-oauth-state to remain for U1, got uid=%q err=%v", uid, err)
	}
}

func signedSlackRequest(t *testing.T, targetURL string, body []byte) *http.Request {
	t.Helper()
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	base := fmt.Sprintf("v0:%s:%s", ts, string(body))
	mac := hmac.New(sha256.New, []byte(testSigningSecret))
	mac.Write([]byte(base))
	sig := "v0=" + hex.EncodeToString(mac.Sum(nil))

	req, err := http.NewRequest(http.MethodPost, targetURL, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("build slack request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Slack-Request-Timestamp", ts)
	req.Header.Set("X-Slack-Signature", sig)
	return req
}
