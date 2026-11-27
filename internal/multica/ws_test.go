package multica

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

func TestBuildWSURL(t *testing.T) {
	cases := []struct {
		name    string
		apiURL  string
		wantPfx string
	}{
		{name: "https to wss", apiURL: "https://api.multica.ai/", wantPfx: "wss://api.multica.ai/ws?"},
		{name: "http to ws", apiURL: "http://127.0.0.1:8080", wantPfx: "ws://127.0.0.1:8080/ws?"},
		{name: "wss preserved", apiURL: "wss://api.multica.ai", wantPfx: "wss://api.multica.ai/ws?"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := buildWSURL(tc.apiURL, "ws-123", "1.2.3")
			if err != nil {
				t.Fatalf("buildWSURL error: %v", err)
			}
			if !strings.HasPrefix(got, tc.wantPfx) {
				t.Fatalf("got %q, want prefix %q", got, tc.wantPfx)
			}
		})
	}
}

func TestWSAuthHandshakeAndEventDelivery(t *testing.T) {
	var (
		gotPath     string
		gotQuery    string
		gotAuthType string
		gotToken    string
	)
	var mu sync.Mutex

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		mu.Unlock()

		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Errorf("Accept failed: %v", err)
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
			t.Errorf("read auth frame failed: %v", err)
			return
		}
		mu.Lock()
		gotAuthType = authFrame.Type
		gotToken = authFrame.Payload.Token
		mu.Unlock()

		if err := wsjson.Write(r.Context(), conn, map[string]any{"type": "auth_ack"}); err != nil {
			t.Errorf("write auth_ack failed: %v", err)
			return
		}
		if err := wsjson.Write(r.Context(), conn, map[string]any{
			"type": "inbox:new",
			"payload": map[string]any{
				"item": map[string]any{"id": "n1"},
			},
		}); err != nil {
			t.Errorf("write inbox:new failed: %v", err)
			return
		}

		// Drain until client closes cleanly on ctx cancel.
		for {
			var msg json.RawMessage
			if err := wsjson.Read(r.Context(), conn, &msg); err != nil {
				return
			}
		}
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	connectedCh := make(chan struct{}, 1)
	eventCh := make(chan WSEnvelope, 1)
	var stateMu sync.Mutex
	var states []bool

	done := make(chan struct{})
	go func() {
		defer close(done)
		RunUserStream(ctx, WSConfig{
			APIURL:      srv.URL,
			WorkspaceID: "ws-uuid-1",
			Token:       "mul_ok",
			Version:     "0.1.0",
		}, WSCallbacks{
			OnConnected: func(ctx context.Context) {
				select {
				case connectedCh <- struct{}{}:
				default:
				}
			},
			OnEvent: func(ctx context.Context, env WSEnvelope) {
				eventCh <- env
			},
			OnAuthFailed: func(ctx context.Context, reason string) {
				t.Errorf("unexpected OnAuthFailed: %s", reason)
			},
			OnStateChange: func(connected bool) {
				stateMu.Lock()
				states = append(states, connected)
				stateMu.Unlock()
			},
		})
	}()

	select {
	case <-connectedCh:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for OnConnected")
	}

	select {
	case env := <-eventCh:
		if env.Type != "inbox:new" {
			t.Fatalf("expected event type inbox:new, got %q", env.Type)
		}
		var payload struct {
			Item struct {
				ID string `json:"id"`
			} `json:"item"`
		}
		if err := json.Unmarshal(env.Payload, &payload); err != nil {
			t.Fatalf("unmarshal payload: %v", err)
		}
		if payload.Item.ID != "n1" {
			t.Fatalf("expected item id n1, got %q", payload.Item.ID)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for OnEvent")
	}

	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("RunUserStream did not exit after cancel")
	}

	mu.Lock()
	if gotPath != "/ws" {
		t.Fatalf("expected path /ws, got %q", gotPath)
	}
	if !strings.Contains(gotQuery, "workspace_id=ws-uuid-1") ||
		!strings.Contains(gotQuery, "client_platform=multica-slack") ||
		!strings.Contains(gotQuery, "client_version=0.1.0") {
		t.Fatalf("unexpected query params: %q", gotQuery)
	}
	if gotAuthType != "auth" || gotToken != "mul_ok" {
		t.Fatalf("unexpected auth frame: type=%q token=%q", gotAuthType, gotToken)
	}
	mu.Unlock()

	stateMu.Lock()
	defer stateMu.Unlock()
	if len(states) < 2 || !states[0] || states[len(states)-1] {
		t.Fatalf("expected state transitions [true, ..., false], got %v", states)
	}
}

func TestWSAuthErrorHaltsRetries(t *testing.T) {
	t.Run("error frame before auth_ack", func(t *testing.T) {
		var attempts atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			attempts.Add(1)
			conn, err := websocket.Accept(w, r, nil)
			if err != nil {
				t.Errorf("Accept failed: %v", err)
				return
			}
			defer func() { _ = conn.CloseNow() }()

			var authFrame json.RawMessage
			if err := wsjson.Read(r.Context(), conn, &authFrame); err != nil {
				return
			}
			_ = wsjson.Write(r.Context(), conn, map[string]any{"error": "invalid token"})
			_ = conn.Close(websocket.StatusNormalClosure, "invalid token")
		}))
		defer srv.Close()

		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()

		var authFailedCalls atomic.Int32
		var gotReason string
		var reasonMu sync.Mutex

		done := make(chan struct{})
		go func() {
			defer close(done)
			RunUserStream(ctx, WSConfig{
				APIURL:         srv.URL,
				WorkspaceID:    "ws-1",
				Token:          "mul_bad",
				Version:        "0.1.0",
				InitialBackoff: 10 * time.Millisecond,
				MaxBackoff:     20 * time.Millisecond,
			}, WSCallbacks{
				OnAuthFailed: func(ctx context.Context, reason string) {
					authFailedCalls.Add(1)
					reasonMu.Lock()
					gotReason = reason
					reasonMu.Unlock()
				},
			})
		}()

		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("RunUserStream did not halt on auth error frame")
		}

		if got := authFailedCalls.Load(); got != 1 {
			t.Fatalf("expected OnAuthFailed called once, got %d", got)
		}
		if got := attempts.Load(); got != 1 {
			t.Fatalf("expected 1 connection attempt without retry, got %d", got)
		}
		reasonMu.Lock()
		if !strings.Contains(gotReason, "invalid token") {
			t.Fatalf("expected reason to contain 'invalid token', got %q", gotReason)
		}
		reasonMu.Unlock()
	})

	t.Run("http 401 on upgrade", func(t *testing.T) {
		var attempts atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			attempts.Add(1)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
		}))
		defer srv.Close()

		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()

		var authFailedCalls atomic.Int32
		done := make(chan struct{})
		go func() {
			defer close(done)
			RunUserStream(ctx, WSConfig{
				APIURL:         srv.URL,
				WorkspaceID:    "ws-1",
				Token:          "mul_bad",
				Version:        "0.1.0",
				InitialBackoff: 10 * time.Millisecond,
				MaxBackoff:     20 * time.Millisecond,
			}, WSCallbacks{
				OnAuthFailed: func(ctx context.Context, reason string) {
					authFailedCalls.Add(1)
				},
			})
		}()

		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("RunUserStream did not halt on HTTP 401")
		}

		if got := authFailedCalls.Load(); got != 1 {
			t.Fatalf("expected OnAuthFailed called once, got %d", got)
		}
		if got := attempts.Load(); got != 1 {
			t.Fatalf("expected 1 connection attempt without retry, got %d", got)
		}
	})

	t.Run("auth_error type frame and http 403", func(t *testing.T) {
		for _, status := range []int{http.StatusForbidden} {
			var attempts atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				attempts.Add(1)
				http.Error(w, "forbidden", status)
			}))

			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			var authFailedCalls atomic.Int32
			RunUserStream(ctx, WSConfig{
				APIURL:         srv.URL,
				WorkspaceID:    "ws-1",
				Token:          "mul_bad",
				InitialBackoff: 10 * time.Millisecond,
			}, WSCallbacks{
				OnAuthFailed: func(ctx context.Context, reason string) {
					authFailedCalls.Add(1)
				},
			})
			cancel()
			srv.Close()

			if authFailedCalls.Load() != 1 || attempts.Load() != 1 {
				t.Fatalf("expected 1 attempt and 1 OnAuthFailed on 403, got attempts=%d callbacks=%d", attempts.Load(), authFailedCalls.Load())
			}
		}

		var attempts atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			attempts.Add(1)
			conn, err := websocket.Accept(w, r, nil)
			if err != nil {
				return
			}
			defer func() { _ = conn.CloseNow() }()
			var authFrame json.RawMessage
			_ = wsjson.Read(r.Context(), conn, &authFrame)
			_ = wsjson.Write(r.Context(), conn, map[string]any{
				"type":    "auth_error",
				"payload": map[string]any{"message": "expired token"},
			})
		}))
		defer srv.Close()

		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		var authFailedCalls atomic.Int32
		RunUserStream(ctx, WSConfig{
			APIURL:         srv.URL,
			WorkspaceID:    "ws-1",
			Token:          "mul_bad",
			InitialBackoff: 10 * time.Millisecond,
		}, WSCallbacks{
			OnAuthFailed: func(ctx context.Context, reason string) {
				authFailedCalls.Add(1)
			},
		})
		if authFailedCalls.Load() != 1 || attempts.Load() != 1 {
			t.Fatalf("expected 1 attempt and 1 OnAuthFailed on auth_error, got attempts=%d callbacks=%d", attempts.Load(), authFailedCalls.Load())
		}
	})

	t.Run("error type frame before auth_ack", func(t *testing.T) {
		cases := []struct {
			name       string
			frame      map[string]any
			wantReason string
		}{
			{
				name: "payload message",
				frame: map[string]any{
					"type":    "error",
					"payload": map[string]any{"message": "unauthorized"},
				},
				wantReason: "unauthorized",
			},
			{
				name: "payload error",
				frame: map[string]any{
					"type":    "error",
					"payload": map[string]any{"error": "revoked token"},
				},
				wantReason: "revoked token",
			},
			{
				name:       "no payload fallback",
				frame:      map[string]any{"type": "error"},
				wantReason: "auth_error",
			},
		}

		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				var attempts atomic.Int32
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					attempts.Add(1)
					conn, err := websocket.Accept(w, r, nil)
					if err != nil {
						return
					}
					defer func() { _ = conn.CloseNow() }()
					var authFrame json.RawMessage
					if err := wsjson.Read(r.Context(), conn, &authFrame); err != nil {
						return
					}
					_ = wsjson.Write(r.Context(), conn, tc.frame)
				}))
				defer srv.Close()

				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				defer cancel()

				var authFailedCalls atomic.Int32
				var gotReason string
				var reasonMu sync.Mutex

				done := make(chan struct{})
				go func() {
					defer close(done)
					RunUserStream(ctx, WSConfig{
						APIURL:         srv.URL,
						WorkspaceID:    "ws-1",
						Token:          "mul_bad",
						InitialBackoff: 10 * time.Millisecond,
						MaxBackoff:     20 * time.Millisecond,
					}, WSCallbacks{
						OnAuthFailed: func(ctx context.Context, reason string) {
							authFailedCalls.Add(1)
							reasonMu.Lock()
							gotReason = reason
							reasonMu.Unlock()
						},
					})
				}()

				select {
				case <-done:
				case <-time.After(2 * time.Second):
					t.Fatal("RunUserStream did not halt on error type frame")
				}

				if got := authFailedCalls.Load(); got != 1 {
					t.Fatalf("expected OnAuthFailed called once, got %d", got)
				}
				if got := attempts.Load(); got != 1 {
					t.Fatalf("expected 1 connection attempt without retry, got %d", got)
				}
				reasonMu.Lock()
				if gotReason != tc.wantReason {
					t.Fatalf("expected reason %q, got %q", tc.wantReason, gotReason)
				}
				reasonMu.Unlock()
			})
		}
	})
}

func TestWSLargeInboxEvent(t *testing.T) {
	largeBody := strings.Repeat("x", 64*1024) // >32 KiB default coder/websocket read limit

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Errorf("Accept failed: %v", err)
			return
		}
		defer func() { _ = conn.CloseNow() }()

		var authFrame json.RawMessage
		if err := wsjson.Read(r.Context(), conn, &authFrame); err != nil {
			t.Errorf("read auth frame failed: %v", err)
			return
		}
		if err := wsjson.Write(r.Context(), conn, map[string]any{"type": "auth_ack"}); err != nil {
			t.Errorf("write auth_ack failed: %v", err)
			return
		}
		if err := wsjson.Write(r.Context(), conn, map[string]any{
			"type": "inbox:new",
			"payload": map[string]any{
				"item": map[string]any{
					"id":   "large-1",
					"body": largeBody,
				},
			},
		}); err != nil {
			t.Errorf("write large inbox:new failed: %v", err)
			return
		}

		for {
			var msg json.RawMessage
			if err := wsjson.Read(r.Context(), conn, &msg); err != nil {
				return
			}
		}
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	eventCh := make(chan WSEnvelope, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		RunUserStream(ctx, WSConfig{
			APIURL:      srv.URL,
			WorkspaceID: "ws-1",
			Token:       "mul_ok",
			Version:     "0.1.0",
		}, WSCallbacks{
			OnEvent: func(ctx context.Context, env WSEnvelope) {
				eventCh <- env
			},
			OnAuthFailed: func(ctx context.Context, reason string) {
				t.Errorf("unexpected OnAuthFailed: %s", reason)
			},
		})
	}()

	select {
	case env := <-eventCh:
		if env.Type != "inbox:new" {
			t.Fatalf("expected event type inbox:new, got %q", env.Type)
		}
		var payload struct {
			Item struct {
				ID   string `json:"id"`
				Body string `json:"body"`
			} `json:"item"`
		}
		if err := json.Unmarshal(env.Payload, &payload); err != nil {
			t.Fatalf("unmarshal payload: %v", err)
		}
		if payload.Item.ID != "large-1" || len(payload.Item.Body) != len(largeBody) {
			t.Fatalf("unexpected payload item: id=%q body_len=%d", payload.Item.ID, len(payload.Item.Body))
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for large inbox:new event")
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("RunUserStream did not exit after cancel")
	}
}

func TestWSPingPongWatchdogAndReconnect(t *testing.T) {
	var attempt atomic.Int32
	var secondConnPongs atomic.Int32
	secondHealthyCh := make(chan struct{}, 1)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := attempt.Add(1)
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Errorf("Accept failed: %v", err)
			return
		}
		defer func() { _ = conn.CloseNow() }()

		var authFrame json.RawMessage
		if err := wsjson.Read(r.Context(), conn, &authFrame); err != nil {
			return
		}
		if err := wsjson.Write(r.Context(), conn, map[string]any{"type": "auth_ack"}); err != nil {
			return
		}

		for {
			var env WSEnvelope
			if err := wsjson.Read(r.Context(), conn, &env); err != nil {
				return
			}
			if env.Type == "ping" {
				if n == 1 {
					// Intentionally ignore ping on first connection to trigger PongTimeout watchdog.
					continue
				}
				if err := wsjson.Write(r.Context(), conn, map[string]any{"type": "pong"}); err != nil {
					return
				}
				if secondConnPongs.Add(1) >= 2 {
					select {
					case secondHealthyCh <- struct{}{}:
					default:
					}
				}
			}
		}
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var connectedCount atomic.Int32
	var stateMu sync.Mutex
	var states []bool

	done := make(chan struct{})
	go func() {
		defer close(done)
		RunUserStream(ctx, WSConfig{
			APIURL:         srv.URL,
			WorkspaceID:    "ws-1",
			Token:          "mul_ok",
			Version:        "0.1.0",
			PingInterval:   50 * time.Millisecond,
			PongTimeout:    50 * time.Millisecond,
			InitialBackoff: 20 * time.Millisecond,
			MaxBackoff:     100 * time.Millisecond,
		}, WSCallbacks{
			OnConnected: func(ctx context.Context) {
				connectedCount.Add(1)
			},
			OnAuthFailed: func(ctx context.Context, reason string) {
				t.Errorf("unexpected OnAuthFailed: %s", reason)
			},
			OnStateChange: func(connected bool) {
				stateMu.Lock()
				states = append(states, connected)
				stateMu.Unlock()
			},
		})
	}()

	select {
	case <-secondHealthyCh:
	case <-time.After(4 * time.Second):
		t.Fatalf("timed out waiting for reconnect and pong exchange (attempts=%d, pongs=%d)", attempt.Load(), secondConnPongs.Load())
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("RunUserStream did not exit on cancel")
	}

	if attempt.Load() != 2 {
		t.Fatalf("expected exactly 2 connection attempts, got %d", attempt.Load())
	}
	if connectedCount.Load() != 2 {
		t.Fatalf("expected 2 OnConnected calls, got %d", connectedCount.Load())
	}

	stateMu.Lock()
	defer stateMu.Unlock()
	// Expect [true, false, true, false]
	if len(states) != 4 || !states[0] || states[1] || !states[2] || states[3] {
		t.Fatalf("expected state sequence [true false true false], got %v", states)
	}
}
