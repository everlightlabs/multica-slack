package slackhttp_test

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
	"github.com/everlightlabs/multica-slack/internal/notify"
	"github.com/everlightlabs/multica-slack/internal/prefs"
	"github.com/everlightlabs/multica-slack/internal/slackhttp"
	"github.com/everlightlabs/multica-slack/internal/store"
	"github.com/everlightlabs/multica-slack/internal/unfurl"
	"github.com/slack-go/slack"
)

const (
	testSigningSecret = "test-signing-secret-123456"
	testBotToken      = "xoxb-test-bot-token"
	testClientID      = "client-id-123"
	testClientSecret  = "client-secret-456"
	testTeamID        = "T_EVERLIGHT"
	testUserID        = "U_ALICE"
	testPAT           = "mul_valid_test_pat_12345"
	testUserOAuthTok  = "xoxp-test-user-oauth-token"
	testPublicBaseURL = "https://multica-slack.example.com"
)

type slackAPICall struct {
	Path   string
	Token  string
	Header http.Header
	Form   url.Values
	Body   string
}

type mockSlackServer struct {
	srv *httptest.Server

	mu              sync.Mutex
	calls           []slackAPICall
	oauthTeamID     string
	oauthUserID     string
	oauthUserToken  string
	publishHomeCh   chan slackAPICall
	unfurlCh        chan slackAPICall
	chatUpdateCh    chan slackAPICall
	chatPostCh      chan slackAPICall
	viewsOpenCh     chan slackAPICall
}

func newMockSlackServer(t *testing.T) *mockSlackServer {
	t.Helper()
	m := &mockSlackServer{
		oauthTeamID:    testTeamID,
		oauthUserID:    testUserID,
		oauthUserToken: testUserOAuthTok,
		publishHomeCh:  make(chan slackAPICall, 16),
		unfurlCh:       make(chan slackAPICall, 16),
		chatUpdateCh:   make(chan slackAPICall, 16),
		chatPostCh:     make(chan slackAPICall, 16),
		viewsOpenCh:    make(chan slackAPICall, 16),
	}

	m.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(raw))
		_ = r.ParseForm()

		tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if tok == "" {
			tok = r.FormValue("token")
		}
		call := slackAPICall{
			Path:   r.URL.Path,
			Token:  tok,
			Header: r.Header.Clone(),
			Form:   r.Form,
			Body:   string(raw),
		}

		m.mu.Lock()
		m.calls = append(m.calls, call)
		oauthTeamID := m.oauthTeamID
		oauthUserID := m.oauthUserID
		oauthUserToken := m.oauthUserToken
		m.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/views.publish":
			select {
			case m.publishHomeCh <- call:
			default:
			}
			_, _ = w.Write([]byte(`{"ok":true,"view":{"id":"V_HOME"}}`))
		case "/views.open":
			select {
			case m.viewsOpenCh <- call:
			default:
			}
			_, _ = w.Write([]byte(`{"ok":true,"view":{"id":"V_MODAL"}}`))
		case "/chat.unfurl":
			select {
			case m.unfurlCh <- call:
			default:
			}
			_, _ = w.Write([]byte(`{"ok":true}`))
		case "/chat.update":
			select {
			case m.chatUpdateCh <- call:
			default:
			}
			_, _ = w.Write([]byte(`{"ok":true,"channel":"D123","ts":"1700000000.000100"}`))
		case "/chat.postMessage":
			select {
			case m.chatPostCh <- call:
			default:
			}
			_, _ = w.Write([]byte(`{"ok":true,"channel":"D123","ts":"1700000001.000200"}`))
		case "/conversations.open":
			_, _ = w.Write([]byte(`{"ok":true,"channel":{"id":"D123"}}`))
		case "/oauth.v2.access":
			resp := map[string]any{
				"ok":         true,
				"app_id":     "A123",
				"token_type": "bot",
				"team": map[string]any{
					"id":   oauthTeamID,
					"name": "Everlight",
				},
				"authed_user": map[string]any{
					"id":           oauthUserID,
					"scope":        "links:read,links:write",
					"access_token": oauthUserToken,
					"token_type":   "user",
				},
			}
			_ = json.NewEncoder(w).Encode(resp)
		default:
			_, _ = w.Write([]byte(`{"ok":true}`))
		}
	}))
	t.Cleanup(m.srv.Close)
	return m
}

func (m *mockSlackServer) apiURL() string {
	return m.srv.URL + "/"
}

type mockMulticaServer struct {
	srv *httptest.Server

	mu              sync.Mutex
	readCalls       []string
	commentCalls    []struct{ IssueID, WorkspaceID, Content, Auth string }
	workspaces      []multica.Workspace
	validPAT        string
	issueByID       map[string]multica.Issue
}

func newMockMulticaServer(t *testing.T) *mockMulticaServer {
	t.Helper()
	m := &mockMulticaServer{
		validPAT: testPAT,
		workspaces: []multica.Workspace{
			{ID: "ws-other", Name: "Other Workspace", Slug: "other-ws"},
			{ID: "ws-acme", Name: "Acme Corp", Slug: "acme"},
		},
		issueByID: map[string]multica.Issue{
			"ACME-42": {
				ID:             "issue-uuid-3581",
				Identifier:     "ACME-42",
				Title:          "Add Slack unfurling",
				Description:    "Implement Slack link unfurls for issues.",
				Status:         "in_progress",
				StatusCategory: "started",
				StatusName:     "In Progress",
				Priority:       "high",
				UpdatedAt:      time.Now().UTC().Add(-5 * time.Minute),
			},
		},
	}

	m.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		validPAT := m.validPAT
		workspaces := append([]multica.Workspace(nil), m.workspaces...)
		m.mu.Unlock()

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
			if authFrame.Type != "auth" || authFrame.Payload.Token != validPAT {
				_ = wsjson.Write(r.Context(), conn, map[string]any{"type": "auth_error", "error": "invalid token"})
				return
			}
			if err := wsjson.Write(r.Context(), conn, map[string]any{"type": "auth_ack"}); err != nil {
				return
			}
			for {
				if _, _, err := conn.Read(r.Context()); err != nil {
					return
				}
			}
		}

		auth := r.Header.Get("Authorization")
		if auth != "Bearer "+validPAT {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"invalid token"}`))
			return
		}

		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/me":
			_, _ = w.Write([]byte(`{"id":"mc-user-1","name":"Alice Smith","email":"alice@example.com"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/api/workspaces":
			_ = json.NewEncoder(w).Encode(workspaces)
		case r.Method == http.MethodGet && r.URL.Path == "/api/inbox":
			_, _ = w.Write([]byte(`[]`))
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/issues/"):
			id := strings.TrimPrefix(r.URL.Path, "/api/issues/")
			m.mu.Lock()
			iss, ok := m.issueByID[id]
			m.mu.Unlock()
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"error":"not found"}`))
				return
			}
			_ = json.NewEncoder(w).Encode(iss)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/members"):
			_, _ = w.Write([]byte(`[]`))
		case r.Method == http.MethodGet && (r.URL.Path == "/api/agents" || r.URL.Path == "/api/squads" || r.URL.Path == "/api/projects" || r.URL.Path == "/api/issue-statuses"):
			_, _ = w.Write([]byte(`[]`))
		case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/api/inbox/") && strings.HasSuffix(r.URL.Path, "/read"):
			itemID := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/inbox/"), "/read")
			m.mu.Lock()
			m.readCalls = append(m.readCalls, itemID+"|"+r.Header.Get("X-Workspace-ID"))
			m.mu.Unlock()
			_, _ = w.Write([]byte(`{"ok":true}`))
		case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/api/issues/") && strings.HasSuffix(r.URL.Path, "/comments"):
			issueID := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/issues/"), "/comments")
			var body struct {
				Content string `json:"content"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			m.mu.Lock()
			m.commentCalls = append(m.commentCalls, struct{ IssueID, WorkspaceID, Content, Auth string }{
				IssueID:     issueID,
				WorkspaceID: r.Header.Get("X-Workspace-ID"),
				Content:     body.Content,
				Auth:        auth,
			})
			m.mu.Unlock()
			_, _ = w.Write([]byte(`{"id":"comment-1"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(m.srv.Close)
	return m
}

type noopDMClient struct{}

func (noopDMClient) OpenDM(context.Context, string) (string, error) { return "D123", nil }
func (noopDMClient) PostMessage(context.Context, string, string, []slack.Block) (string, error) {
	return "1700000000.000001", nil
}
func (noopDMClient) UpdateMessage(context.Context, string, string, string, []slack.Block) error {
	return nil
}

type testHarness struct {
	srv     *slackhttp.Server
	store   *store.Store
	mc      *multica.Client
	cache   *multica.MetadataCache
	notify  *notify.Manager
	slack   *mockSlackServer
	multica *mockMulticaServer
	cfg     config.Config
	now     time.Time
}

func newTestHarness(t *testing.T, allowedTeamID string) *testHarness {
	t.Helper()

	key := bytes.Repeat([]byte{0x42}, 32)
	box, err := crypto.NewBox(key)
	if err != nil {
		t.Fatalf("new crypto box: %v", err)
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"), box)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	mockSlack := newMockSlackServer(t)
	mockMC := newMockMulticaServer(t)

	cfg := config.Config{
		Port:                        3020,
		PublicBaseURL:               testPublicBaseURL,
		EncryptionKey:               key,
		SlackBotToken:               testBotToken,
		SlackSigningSecret:          testSigningSecret,
		SlackClientID:               testClientID,
		SlackClientSecret:           testClientSecret,
		SlackAllowedTeamID:          allowedTeamID,
		MulticaAPIURL:               mockMC.srv.URL,
		MulticaAppURL:               "https://multica.ai",
		MulticaDefaultWorkspaceSlug: "acme",
		Version:                     "test",
	}

	mcClient := multica.NewClient(mockMC.srv.URL, nil)
	cache := multica.NewMetadataCache(mcClient, time.Minute)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	slackClient := slack.New(cfg.SlackBotToken, slack.OptionAPIURL(mockSlack.apiURL()))
	unfurlSvc := unfurl.NewService(
		unfurl.ServiceConfig{
			AppBaseURL: cfg.MulticaAppURL,
			BotToken:   cfg.SlackBotToken,
		},
		st,
		mcClient,
		cache,
		unfurl.NewSlackClient(slack.OptionAPIURL(mockSlack.apiURL())),
		logger,
	)

	notifyMgr := notify.NewManager(
		notify.ManagerConfig{
			MulticaAPIURL:    cfg.MulticaAPIURL,
			MulticaAppURL:    cfg.MulticaAppURL,
			Version:          cfg.Version,
			BatchUpdateDelay: -1,
			WSInitialBackoff: 5 * time.Second,
			WSMaxBackoff:     5 * time.Second,
		},
		st,
		mcClient,
		cache,
		noopDMClient{},
		logger,
	)
	t.Cleanup(notifyMgr.Stop)

	fixedNow := time.Now().UTC()
	h := &testHarness{
		store:   st,
		mc:      mcClient,
		cache:   cache,
		notify:  notifyMgr,
		slack:   mockSlack,
		multica: mockMC,
		cfg:     cfg,
		now:     fixedNow,
	}

	h.srv = slackhttp.NewServer(slackhttp.Deps{
		Config:      cfg,
		Store:       st,
		Multica:     mcClient,
		Cache:       cache,
		Unfurl:      unfurlSvc,
		Notify:      notifyMgr,
		Slack:       slackClient,
		SlackAPIURL: mockSlack.apiURL(),
		Logger:      logger,
		Now:         func() time.Time { return h.now },
	})
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = h.srv.Shutdown(ctx)
	})

	return h
}

func signSlackRequest(t *testing.T, req *http.Request, body []byte, secret string, ts time.Time) {
	t.Helper()
	tsStr := strconv.FormatInt(ts.Unix(), 10)
	base := fmt.Sprintf("v0:%s:%s", tsStr, string(body))
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(base))
	sig := "v0=" + hex.EncodeToString(mac.Sum(nil))
	req.Header.Set("X-Slack-Request-Timestamp", tsStr)
	req.Header.Set("X-Slack-Signature", sig)
}

func doSignedPost(t *testing.T, handler http.Handler, path, contentType string, body []byte, ts time.Time, secret string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	req.Header.Set("Content-Type", contentType)
	signSlackRequest(t, req, body, secret, ts)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func TestSlackSignatureAndTeamGuard(t *testing.T) {
	h := newTestHarness(t, testTeamID)
	eventBody := []byte(`{"type":"event_callback","team_id":"` + testTeamID + `","event":{"type":"app_home_opened","user":"` + testUserID + `","tab":"home"}}`)

	// Missing signature headers -> 400
	req := httptest.NewRequest(http.MethodPost, "/slack/events", bytes.NewReader(eventBody))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("missing signature: got %d, want 400", rec.Code)
	}

	// Bad HMAC signature -> 401
	rec = doSignedPost(t, h.srv, "/slack/events", "application/json", eventBody, time.Now(), "wrong-secret")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("bad signature: got %d, want 401", rec.Code)
	}

	// Timestamp > 5m old -> 401
	rec = doSignedPost(t, h.srv, "/slack/events", "application/json", eventBody, time.Now().Add(-6*time.Minute), testSigningSecret)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expired timestamp: got %d, want 401", rec.Code)
	}

	// Wrong team_id on Events -> 403
	wrongTeamEvent := []byte(`{"type":"event_callback","team_id":"T_INTRUDER","event":{"type":"app_home_opened","user":"U999","tab":"home"}}`)
	rec = doSignedPost(t, h.srv, "/slack/events", "application/json", wrongTeamEvent, time.Now(), testSigningSecret)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("wrong team on events: got %d, want 403", rec.Code)
	}

	// Wrong team_id on Slash Commands -> 403
	cmdForm := url.Values{
		"team_id":    {"T_INTRUDER"},
		"user_id":    {testUserID},
		"command":    {"/multica"},
		"text":       {"status"},
		"trigger_id": {"trig-1"},
	}.Encode()
	rec = doSignedPost(t, h.srv, "/slack/commands", "application/x-www-form-urlencoded", []byte(cmdForm), time.Now(), testSigningSecret)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("wrong team on commands: got %d, want 403", rec.Code)
	}

	// Wrong team_id on Interactivity -> 403
	interPayload, _ := json.Marshal(map[string]any{
		"type":       "block_actions",
		"team":       map[string]any{"id": "T_INTRUDER"},
		"user":       map[string]any{"id": testUserID},
		"trigger_id": "trig-2",
		"actions":    []map[string]any{{"action_id": slackhttp.ActionOpenPATModal}},
	})
	interForm := url.Values{"payload": {string(interPayload)}}.Encode()
	rec = doSignedPost(t, h.srv, "/slack/interactions", "application/x-www-form-urlencoded", []byte(interForm), time.Now(), testSigningSecret)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("wrong team on interactions: got %d, want 403", rec.Code)
	}

	// Wrong team_id on OAuth callback -> 403
	if err := h.store.CreateOAuthState(context.Background(), "state-wrong-team", testUserID, h.now.Add(10*time.Minute)); err != nil {
		t.Fatalf("create oauth state: %v", err)
	}
	h.slack.mu.Lock()
	h.slack.oauthTeamID = "T_INTRUDER"
	h.slack.mu.Unlock()
	oauthReq := httptest.NewRequest(http.MethodGet, "/slack/oauth/callback?state=state-wrong-team&code=code-123", nil)
	rec = httptest.NewRecorder()
	h.srv.ServeHTTP(rec, oauthReq)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("wrong team on oauth callback: got %d, want 403", rec.Code)
	}
}

func TestEventsURLVerificationAndAsyncLinkShared(t *testing.T) {
	h := newTestHarness(t, testTeamID)

	// 1. url_verification
	verifyBody := []byte(`{"type":"url_verification","challenge":"challenge-xyz-999"}`)
	rec := doSignedPost(t, h.srv, "/slack/events", "application/json", verifyBody, time.Now(), testSigningSecret)
	if rec.Code != http.StatusOK {
		t.Fatalf("url_verification status: got %d, want 200", rec.Code)
	}
	var challengeResp struct {
		Challenge string `json:"challenge"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &challengeResp); err != nil {
		t.Fatalf("unmarshal challenge: %v", err)
	}
	if challengeResp.Challenge != "challenge-xyz-999" {
		t.Fatalf("challenge mismatch: got %q", challengeResp.Challenge)
	}

	// Seed linked user so unfurl fetches issue with poster's PAT
	if err := h.store.UpsertMulticaConnection(context.Background(), store.UpsertMulticaParams{
		SlackUserID:          testUserID,
		SlackTeamID:          testTeamID,
		MulticaUserID:        "mc-user-1",
		MulticaUserName:      "Alice Smith",
		MulticaWorkspaceID:   "ws-acme",
		MulticaWorkspaceSlug: "acme",
		MulticaPAT:           testPAT,
		NotifyAfter:          h.now,
	}); err != nil {
		t.Fatalf("seed user: %v", err)
	}

	// 2. link_shared returns 200 immediately and invokes chat.unfurl asynchronously
	linkBody := []byte(`{
		"type":"event_callback",
		"team_id":"` + testTeamID + `",
		"authorizations":[{"team_id":"` + testTeamID + `","user_id":"U_AUTH_USER"}],
		"event":{
			"type":"link_shared",
			"channel":"C_GENERAL",
			"user":"` + testUserID + `",
			"message_ts":"1700000000.000100",
			"unfurl_id":"unfurl-abc-123",
			"source":"conversations_history",
			"links":[{"domain":"multica.ai","url":"https://multica.ai/acme/issues/ACME-42"}]
		}
	}`)
	rec = doSignedPost(t, h.srv, "/slack/events", "application/json", linkBody, time.Now(), testSigningSecret)
	if rec.Code != http.StatusOK {
		t.Fatalf("link_shared status: got %d, want 200", rec.Code)
	}

	select {
	case call := <-h.slack.unfurlCh:
		if call.Form.Get("unfurl_id") != "unfurl-abc-123" {
			t.Fatalf("unfurl_id: got %q, want unfurl-abc-123", call.Form.Get("unfurl_id"))
		}
		if call.Form.Get("source") != "conversations_history" {
			t.Fatalf("source: got %q, want conversations_history", call.Form.Get("source"))
		}
		if !strings.Contains(call.Form.Get("unfurls"), "ACME-42") {
			t.Fatalf("unfurls payload missing ACME-42: %s", call.Form.Get("unfurls"))
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for async chat.unfurl call")
	}

	// 3. app_home_opened publishes Home view asynchronously
	homeBody := []byte(`{
		"type":"event_callback",
		"team_id":"` + testTeamID + `",
		"event":{"type":"app_home_opened","user":"` + testUserID + `","tab":"home"}
	}`)
	rec = doSignedPost(t, h.srv, "/slack/events", "application/json", homeBody, time.Now(), testSigningSecret)
	if rec.Code != http.StatusOK {
		t.Fatalf("app_home_opened status: got %d, want 200", rec.Code)
	}

	select {
	case call := <-h.slack.publishHomeCh:
		if !strings.Contains(call.Body, "Alice Smith") || !strings.Contains(call.Body, "acme") {
			t.Fatalf("home view missing user identity: %s", call.Body)
		}
		if !strings.Contains(call.Body, "Notification Workspace") {
			t.Fatalf("home view missing multi-workspace selector: %s", call.Body)
		}
		if !strings.Contains(call.Body, "/slack/oauth/start?token=") {
			t.Fatalf("home view missing Step 2 OAuth start URL: %s", call.Body)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for async views.publish call")
	}
}

func TestPATConnectModalWorkspaceSwitchAndDisconnect(t *testing.T) {
	h := newTestHarness(t, testTeamID)

	// 1. Invalid PAT submission returns Slack modal validation error on pat_block
	badModalPayload, _ := json.Marshal(map[string]any{
		"type": "view_submission",
		"team": map[string]any{"id": testTeamID},
		"user": map[string]any{"id": testUserID},
		"view": map[string]any{
			"callback_id": slackhttp.CallbackPATModal,
			"state": map[string]any{
				"values": map[string]any{
					slackhttp.BlockIDPAT: map[string]any{
						slackhttp.ActionIDPATInput: map[string]any{"value": "mul_invalid_token"},
					},
				},
			},
		},
	})
	form := url.Values{"payload": {string(badModalPayload)}}.Encode()
	rec := doSignedPost(t, h.srv, "/slack/interactions", "application/x-www-form-urlencoded", []byte(form), time.Now(), testSigningSecret)
	if rec.Code != http.StatusOK {
		t.Fatalf("invalid PAT modal status: got %d, want 200", rec.Code)
	}
	var errResp struct {
		ResponseAction string            `json:"response_action"`
		Errors         map[string]string `json:"errors"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &errResp); err != nil {
		t.Fatalf("unmarshal modal error response: %v (body=%s)", err, rec.Body.String())
	}
	if errResp.ResponseAction != "errors" || errResp.Errors[slackhttp.BlockIDPAT] == "" {
		t.Fatalf("expected response_action=errors on %s, got %+v", slackhttp.BlockIDPAT, errResp)
	}

	// 2. Valid PAT submission validates, stores encrypted PAT, picks default workspace, connects notify, publishes App Home
	goodModalPayload, _ := json.Marshal(map[string]any{
		"type": "view_submission",
		"team": map[string]any{"id": testTeamID},
		"user": map[string]any{"id": testUserID},
		"view": map[string]any{
			"callback_id": slackhttp.CallbackPATModal,
			"state": map[string]any{
				"values": map[string]any{
					slackhttp.BlockIDPAT: map[string]any{
						slackhttp.ActionIDPATInput: map[string]any{"value": testPAT},
					},
				},
			},
		},
	})
	form = url.Values{"payload": {string(goodModalPayload)}}.Encode()
	rec = doSignedPost(t, h.srv, "/slack/interactions", "application/x-www-form-urlencoded", []byte(form), time.Now(), testSigningSecret)
	if rec.Code != http.StatusOK {
		t.Fatalf("valid PAT modal status: got %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}

	user, err := h.store.GetUser(context.Background(), testUserID)
	if err != nil {
		t.Fatalf("get user after PAT connect: %v", err)
	}
	if user.MulticaPAT != testPAT {
		t.Fatalf("stored PAT mismatch")
	}
	if user.MulticaWorkspaceSlug != "acme" || user.MulticaWorkspaceID != "ws-acme" {
		t.Fatalf("expected default workspace acme, got %s (%s)", user.MulticaWorkspaceSlug, user.MulticaWorkspaceID)
	}

	select {
	case <-h.slack.publishHomeCh:
	case <-time.After(2 * time.Second):
		t.Fatal("expected views.publish after PAT connect")
	}

	// 3. Customize notification preferences so we can assert they survive Disconnect
	prefsPayload, _ := json.Marshal(map[string]any{
		"type": "block_actions",
		"team": map[string]any{"id": testTeamID},
		"user": map[string]any{"id": testUserID},
		"actions": []map[string]any{
			{
				"type":      "checkboxes",
				"block_id":  "multica_prefs_block",
				"action_id": slackhttp.ActionUpdatePrefs,
				"selected_options": []map[string]any{
					{"value": string(prefs.GroupMentions)},
					{"value": string(prefs.GroupReactions)},
				},
			},
		},
	})
	form = url.Values{"payload": {string(prefsPayload)}}.Encode()
	rec = doSignedPost(t, h.srv, "/slack/interactions", "application/x-www-form-urlencoded", []byte(form), time.Now(), testSigningSecret)
	if rec.Code != http.StatusOK {
		t.Fatalf("update prefs status: got %d, want 200", rec.Code)
	}
	select {
	case <-h.slack.publishHomeCh:
	case <-time.After(2 * time.Second):
		t.Fatal("expected views.publish after update prefs")
	}

	// 4. Switch workspace via static_select dropdown
	switchPayload, _ := json.Marshal(map[string]any{
		"type": "block_actions",
		"team": map[string]any{"id": testTeamID},
		"user": map[string]any{"id": testUserID},
		"actions": []map[string]any{
			{
				"type":            "static_select",
				"block_id":        "multica_workspace_block",
				"action_id":       slackhttp.ActionSwitchWorkspace,
				"selected_option": map[string]any{"value": "ws-other|other-ws"},
			},
		},
	})
	form = url.Values{"payload": {string(switchPayload)}}.Encode()
	rec = doSignedPost(t, h.srv, "/slack/interactions", "application/x-www-form-urlencoded", []byte(form), time.Now(), testSigningSecret)
	if rec.Code != http.StatusOK {
		t.Fatalf("switch workspace status: got %d, want 200", rec.Code)
	}
	select {
	case <-h.slack.publishHomeCh:
	case <-time.After(2 * time.Second):
		t.Fatal("expected views.publish after workspace switch")
	}

	user, err = h.store.GetUser(context.Background(), testUserID)
	if err != nil {
		t.Fatalf("get user after workspace switch: %v", err)
	}
	if user.MulticaWorkspaceID != "ws-other" || user.MulticaWorkspaceSlug != "other-ws" {
		t.Fatalf("workspace switch did not persist: got %s (%s)", user.MulticaWorkspaceID, user.MulticaWorkspaceSlug)
	}

	// 4b. Pause & Resume notifications
	pausePayload, _ := json.Marshal(map[string]any{
		"type": "block_actions",
		"team": map[string]any{"id": testTeamID},
		"user": map[string]any{"id": testUserID},
		"actions": []map[string]any{
			{"type": "button", "block_id": "multica_notify_toggle", "action_id": slackhttp.ActionPauseNotifications},
		},
	})
	form = url.Values{"payload": {string(pausePayload)}}.Encode()
	rec = doSignedPost(t, h.srv, "/slack/interactions", "application/x-www-form-urlencoded", []byte(form), time.Now(), testSigningSecret)
	if rec.Code != http.StatusOK {
		t.Fatalf("pause notifications status: got %d, want 200", rec.Code)
	}
	select {
	case homeCall := <-h.slack.publishHomeCh:
		if !strings.Contains(homeCall.Body, "WS: paused") {
			t.Fatalf("expected paused state in home view: %s", homeCall.Body)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("expected views.publish after pause")
	}

	resumePayload, _ := json.Marshal(map[string]any{
		"type": "block_actions",
		"team": map[string]any{"id": testTeamID},
		"user": map[string]any{"id": testUserID},
		"actions": []map[string]any{
			{"type": "button", "block_id": "multica_notify_toggle", "action_id": slackhttp.ActionResumeNotifications},
		},
	})
	form = url.Values{"payload": {string(resumePayload)}}.Encode()
	rec = doSignedPost(t, h.srv, "/slack/interactions", "application/x-www-form-urlencoded", []byte(form), time.Now(), testSigningSecret)
	if rec.Code != http.StatusOK {
		t.Fatalf("resume notifications status: got %d, want 200", rec.Code)
	}
	select {
	case <-h.slack.publishHomeCh:
	case <-time.After(2 * time.Second):
		t.Fatal("expected views.publish after resume")
	}

	// 5. Disconnect clears tokens/identity while preserving notification preferences
	if err := h.store.SetSlackUserToken(context.Background(), testUserID, testTeamID, testUserOAuthTok); err != nil {
		t.Fatalf("seed slack user token: %v", err)
	}
	disconnectPayload, _ := json.Marshal(map[string]any{
		"type": "block_actions",
		"team": map[string]any{"id": testTeamID},
		"user": map[string]any{"id": testUserID},
		"actions": []map[string]any{
			{"type": "button", "block_id": "multica_account_actions", "action_id": slackhttp.ActionDisconnect},
		},
	})
	form = url.Values{"payload": {string(disconnectPayload)}}.Encode()
	rec = doSignedPost(t, h.srv, "/slack/interactions", "application/x-www-form-urlencoded", []byte(form), time.Now(), testSigningSecret)
	if rec.Code != http.StatusOK {
		t.Fatalf("disconnect status: got %d, want 200", rec.Code)
	}
	select {
	case homeCall := <-h.slack.publishHomeCh:
		if !strings.Contains(homeCall.Body, "Not connected") {
			t.Fatalf("expected Not connected in home view after disconnect: %s", homeCall.Body)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("expected views.publish after disconnect")
	}

	user, err = h.store.GetUser(context.Background(), testUserID)
	if err != nil {
		t.Fatalf("get user after disconnect: %v", err)
	}
	if user.MulticaPAT != "" || user.SlackUserToken != "" || user.AuthStatus != store.AuthDisconnected {
		t.Fatalf("expected cleared tokens and disconnected status, got pat=%q tok=%q status=%s", user.MulticaPAT, user.SlackUserToken, user.AuthStatus)
	}
	parsedPrefs := prefs.Parse(user.NotificationPrefsJSON)
	if !parsedPrefs[prefs.GroupMentions] || !parsedPrefs[prefs.GroupReactions] || parsedPrefs[prefs.GroupComments] {
		t.Fatalf("expected custom notification prefs preserved across disconnect, got %+v", parsedPrefs)
	}
}

func TestDMInteractionsMarkReadAndReply(t *testing.T) {
	h := newTestHarness(t, testTeamID)

	if err := h.store.UpsertMulticaConnection(context.Background(), store.UpsertMulticaParams{
		SlackUserID:          testUserID,
		SlackTeamID:          testTeamID,
		MulticaUserID:        "mc-user-1",
		MulticaUserName:      "Alice Smith",
		MulticaWorkspaceID:   "ws-acme",
		MulticaWorkspaceSlug: "acme",
		MulticaPAT:           testPAT,
		NotifyAfter:          h.now,
	}); err != nil {
		t.Fatalf("seed user: %v", err)
	}

	item := multica.InboxItem{
		ID:              "inbox-item-1",
		WorkspaceID:     "ws-acme",
		Type:            "mentioned",
		Title:           "Add Slack unfurling",
		Body:            "Can you review this?",
		IssueID:         "issue-uuid-3581",
		IssueIdentifier: "ACME-42",
	}
	fallback, blocks := notify.FormatNotificationBlocks("https://multica.ai", "acme", item, multica.WorkspaceMeta{}, false)
	blocksJSON, err := notify.MarshalBlocks(blocks)
	if err != nil {
		t.Fatalf("marshal blocks: %v", err)
	}
	if _, err := h.store.TryClaimDelivery(context.Background(), store.DeliveredNotification{
		SlackUserID:    testUserID,
		InboxItemID:    item.ID,
		DeliveryStatus: store.DeliveryDelivered,
		IssueID:        item.IssueID,
		SlackChannelID: "D_DM_1",
		SlackMessageTS: "1700000100.000100",
		CardFallback:   fallback,
		CardBlocksJSON: blocksJSON,
		CreatedAt:      h.now,
	}); err != nil {
		t.Fatalf("claim delivery: %v", err)
	}

	// 1. Click Mark Read button
	markReadPayload, _ := json.Marshal(map[string]any{
		"type":      "block_actions",
		"team":      map[string]any{"id": testTeamID},
		"user":      map[string]any{"id": testUserID},
		"channel":   map[string]any{"id": "D_DM_1"},
		"container": map[string]any{"channel_id": "D_DM_1", "message_ts": "1700000100.000100"},
		"message": map[string]any{
			"ts":     "1700000100.000100",
			"text":   fallback,
			"blocks": json.RawMessage(blocksJSON),
		},
		"actions": []map[string]any{
			{
				"type":      "button",
				"block_id":  "multica_actions",
				"action_id": notify.ActionMarkRead,
				"value":     item.ID + "|ws-acme",
			},
		},
	})
	form := url.Values{"payload": {string(markReadPayload)}}.Encode()
	rec := doSignedPost(t, h.srv, "/slack/interactions", "application/x-www-form-urlencoded", []byte(form), time.Now(), testSigningSecret)
	if rec.Code != http.StatusOK {
		t.Fatalf("mark read status: got %d, want 200", rec.Code)
	}

	select {
	case updateCall := <-h.slack.chatUpdateCh:
		if !strings.Contains(updateCall.Form.Get("blocks"), "✓ Read") {
			t.Fatalf("expected updated blocks to contain ✓ Read, got %s", updateCall.Form.Get("blocks"))
		}
		if strings.Contains(updateCall.Form.Get("blocks"), notify.ActionMarkRead) {
			t.Fatalf("expected Mark Read button stripped from updated blocks: %s", updateCall.Form.Get("blocks"))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("expected chat.update call on Mark Read")
	}

	h.multica.mu.Lock()
	readCalls := append([]string(nil), h.multica.readCalls...)
	h.multica.mu.Unlock()
	if len(readCalls) != 1 || readCalls[0] != "inbox-item-1|ws-acme" {
		t.Fatalf("expected 1 MarkInboxRead call for inbox-item-1|ws-acme, got %v", readCalls)
	}

	if _, err := h.store.GetUnreadDelivery(context.Background(), testUserID, item.ID); err != store.ErrNotFound {
		t.Fatalf("expected delivery marked read in SQLite, got err=%v", err)
	}

	// 2. Seed a second unread delivery for Reply flow
	item2 := item
	item2.ID = "inbox-item-2"
	fallback2, blocks2 := notify.FormatNotificationBlocks("https://multica.ai", "acme", item2, multica.WorkspaceMeta{}, false)
	blocks2JSON, _ := notify.MarshalBlocks(blocks2)
	if _, err := h.store.TryClaimDelivery(context.Background(), store.DeliveredNotification{
		SlackUserID:    testUserID,
		InboxItemID:    item2.ID,
		DeliveryStatus: store.DeliveryDelivered,
		IssueID:        item2.IssueID,
		SlackChannelID: "D_DM_1",
		SlackMessageTS: "1700000200.000200",
		CardFallback:   fallback2,
		CardBlocksJSON: blocks2JSON,
		CreatedAt:      h.now,
	}); err != nil {
		t.Fatalf("claim delivery 2: %v", err)
	}

	// Open reply modal
	openReplyPayload, _ := json.Marshal(map[string]any{
		"type":       "block_actions",
		"team":       map[string]any{"id": testTeamID},
		"user":       map[string]any{"id": testUserID},
		"trigger_id": "trig-reply-1",
		"channel":    map[string]any{"id": "D_DM_1"},
		"container":  map[string]any{"channel_id": "D_DM_1", "message_ts": "1700000200.000200"},
		"actions": []map[string]any{
			{
				"type":      "button",
				"block_id":  "multica_actions",
				"action_id": notify.ActionOpenReply,
				"value":     "issue-uuid-3581|ACME-42|inbox-item-2|ws-acme",
			},
		},
	})
	form = url.Values{"payload": {string(openReplyPayload)}}.Encode()
	rec = doSignedPost(t, h.srv, "/slack/interactions", "application/x-www-form-urlencoded", []byte(form), time.Now(), testSigningSecret)
	if rec.Code != http.StatusOK {
		t.Fatalf("open reply status: got %d, want 200", rec.Code)
	}
	select {
	case openCall := <-h.slack.viewsOpenCh:
		wantMeta := "issue-uuid-3581|ACME-42|inbox-item-2|ws-acme|D_DM_1|1700000200.000200"
		if !strings.Contains(openCall.Body, wantMeta) {
			t.Fatalf("reply modal missing expected private_metadata %q: %s", wantMeta, openCall.Body)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("expected views.open for reply modal")
	}

	// Submit reply modal -> 200 immediately, async comment + mark read + chat.update + threaded reply
	submitReplyPayload, _ := json.Marshal(map[string]any{
		"type": "view_submission",
		"team": map[string]any{"id": testTeamID},
		"user": map[string]any{"id": testUserID},
		"view": map[string]any{
			"callback_id":      slackhttp.CallbackReplyModal,
			"private_metadata": "issue-uuid-3581|ACME-42|inbox-item-2|ws-acme|D_DM_1|1700000200.000200",
			"state": map[string]any{
				"values": map[string]any{
					slackhttp.BlockIDReply: map[string]any{
						slackhttp.ActionIDReplyInput: map[string]any{"value": "Looks good to ship!"},
					},
				},
			},
		},
	})
	form = url.Values{"payload": {string(submitReplyPayload)}}.Encode()
	rec = doSignedPost(t, h.srv, "/slack/interactions", "application/x-www-form-urlencoded", []byte(form), time.Now(), testSigningSecret)
	if rec.Code != http.StatusOK {
		t.Fatalf("submit reply status: got %d, want 200", rec.Code)
	}

	select {
	case updateCall := <-h.slack.chatUpdateCh:
		if !strings.Contains(updateCall.Form.Get("blocks"), "✓ Read") {
			t.Fatalf("reply chat.update missing ✓ Read: %s", updateCall.Form.Get("blocks"))
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for reply chat.update")
	}

	select {
	case postCall := <-h.slack.chatPostCh:
		if postCall.Form.Get("thread_ts") != "1700000200.000200" {
			t.Fatalf("threaded reply wrong thread_ts: got %q", postCall.Form.Get("thread_ts"))
		}
		if !strings.Contains(postCall.Form.Get("text"), "ACME-42") {
			t.Fatalf("threaded confirmation reply missing issue identifier: %s", postCall.Form.Get("text"))
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for threaded confirmation reply")
	}

	h.multica.mu.Lock()
	comments := append([]struct{ IssueID, WorkspaceID, Content, Auth string }(nil), h.multica.commentCalls...)
	readCalls = append([]string(nil), h.multica.readCalls...)
	h.multica.mu.Unlock()
	if len(comments) != 1 || comments[0].IssueID != "issue-uuid-3581" || comments[0].Content != "Looks good to ship!" {
		t.Fatalf("unexpected comment calls: %+v", comments)
	}
	if len(readCalls) != 2 || readCalls[1] != "inbox-item-2|ws-acme" {
		t.Fatalf("expected MarkInboxRead after reply, got %v", readCalls)
	}

	// 3. Regression: replying to an already-read notification (inbox-item-1) posts comment +
	// threaded confirmation and must NOT call chat.update with empty blocks to wipe the DM card.
	replyAlreadyReadPayload, _ := json.Marshal(map[string]any{
		"type": "view_submission",
		"team": map[string]any{"id": testTeamID},
		"user": map[string]any{"id": testUserID},
		"view": map[string]any{
			"callback_id":      slackhttp.CallbackReplyModal,
			"private_metadata": "issue-uuid-3581|ACME-42|inbox-item-1|ws-acme|D_DM_1|1700000100.000100",
			"state": map[string]any{
				"values": map[string]any{
					slackhttp.BlockIDReply: map[string]any{
						slackhttp.ActionIDReplyInput: map[string]any{"value": "Following up after reading"},
					},
				},
			},
		},
	})
	form = url.Values{"payload": {string(replyAlreadyReadPayload)}}.Encode()
	rec = doSignedPost(t, h.srv, "/slack/interactions", "application/x-www-form-urlencoded", []byte(form), time.Now(), testSigningSecret)
	if rec.Code != http.StatusOK {
		t.Fatalf("reply already-read status: got %d, want 200", rec.Code)
	}

	select {
	case postCall := <-h.slack.chatPostCh:
		if postCall.Form.Get("thread_ts") != "1700000100.000100" {
			t.Fatalf("already-read threaded reply wrong thread_ts: got %q", postCall.Form.Get("thread_ts"))
		}
		if !strings.Contains(postCall.Form.Get("text"), "ACME-42") {
			t.Fatalf("already-read threaded confirmation reply missing issue identifier: %s", postCall.Form.Get("text"))
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for threaded confirmation reply on already-read notification")
	}

	select {
	case unexpectedUpdate := <-h.slack.chatUpdateCh:
		t.Fatalf("expected NO chat.update when replying to already-read notification, got blocks=%s", unexpectedUpdate.Form.Get("blocks"))
	default:
	}

	h.multica.mu.Lock()
	comments = append([]struct{ IssueID, WorkspaceID, Content, Auth string }(nil), h.multica.commentCalls...)
	h.multica.mu.Unlock()
	if len(comments) != 2 || comments[1].IssueID != "issue-uuid-3581" || comments[1].Content != "Following up after reading" {
		t.Fatalf("expected second comment on already-read reply, got %+v", comments)
	}
}

func TestSlashCommandAndOAuthFlow(t *testing.T) {
	h := newTestHarness(t, testTeamID)

	// 1. /multica connect opens PAT modal
	connectForm := url.Values{
		"team_id":    {testTeamID},
		"user_id":    {testUserID},
		"command":    {"/multica"},
		"text":       {"connect"},
		"trigger_id": {"trig-cmd-1"},
	}.Encode()
	rec := doSignedPost(t, h.srv, "/slack/commands", "application/x-www-form-urlencoded", []byte(connectForm), time.Now(), testSigningSecret)
	if rec.Code != http.StatusOK {
		t.Fatalf("/multica connect status: got %d, want 200", rec.Code)
	}
	select {
	case openCall := <-h.slack.viewsOpenCh:
		if !strings.Contains(openCall.Body, slackhttp.CallbackPATModal) {
			t.Fatalf("expected PAT modal open, got %s", openCall.Body)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("expected views.open on /multica connect")
	}

	// Seed linked user and check /multica status
	if err := h.store.UpsertMulticaConnection(context.Background(), store.UpsertMulticaParams{
		SlackUserID:          testUserID,
		SlackTeamID:          testTeamID,
		MulticaUserID:        "mc-user-1",
		MulticaUserName:      "Alice Smith",
		MulticaWorkspaceID:   "ws-acme",
		MulticaWorkspaceSlug: "acme",
		MulticaPAT:           testPAT,
		NotifyAfter:          h.now,
	}); err != nil {
		t.Fatalf("seed user: %v", err)
	}

	statusForm := url.Values{
		"team_id": {testTeamID},
		"user_id": {testUserID},
		"command": {"/multica"},
		"text":    {"status"},
	}.Encode()
	rec = doSignedPost(t, h.srv, "/slack/commands", "application/x-www-form-urlencoded", []byte(statusForm), time.Now(), testSigningSecret)
	if rec.Code != http.StatusOK {
		t.Fatalf("/multica status: got %d, want 200", rec.Code)
	}
	var statusResp struct {
		ResponseType string `json:"response_type"`
		Text         string `json:"text"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &statusResp); err != nil {
		t.Fatalf("unmarshal status response: %v", err)
	}
	if statusResp.ResponseType != "ephemeral" {
		t.Fatalf("expected ephemeral response_type, got %q", statusResp.ResponseType)
	}
	if !strings.Contains(statusResp.Text, "acme") || !strings.Contains(statusResp.Text, "Mentions") {
		t.Fatalf("status summary missing expected details: %s", statusResp.Text)
	}

	// 2. OAuth start with invalid token -> 400
	badStartReq := httptest.NewRequest(http.MethodGet, "/slack/oauth/start?token=not-valid", nil)
	rec = httptest.NewRecorder()
	h.srv.ServeHTTP(rec, badStartReq)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid oauth start token: got %d, want 400", rec.Code)
	}

	// 3. OAuth start with valid token -> 302 redirect to Slack with stored state
	startTok := crypto.SignStartToken(h.cfg.EncryptionKey, testUserID, h.now)
	startReq := httptest.NewRequest(http.MethodGet, "/slack/oauth/start?token="+url.QueryEscape(startTok), nil)
	rec = httptest.NewRecorder()
	h.srv.ServeHTTP(rec, startReq)
	if rec.Code != http.StatusFound {
		t.Fatalf("oauth start status: got %d, want 302", rec.Code)
	}
	loc, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		t.Fatalf("parse redirect Location: %v", err)
	}
	if loc.Host != "slack.com" || loc.Path != "/oauth/v2/authorize" {
		t.Fatalf("unexpected authorize URL: %s", loc.String())
	}
	if loc.Query().Get("user_scope") != "links:read,links:write" {
		t.Fatalf("unexpected user_scope: %q", loc.Query().Get("user_scope"))
	}
	if loc.Query().Get("client_id") != testClientID {
		t.Fatalf("unexpected client_id: %q", loc.Query().Get("client_id"))
	}
	state := loc.Query().Get("state")
	if len(state) != 64 {
		t.Fatalf("expected 64-char hex state, got %q (len=%d)", state, len(state))
	}

	// 4. OAuth callback consumes state, stores encrypted xoxp-, refreshes App Home, returns HTML
	cbReq := httptest.NewRequest(http.MethodGet, "/slack/oauth/callback?state="+url.QueryEscape(state)+"&code=oauth-code-xyz", nil)
	rec = httptest.NewRecorder()
	h.srv.ServeHTTP(rec, cbReq)
	if rec.Code != http.StatusOK {
		t.Fatalf("oauth callback status: got %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Header().Get("Content-Type"), "text/html") {
		t.Fatalf("expected text/html content type, got %q", rec.Header().Get("Content-Type"))
	}
	if strings.Contains(rec.Body.String(), testUserOAuthTok) {
		t.Fatal("HTML confirmation page must never leak access token")
	}

	user, err := h.store.GetUser(context.Background(), testUserID)
	if err != nil {
		t.Fatalf("get user after oauth: %v", err)
	}
	if user.SlackUserToken != testUserOAuthTok {
		t.Fatalf("expected stored Slack user token, got %q", user.SlackUserToken)
	}

	select {
	case <-h.slack.publishHomeCh:
	case <-time.After(2 * time.Second):
		t.Fatal("expected App Home refresh after oauth callback")
	}

	// 5. Replayed state is rejected
	replayReq := httptest.NewRequest(http.MethodGet, "/slack/oauth/callback?state="+url.QueryEscape(state)+"&code=oauth-code-xyz", nil)
	rec = httptest.NewRecorder()
	h.srv.ServeHTTP(rec, replayReq)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("replayed oauth state: got %d, want 400", rec.Code)
	}
}

func TestHealthAndReadyz(t *testing.T) {
	h := newTestHarness(t, testTeamID)

	// /healthz -> 200 {"status":"ok"}
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	h.srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("/healthz status: got %d, want 200", rec.Code)
	}

	// Seed 2 linked users, 1 auth_failed
	if err := h.store.UpsertMulticaConnection(context.Background(), store.UpsertMulticaParams{
		SlackUserID:          "U_OK",
		SlackTeamID:          testTeamID,
		MulticaUserID:        "mc-1",
		MulticaUserName:      "ok@example.com",
		MulticaWorkspaceID:   "ws-1",
		MulticaWorkspaceSlug: "acme",
		MulticaPAT:           testPAT,
		NotifyAfter:          h.now,
	}); err != nil {
		t.Fatalf("seed U_OK: %v", err)
	}
	if err := h.store.UpsertMulticaConnection(context.Background(), store.UpsertMulticaParams{
		SlackUserID:          "U_BAD",
		SlackTeamID:          testTeamID,
		MulticaUserID:        "mc-2",
		MulticaUserName:      "bad@example.com",
		MulticaWorkspaceID:   "ws-1",
		MulticaWorkspaceSlug: "acme",
		MulticaPAT:           "mul_secret_other_pat",
		NotifyAfter:          h.now,
	}); err != nil {
		t.Fatalf("seed U_BAD: %v", err)
	}
	if err := h.store.SetAuthStatus(context.Background(), "U_BAD", store.AuthFailed); err != nil {
		t.Fatalf("set auth failed: %v", err)
	}

	// /readyz -> 200 with aggregate counts and zero PII/tokens
	req = httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rec = httptest.NewRecorder()
	h.srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("/readyz status: got %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, forbidden := range []string{testPAT, "mul_secret_other_pat", "example.com", "U_OK", "U_BAD"} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("/readyz leaked %q: %s", forbidden, body)
		}
	}
	var ready struct {
		Status       string `json:"status"`
		LinkedUsers  int    `json:"linked_users"`
		WSConnected  int    `json:"ws_connected"`
		WSAuthFailed int    `json:"ws_auth_failed"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &ready); err != nil {
		t.Fatalf("unmarshal /readyz: %v", err)
	}
	if ready.Status != "ready" || ready.LinkedUsers != 2 || ready.WSAuthFailed != 1 {
		t.Fatalf("unexpected /readyz payload: %+v", ready)
	}

	// Closed DB -> 503 not_ready
	if err := h.store.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}
	req = httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rec = httptest.NewRecorder()
	h.srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("/readyz with closed DB: got %d, want 503", rec.Code)
	}
}

func TestPATRotationPreservesSelectedWorkspace(t *testing.T) {
	h := newTestHarness(t, testTeamID)

	// Seed user connected to non-default workspace ws-other (default slug is acme -> ws-acme).
	if err := h.store.UpsertMulticaConnection(context.Background(), store.UpsertMulticaParams{
		SlackUserID:          testUserID,
		SlackTeamID:          testTeamID,
		MulticaUserID:        "mc-user-1",
		MulticaUserName:      "Alice Smith",
		MulticaWorkspaceID:   "ws-other",
		MulticaWorkspaceSlug: "other-ws",
		MulticaPAT:           testPAT,
		NotifyAfter:          h.now,
	}); err != nil {
		t.Fatalf("seed user: %v", err)
	}

	// Rotate PAT while ws-other is still accessible: workspace selection must be preserved.
	const rotatedPAT = "mul_rotated_pat_99999"
	h.multica.mu.Lock()
	h.multica.validPAT = rotatedPAT
	h.multica.mu.Unlock()

	submitPAT := func(pat string) {
		t.Helper()
		payload, _ := json.Marshal(map[string]any{
			"type": "view_submission",
			"team": map[string]any{"id": testTeamID},
			"user": map[string]any{"id": testUserID},
			"view": map[string]any{
				"callback_id": slackhttp.CallbackPATModal,
				"state": map[string]any{
					"values": map[string]any{
						slackhttp.BlockIDPAT: map[string]any{
							slackhttp.ActionIDPATInput: map[string]any{"value": pat},
						},
					},
				},
			},
		})
		form := url.Values{"payload": {string(payload)}}.Encode()
		rec := doSignedPost(t, h.srv, "/slack/interactions", "application/x-www-form-urlencoded", []byte(form), time.Now(), testSigningSecret)
		if rec.Code != http.StatusOK {
			t.Fatalf("PAT modal submit status: got %d, want 200 (body=%s)", rec.Code, rec.Body.String())
		}
		select {
		case <-h.slack.publishHomeCh:
		case <-time.After(2 * time.Second):
			t.Fatal("expected views.publish after PAT rotation")
		}
	}

	submitPAT(rotatedPAT)

	user, err := h.store.GetUser(context.Background(), testUserID)
	if err != nil {
		t.Fatalf("get user after PAT rotation: %v", err)
	}
	if user.MulticaPAT != rotatedPAT {
		t.Fatalf("expected rotated PAT saved")
	}
	if user.MulticaWorkspaceID != "ws-other" || user.MulticaWorkspaceSlug != "other-ws" {
		t.Fatalf("expected existing workspace ws-other preserved on PAT rotation, got %s (%s)", user.MulticaWorkspaceID, user.MulticaWorkspaceSlug)
	}

	// If the previously selected workspace is no longer in the new PAT's workspace list, fall back to default.
	const rotatedPAT2 = "mul_rotated_pat_88888"
	h.multica.mu.Lock()
	h.multica.validPAT = rotatedPAT2
	h.multica.workspaces = []multica.Workspace{
		{ID: "ws-acme", Name: "Acme Corp", Slug: "acme"},
	}
	h.multica.mu.Unlock()

	submitPAT(rotatedPAT2)

	user, err = h.store.GetUser(context.Background(), testUserID)
	if err != nil {
		t.Fatalf("get user after second PAT rotation: %v", err)
	}
	if user.MulticaWorkspaceID != "ws-acme" || user.MulticaWorkspaceSlug != "acme" {
		t.Fatalf("expected fallback to default workspace ws-acme, got %s (%s)", user.MulticaWorkspaceID, user.MulticaWorkspaceSlug)
	}
}

func TestConcurrentRunAsyncAndShutdown(t *testing.T) {
	h := newTestHarness(t, testTeamID)

	homeBody := []byte(`{
		"type":"event_callback",
		"team_id":"` + testTeamID + `",
		"event":{"type":"app_home_opened","user":"` + testUserID + `","tab":"home"}
	}`)

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 10; j++ {
				rec := doSignedPost(t, h.srv, "/slack/events", "application/json", homeBody, time.Now(), testSigningSecret)
				if rec.Code != http.StatusOK {
					t.Errorf("app_home_opened status: got %d, want 200", rec.Code)
				}
			}
		}()
	}

	time.Sleep(5 * time.Millisecond)
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := h.srv.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("Shutdown returned error: %v", err)
	}
	wg.Wait()

	// Post-shutdown requests must still return 200 without panicking or spawning new async goroutines.
	rec := doSignedPost(t, h.srv, "/slack/events", "application/json", homeBody, time.Now(), testSigningSecret)
	if rec.Code != http.StatusOK {
		t.Fatalf("post-shutdown app_home_opened status: got %d, want 200", rec.Code)
	}
}

func TestOAuthCacheControlAndNilStoreGuard(t *testing.T) {
	h := newTestHarness(t, testTeamID)

	// Valid server sets Cache-Control: no-store on both OAuth endpoints.
	tok := crypto.SignStartToken(h.cfg.EncryptionKey, testUserID, h.now)
	startReq := httptest.NewRequest(http.MethodGet, "/slack/oauth/start?token="+url.QueryEscape(tok), nil)
	rec := httptest.NewRecorder()
	h.srv.ServeHTTP(rec, startReq)
	if rec.Code != http.StatusFound {
		t.Fatalf("oauth start status: got %d, want 302", rec.Code)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("oauth start Cache-Control: got %q, want no-store", got)
	}

	cbReq := httptest.NewRequest(http.MethodGet, "/slack/oauth/callback?error=access_denied", nil)
	rec = httptest.NewRecorder()
	h.srv.ServeHTTP(rec, cbReq)
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("oauth callback Cache-Control: got %q, want no-store", got)
	}

	// Nil-store server guards both OAuth endpoints cleanly with 503 + Cache-Control: no-store.
	nilSrv := slackhttp.NewServer(slackhttp.Deps{
		Config: h.cfg,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = nilSrv.Shutdown(ctx)
	})

	rec = httptest.NewRecorder()
	nilSrv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/slack/oauth/start?token=abc", nil))
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("nil-store oauth start: code=%d cache-control=%q", rec.Code, rec.Header().Get("Cache-Control"))
	}

	rec = httptest.NewRecorder()
	nilSrv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/slack/oauth/callback?state=s&code=c", nil))
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("nil-store oauth callback: code=%d cache-control=%q", rec.Code, rec.Header().Get("Cache-Control"))
	}
}
