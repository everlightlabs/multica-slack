package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/everlightlabs/multica-slack/internal/config"
	"github.com/everlightlabs/multica-slack/internal/crypto"
	"github.com/everlightlabs/multica-slack/internal/multica"
	"github.com/everlightlabs/multica-slack/internal/notify"
	"github.com/everlightlabs/multica-slack/internal/slackhttp"
	"github.com/everlightlabs/multica-slack/internal/store"
	"github.com/everlightlabs/multica-slack/internal/unfurl"
	"github.com/slack-go/slack"
)

const (
	dbFileName               = "multica-slack.db"
	defaultHTTPClientTimeout = 15 * time.Second
	defaultMetadataCacheTTL  = 60 * time.Second
	defaultPruneInterval     = 24 * time.Hour
	deliveryRetention        = 30 * 24 * time.Hour
	shutdownTimeout          = 10 * time.Second
)

// version is set at build time via -ldflags "-X main.version=...".
var version = "dev"

type runOptions struct {
	slackAPIURL   string
	pruneInterval time.Duration
	httpClient    *http.Client
	onReady       func(addr string, a *app)
}

type app struct {
	store       *store.Store
	notifyMgr   *notify.Manager
	slackServer *slackhttp.Server
	httpServer  *http.Server
}

type slackDMClient struct {
	client *slack.Client
}

func (c *slackDMClient) OpenDM(ctx context.Context, slackUserID string) (string, error) {
	ch, _, _, err := c.client.OpenConversationContext(ctx, &slack.OpenConversationParameters{
		Users: []string{slackUserID},
	})
	if err != nil {
		return "", err
	}
	if ch == nil || ch.ID == "" {
		return "", errors.New("slack: conversations.open returned empty channel id")
	}
	return ch.ID, nil
}

func (c *slackDMClient) PostMessage(ctx context.Context, channelID, fallbackText string, blocks []slack.Block) (string, error) {
	_, ts, err := c.client.PostMessageContext(
		ctx,
		channelID,
		slack.MsgOptionText(fallbackText, false),
		slack.MsgOptionBlocks(blocks...),
	)
	return ts, err
}

func (c *slackDMClient) UpdateMessage(ctx context.Context, channelID, ts, fallbackText string, blocks []slack.Block) error {
	_, _, _, err := c.client.UpdateMessageContext(
		ctx,
		channelID,
		ts,
		slack.MsgOptionText(fallbackText, false),
		slack.MsgOptionBlocks(blocks...),
	)
	return err
}

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))

	cfg, err := config.Load(nil)
	if err != nil {
		logger.Error("failed to load configuration", slog.String("error", err.Error()))
		os.Exit(1)
	}
	if cfg.Version == "dev" && version != "" && version != "dev" {
		cfg.Version = version
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, cfg, logger); err != nil {
		logger.Error("multica-slack exited with error", slog.String("error", err.Error()))
		os.Exit(1)
	}
}

func run(ctx context.Context, cfg config.Config, logger *slog.Logger) error {
	return runWithOptions(ctx, cfg, logger, runOptions{})
}

func runWithOptions(ctx context.Context, cfg config.Config, logger *slog.Logger, opts runOptions) error {
	if logger == nil {
		logger = slog.Default()
	}

	box, err := crypto.NewBox(cfg.EncryptionKey)
	if err != nil {
		return fmt.Errorf("init crypto box: %w", err)
	}

	dbPath := filepath.Join(cfg.DataDir, dbFileName)
	st, err := store.Open(dbPath, box)
	if err != nil {
		return fmt.Errorf("open sqlite store: %w", err)
	}

	httpClient := opts.httpClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: defaultHTTPClientTimeout}
	}

	mc := multica.NewClient(cfg.MulticaAPIURL, httpClient)
	cache := multica.NewMetadataCache(mc, defaultMetadataCacheTTL)
	cache.SetLogger(logger)

	slackOpts := []slack.Option{slack.OptionHTTPClient(httpClient)}
	if opts.slackAPIURL != "" {
		slackOpts = append(slackOpts, slack.OptionAPIURL(opts.slackAPIURL))
	}
	slackClient := slack.New(cfg.SlackBotToken, slackOpts...)

	unfurlSvc := unfurl.NewService(
		unfurl.ServiceConfig{
			AppBaseURL:  cfg.MulticaAppURL,
			BotToken:    cfg.SlackBotToken,
			FallbackPAT: cfg.MulticaFallbackPAT,
		},
		st,
		mc,
		cache,
		unfurl.NewSlackClient(slackOpts...),
		logger,
	)

	notifyMgr := notify.NewManager(
		notify.ManagerConfig{
			MulticaAPIURL: cfg.MulticaAPIURL,
			MulticaAppURL: cfg.MulticaAppURL,
			Version:       cfg.Version,
		},
		st,
		mc,
		cache,
		&slackDMClient{client: slackClient},
		logger,
	)

	slackServer := slackhttp.NewServer(slackhttp.Deps{
		Config:      cfg,
		Store:       st,
		Multica:     mc,
		Cache:       cache,
		Unfurl:      unfurlSvc,
		Notify:      notifyMgr,
		Slack:       slackClient,
		SlackAPIURL: opts.slackAPIURL,
		HTTPClient:  httpClient,
		Logger:      logger,
	})
	notifyMgr.SetOnStateChange(func(slackUserID string, _ bool) {
		slackServer.RefreshAppHome(slackUserID)
	})

	httpServer := &http.Server{
		Handler:           slackServer,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", cfg.Port))
	if err != nil {
		_ = st.Close()
		return fmt.Errorf("listen on port %d: %w", cfg.Port, err)
	}

	notifyCtx, notifyCancel := context.WithCancel(context.Background())
	defer notifyCancel()

	if err := notifyMgr.Start(notifyCtx); err != nil {
		_ = ln.Close()
		_ = st.Close()
		return fmt.Errorf("start notification manager: %w", err)
	}

	pruneCtx, pruneCancel := context.WithCancel(context.Background())
	defer pruneCancel()

	pruneInterval := opts.pruneInterval
	if pruneInterval <= 0 {
		pruneInterval = defaultPruneInterval
	}
	var pruneWG sync.WaitGroup
	pruneWG.Add(1)
	go func() {
		defer pruneWG.Done()
		runPruneLoop(pruneCtx, st, pruneInterval, logger)
	}()

	a := &app{
		store:       st,
		notifyMgr:   notifyMgr,
		slackServer: slackServer,
		httpServer:  httpServer,
	}

	serveErrCh := make(chan error, 1)
	go func() {
		logger.Info("multica-slack listening",
			slog.String("addr", ln.Addr().String()),
			slog.String("version", cfg.Version),
		)
		serveErrCh <- httpServer.Serve(ln)
	}()

	if opts.onReady != nil {
		opts.onReady(ln.Addr().String(), a)
	}

	var serveErr error
	select {
	case <-ctx.Done():
		logger.Info("shutdown signal received")
	case err := <-serveErrCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr = fmt.Errorf("http server: %w", err)
		}
	}

	pruneCancel()

	var errs []error
	if serveErr != nil {
		errs = append(errs, serveErr)
	}
	httpShutdownCtx, httpShutdownCancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer httpShutdownCancel()
	if err := httpServer.Shutdown(httpShutdownCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		errs = append(errs, fmt.Errorf("shutdown http server: %w", err))
	}

	slackShutdownCtx, slackShutdownCancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer slackShutdownCancel()
	if err := slackServer.Shutdown(slackShutdownCtx); err != nil {
		errs = append(errs, fmt.Errorf("drain async slack handlers: %w", err))
	}

	notifyMgr.Stop()
	notifyCancel()
	pruneWG.Wait()
	if err := st.Close(); err != nil {
		errs = append(errs, fmt.Errorf("close store: %w", err))
	}

	return errors.Join(errs...)
}

func runPruneLoop(ctx context.Context, st *store.Store, interval time.Duration, logger *slog.Logger) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			now := time.Now().UTC()
			expiredStates, err := st.PruneExpiredOAuthStates(ctx, now)
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				logger.Warn("prune expired oauth states failed", slog.String("error", err.Error()))
			} else if expiredStates > 0 {
				logger.Info("pruned expired oauth states", slog.Int64("deleted", expiredStates))
			}

			cutoff := now.Add(-deliveryRetention)
			deleted, err := st.PruneDeliveriesOlderThan(ctx, cutoff)
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				logger.Warn("prune old delivery records failed", slog.String("error", err.Error()))
			} else if deleted > 0 {
				logger.Info("pruned old delivery records", slog.Int64("deleted", deleted))
			}
		}
	}
}
