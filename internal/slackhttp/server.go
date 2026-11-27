// Package slackhttp implements the Slack Events API, App Home, Interactivity,
// Slash Commands, per-user OAuth, and health/readiness HTTP endpoints.
package slackhttp

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/everlightlabs/multica-slack/internal/config"
	"github.com/everlightlabs/multica-slack/internal/multica"
	"github.com/everlightlabs/multica-slack/internal/notify"
	"github.com/everlightlabs/multica-slack/internal/store"
	"github.com/everlightlabs/multica-slack/internal/unfurl"
	"github.com/slack-go/slack"
)

const (
	asyncHandlerTimeout = 15 * time.Second
	defaultHTTPTimeout  = 15 * time.Second
)

// Deps bundles dependencies injected into the Slack HTTP server.
type Deps struct {
	Config      config.Config
	Store       *store.Store
	Multica     *multica.Client
	Cache       *multica.MetadataCache
	Unfurl      *unfurl.Service
	Notify      *notify.Manager
	Slack       *slack.Client
	SlackAPIURL string
	HTTPClient  *http.Client
	Logger      *slog.Logger
	Now         func() time.Time
}

// Server serves Slack webhook/OAuth routes and health endpoints, and tracks
// in-flight async handlers for clean shutdown draining.
type Server struct {
	cfg         config.Config
	store       *store.Store
	mc          *multica.Client
	cache       *multica.MetadataCache
	unfurl      *unfurl.Service
	notify      *notify.Manager
	slack       *slack.Client
	slackAPIURL string
	httpClient  *http.Client
	logger      *slog.Logger
	now         func() time.Time

	mux          *http.ServeMux
	bgCtx        context.Context
	bgCancel     context.CancelFunc
	bgMu         sync.Mutex
	shuttingDown bool
	bgWG         sync.WaitGroup
}

var _ http.Handler = (*Server)(nil)

// NewServer constructs a configured Slack HTTP server implementing http.Handler.
func NewServer(deps Deps) *Server {
	logger := deps.Logger
	if logger == nil {
		logger = slog.Default()
	}
	nowFn := deps.Now
	if nowFn == nil {
		nowFn = time.Now
	}
	httpClient := deps.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: defaultHTTPTimeout}
	}
	slackClient := deps.Slack
	if slackClient == nil && deps.Config.SlackBotToken != "" {
		var opts []slack.Option
		if deps.SlackAPIURL != "" {
			opts = append(opts, slack.OptionAPIURL(deps.SlackAPIURL))
		}
		slackClient = slack.New(deps.Config.SlackBotToken, opts...)
	}

	bgCtx, bgCancel := context.WithCancel(context.Background())

	s := &Server{
		cfg:         deps.Config,
		store:       deps.Store,
		mc:          deps.Multica,
		cache:       deps.Cache,
		unfurl:      deps.Unfurl,
		notify:      deps.Notify,
		slack:       slackClient,
		slackAPIURL: deps.SlackAPIURL,
		httpClient:  httpClient,
		logger:      logger,
		now:         nowFn,
		mux:         http.NewServeMux(),
		bgCtx:       bgCtx,
		bgCancel:    bgCancel,
	}

	s.mux.HandleFunc("POST /slack/events", s.withSlackSignature(s.handleEvents))
	s.mux.HandleFunc("POST /slack/interactions", s.withSlackSignature(s.handleInteractions))
	s.mux.HandleFunc("POST /slack/commands", s.withSlackSignature(s.handleCommands))
	s.mux.HandleFunc("GET /slack/oauth/start", s.handleOAuthStart)
	s.mux.HandleFunc("GET /slack/oauth/callback", s.handleOAuthCallback)
	s.mux.HandleFunc("GET /healthz", s.handleHealthz)
	s.mux.HandleFunc("GET /readyz", s.handleReadyz)

	return s
}

// ServeHTTP dispatches HTTP requests through the registered routes.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

// Shutdown prevents new async Slack handlers from starting, cancels the
// background context, and waits for in-flight async handlers to complete until
// ctx expires.
func (s *Server) Shutdown(ctx context.Context) error {
	s.bgMu.Lock()
	s.shuttingDown = true
	s.bgMu.Unlock()

	s.bgCancel()

	done := make(chan struct{})
	go func() {
		s.bgWG.Wait()
		close(done)
	}()

	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Server) runAsync(name string, fn func(ctx context.Context)) {
	s.bgMu.Lock()
	if s.shuttingDown {
		s.bgMu.Unlock()
		return
	}
	s.bgWG.Add(1)
	s.bgMu.Unlock()

	go func() {
		defer s.bgWG.Done()
		defer func() {
			if rec := recover(); rec != nil {
				s.logger.Error("panic in async slack handler",
					slog.String("handler", name),
					slog.Any("panic", rec),
				)
			}
		}()

		ctx, cancel := context.WithTimeout(s.bgCtx, asyncHandlerTimeout)
		defer cancel()
		fn(ctx)
	}()
}

func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	if s.store == nil || s.store.Ping(r.Context()) != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "not_ready"})
		return
	}

	linkedUsers, authFailed, err := s.store.Counts(r.Context())
	if err != nil {
		s.logger.Warn("readyz counts query failed", slog.String("error", err.Error()))
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "not_ready"})
		return
	}

	wsConnected := 0
	if s.notify != nil {
		wsConnected = s.notify.ConnectedCount()
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"status":         "ready",
		"linked_users":   linkedUsers,
		"ws_connected":   wsConnected,
		"ws_auth_failed": authFailed,
	})
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if trimmed := strings.TrimSpace(v); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

func escapeSlackMrkdwn(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}
