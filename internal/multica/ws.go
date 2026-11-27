package multica

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

const (
	defaultWSPingInterval   = 30 * time.Second
	defaultWSPongTimeout    = 10 * time.Second
	defaultWSAuthTimeout    = 10 * time.Second
	defaultWSInitialBackoff = 1 * time.Second
	defaultWSMaxBackoff     = 60 * time.Second
	stableConnThreshold     = 30 * time.Second
	wsClientPlatform        = "multica-slack"
	backoffJitterFraction   = 0.20
)

// WSConfig configures a per-user Multica Cloud WebSocket connection.
type WSConfig struct {
	APIURL         string
	WorkspaceID    string
	Token          string
	Version        string
	PingInterval   time.Duration
	PongTimeout    time.Duration
	AuthTimeout    time.Duration
	InitialBackoff time.Duration
	MaxBackoff     time.Duration
}

func (c WSConfig) withDefaults() WSConfig {
	if c.PingInterval <= 0 {
		c.PingInterval = defaultWSPingInterval
	}
	if c.PongTimeout <= 0 {
		c.PongTimeout = defaultWSPongTimeout
	}
	if c.AuthTimeout <= 0 {
		c.AuthTimeout = defaultWSAuthTimeout
	}
	if c.InitialBackoff <= 0 {
		c.InitialBackoff = defaultWSInitialBackoff
	}
	if c.MaxBackoff <= 0 {
		c.MaxBackoff = defaultWSMaxBackoff
	}
	if c.MaxBackoff < c.InitialBackoff {
		c.MaxBackoff = c.InitialBackoff
	}
	return c
}

// WSCallbacks receives lifecycle and event notifications from RunUserStream.
type WSCallbacks struct {
	OnConnected   func(ctx context.Context)
	OnEvent       func(ctx context.Context, env WSEnvelope)
	OnAuthFailed  func(ctx context.Context, reason string)
	OnStateChange func(connected bool)
}

func (cb WSCallbacks) notifyConnected(ctx context.Context) {
	if cb.OnConnected != nil {
		cb.OnConnected(ctx)
	}
}

func (cb WSCallbacks) notifyEvent(ctx context.Context, env WSEnvelope) {
	if cb.OnEvent != nil {
		cb.OnEvent(ctx, env)
	}
}

func (cb WSCallbacks) notifyAuthFailed(ctx context.Context, reason string) {
	if cb.OnAuthFailed != nil {
		cb.OnAuthFailed(ctx, reason)
	}
}

func (cb WSCallbacks) notifyStateChange(connected bool) {
	if cb.OnStateChange != nil {
		cb.OnStateChange(connected)
	}
}

type wsAuthFrame struct {
	Type    string        `json:"type"`
	Payload wsAuthPayload `json:"payload"`
}

type wsAuthPayload struct {
	Token string `json:"token"`
}

type wsPingFrame struct {
	Type string `json:"type"`
}

// RunUserStream maintains a persistent authenticated WebSocket connection for a single user
// and workspace, reconnecting with jittered exponential backoff until ctx is cancelled or a
// terminal authentication failure occurs.
func RunUserStream(ctx context.Context, cfg WSConfig, cb WSCallbacks) {
	cfg = cfg.withDefaults()

	wsURL, err := buildWSURL(cfg.APIURL, cfg.WorkspaceID, cfg.Version)
	if err != nil {
		return
	}

	backoff := cfg.InitialBackoff
	for {
		if ctx.Err() != nil {
			return
		}

		terminal, authDuration, _ := runSingleSession(ctx, wsURL, cfg, cb)
		if terminal || ctx.Err() != nil {
			return
		}

		if authDuration > stableConnThreshold {
			backoff = cfg.InitialBackoff
		}

		wait := jitteredBackoff(backoff)
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}

		backoff = min(backoff*2, cfg.MaxBackoff)
	}
}

func runSingleSession(ctx context.Context, wsURL string, cfg WSConfig, cb WSCallbacks) (terminal bool, authDuration time.Duration, err error) {
	dialCtx, dialCancel := context.WithTimeout(ctx, cfg.AuthTimeout)
	conn, resp, err := websocket.Dial(dialCtx, wsURL, nil)
	dialCancel()
	if err != nil {
		if resp != nil && (resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden) {
			cb.notifyAuthFailed(ctx, fmt.Sprintf("websocket upgrade rejected: HTTP %d", resp.StatusCode))
			return true, 0, err
		}
		return false, 0, err
	}
	defer func() { _ = conn.CloseNow() }()
	conn.SetReadLimit(4 << 20)

	terminal, err = authenticateConn(ctx, conn, cfg, cb)
	if err != nil || terminal {
		return terminal, 0, err
	}

	authAt := time.Now()
	cb.notifyStateChange(true)
	defer cb.notifyStateChange(false)
	cb.notifyConnected(ctx)

	err = streamConnLoop(ctx, conn, cfg, cb)
	return false, time.Since(authAt), err
}

func authenticateConn(ctx context.Context, conn *websocket.Conn, cfg WSConfig, cb WSCallbacks) (terminal bool, err error) {
	authCtx, authCancel := context.WithTimeout(ctx, cfg.AuthTimeout)
	defer authCancel()

	authFrame := wsAuthFrame{
		Type:    "auth",
		Payload: wsAuthPayload{Token: cfg.Token},
	}
	if err := wsjson.Write(authCtx, conn, authFrame); err != nil {
		return false, err
	}

	for {
		var env WSEnvelope
		if err := wsjson.Read(authCtx, conn, &env); err != nil {
			return false, err
		}
		if env.Error != "" || env.Type == "auth_error" || env.Type == "error" {
			_ = conn.Close(websocket.StatusNormalClosure, "auth failed")
			cb.notifyAuthFailed(ctx, extractAuthErrorReason(env))
			return true, nil
		}
		if env.Type == "auth_ack" {
			return false, nil
		}
	}
}

func streamConnLoop(ctx context.Context, conn *websocket.Conn, cfg WSConfig, cb WSCallbacks) error {
	connCtx, connCancel := context.WithCancel(ctx)
	defer connCancel()

	pongCh := make(chan struct{}, 1)
	readErrCh := make(chan error, 1)
	readDone := make(chan struct{})

	go func() {
		defer close(readDone)
		for {
			var env WSEnvelope
			if err := wsjson.Read(connCtx, conn, &env); err != nil {
				readErrCh <- err
				return
			}
			if env.Type == "pong" {
				select {
				case pongCh <- struct{}{}:
				default:
				}
				continue
			}
			cb.notifyEvent(connCtx, env)
		}
	}()

	ticker := time.NewTicker(cfg.PingInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			_ = conn.Close(websocket.StatusNormalClosure, "shutdown")
			connCancel()
			<-readDone
			return ctx.Err()
		case err := <-readErrCh:
			connCancel()
			<-readDone
			return err
		case <-ticker.C:
			select {
			case <-pongCh:
			default:
			}

			writeCtx, writeCancel := context.WithTimeout(connCtx, cfg.PongTimeout)
			err := wsjson.Write(writeCtx, conn, wsPingFrame{Type: "ping"})
			writeCancel()
			if err != nil {
				connCancel()
				_ = conn.CloseNow()
				<-readDone
				return err
			}

			pongTimer := time.NewTimer(cfg.PongTimeout)
			select {
			case <-pongCh:
				pongTimer.Stop()
			case <-pongTimer.C:
				connCancel()
				_ = conn.CloseNow()
				<-readDone
				return errors.New("multica: websocket pong timeout")
			case err := <-readErrCh:
				pongTimer.Stop()
				connCancel()
				<-readDone
				return err
			case <-ctx.Done():
				pongTimer.Stop()
				_ = conn.Close(websocket.StatusNormalClosure, "shutdown")
				connCancel()
				<-readDone
				return ctx.Err()
			}
		}
	}
}

func buildWSURL(apiURL, workspaceID, version string) (string, error) {
	raw := strings.TrimSpace(apiURL)
	if raw == "" {
		return "", errors.New("multica: empty API URL")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("multica: invalid API URL: %w", err)
	}
	switch strings.ToLower(u.Scheme) {
	case "https":
		u.Scheme = "wss"
	case "http":
		u.Scheme = "ws"
	case "wss", "ws":
	default:
		return "", fmt.Errorf("multica: unsupported URL scheme %q", u.Scheme)
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/ws"
	q := u.Query()
	q.Set("workspace_id", workspaceID)
	q.Set("client_platform", wsClientPlatform)
	q.Set("client_version", version)
	u.RawQuery = q.Encode()
	return u.String(), nil
}

func extractAuthErrorReason(env WSEnvelope) string {
	if env.Error != "" {
		return env.Error
	}
	if len(env.Payload) > 0 {
		var msg struct {
			Message string `json:"message"`
			Error   string `json:"error"`
		}
		if err := json.Unmarshal(env.Payload, &msg); err == nil {
			if msg.Message != "" {
				return msg.Message
			}
			if msg.Error != "" {
				return msg.Error
			}
		}
	}
	return "auth_error"
}

func jitteredBackoff(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	factor := (1 - backoffJitterFraction) + rand.Float64()*(2*backoffJitterFraction)
	return time.Duration(float64(d) * factor)
}
