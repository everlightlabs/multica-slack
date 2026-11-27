package unfurl

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/everlightlabs/multica-slack/internal/multica"
	"github.com/everlightlabs/multica-slack/internal/store"
	"github.com/slack-go/slack"
)

const (
	defaultDedupTTL = 30 * time.Second
	maxUnfurlLinks  = 5
)

// SlackUnfurler dispatches chat.unfurl payloads using a bot or user token.
type SlackUnfurler interface {
	UnfurlByID(ctx context.Context, token, unfurlID, source string, unfurls map[string]slack.Attachment) error
	UnfurlByTS(ctx context.Context, token, channel, ts string, unfurls map[string]slack.Attachment) error
}

type slackClientUnfurler struct {
	opts []slack.Option
}

// NewSlackClient wraps github.com/slack-go/slack as a SlackUnfurler.
func NewSlackClient(opts ...slack.Option) SlackUnfurler {
	copied := make([]slack.Option, len(opts))
	copy(copied, opts)
	return &slackClientUnfurler{opts: copied}
}

func (s *slackClientUnfurler) UnfurlByID(ctx context.Context, token, unfurlID, source string, unfurls map[string]slack.Attachment) error {
	client := slack.New(token, s.opts...)
	_, _, _, err := client.UnfurlMessageByIDContext(ctx, unfurlID, source, unfurls)
	return err
}

func (s *slackClientUnfurler) UnfurlByTS(ctx context.Context, token, channel, ts string, unfurls map[string]slack.Attachment) error {
	client := slack.New(token, s.opts...)
	_, _, _, err := client.UnfurlMessageContext(ctx, channel, ts, unfurls)
	return err
}

// ServiceConfig configures link matching, credentials, and deduplication.
type ServiceConfig struct {
	AppBaseURL  string
	BotToken    string
	FallbackPAT string
	DedupTTL    time.Duration
	Now         func() time.Time
}

// LinkSharedInput captures the Slack link_shared event fields needed for unfurling.
type LinkSharedInput struct {
	Channel           string
	MessageTS         string
	UnfurlID          string
	Source            string
	PosterUserID      string
	AuthorizingUserID string
	Links             []string
}

type dedupKey struct {
	channel   string
	messageTS string
	rawURL    string
}

// Service matches Multica links, fetches authorized metadata, and sends Slack unfurls.
type Service struct {
	cfg    ServiceConfig
	st     *store.Store
	mc     *multica.Client
	cache  *multica.MetadataCache
	slack  SlackUnfurler
	logger *slog.Logger
	nowFn  func() time.Time

	mu    sync.Mutex
	dedup map[dedupKey]time.Time
}

// NewService constructs a Service for handling Slack link_shared events.
func NewService(
	cfg ServiceConfig,
	st *store.Store,
	mc *multica.Client,
	cache *multica.MetadataCache,
	slackClient SlackUnfurler,
	logger *slog.Logger,
) *Service {
	if strings.TrimSpace(cfg.AppBaseURL) == "" {
		cfg.AppBaseURL = defaultAppBaseURL
	}
	if cfg.DedupTTL <= 0 {
		cfg.DedupTTL = defaultDedupTTL
	}
	nowFn := cfg.Now
	if nowFn == nil {
		nowFn = time.Now
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Service{
		cfg:    cfg,
		st:     st,
		mc:     mc,
		cache:  cache,
		slack:  slackClient,
		logger: logger,
		nowFn:  nowFn,
		dedup:  make(map[dedupKey]time.Time),
	}
}

// HandleLinkShared matches up to 5 Multica URLs, deduplicates within 30s,
// fetches Multica data using strictly the poster's PAT (or FallbackPAT), and calls chat.unfurl.
func (s *Service) HandleLinkShared(ctx context.Context, in LinkSharedInput) error {
	if len(in.Links) == 0 {
		return nil
	}

	matches := s.collectMatches(in.Links)
	if len(matches) == 0 {
		return nil
	}

	poster, err := s.lookupUser(ctx, in.PosterUserID)
	if err != nil {
		return err
	}

	pat, cacheUserKey := s.resolveMulticaPAT(poster, in.PosterUserID)
	if pat == "" {
		s.logger.Debug("skipping unfurl for unlinked poster without fallback PAT",
			slog.String("channel", in.Channel),
			slog.String("poster_user_id", in.PosterUserID),
		)
		return nil
	}

	matches, claimedKeys, claimedExpiresAt := s.claimDedupMatches(in, matches)
	if len(matches) == 0 {
		return nil
	}

	unfurls := s.fetchAllCards(ctx, cacheUserKey, pat, matches)
	if len(unfurls) == 0 {
		s.releaseDedupClaim(claimedKeys, claimedExpiresAt)
		return nil
	}

	if err := s.dispatchUnfurl(ctx, in, poster, unfurls); err != nil {
		s.releaseDedupClaim(claimedKeys, claimedExpiresAt)
		return err
	}
	return nil
}

func (s *Service) collectMatches(links []string) []Match {
	seen := make(map[string]struct{}, len(links))
	matches := make([]Match, 0, min(len(links), maxUnfurlLinks))
	for _, rawURL := range links {
		if _, dup := seen[rawURL]; dup {
			continue
		}
		m, ok := MatchURL(s.cfg.AppBaseURL, rawURL)
		if !ok {
			continue
		}
		seen[rawURL] = struct{}{}
		matches = append(matches, m)
		if len(matches) >= maxUnfurlLinks {
			break
		}
	}
	return matches
}

func (s *Service) lookupUser(ctx context.Context, slackUserID string) (*store.User, error) {
	if slackUserID == "" || s.st == nil {
		return nil, nil
	}
	u, err := s.st.GetUser(ctx, slackUserID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("unfurl: lookup user %s: %w", slackUserID, err)
	}
	return u, nil
}

// resolveMulticaPAT strictly selects the poster's own valid PAT, falling back only to
// the operator-configured FallbackPAT. Never uses another Slack user's PAT.
func (s *Service) resolveMulticaPAT(poster *store.User, posterUserID string) (pat, cacheUserKey string) {
	if poster != nil && poster.AuthStatus == store.AuthOK && poster.MulticaPAT != "" {
		return poster.MulticaPAT, posterUserID
	}
	if s.cfg.FallbackPAT != "" {
		return s.cfg.FallbackPAT, "__fallback__"
	}
	return "", ""
}

func (s *Service) claimDedupMatches(in LinkSharedInput, matches []Match) ([]Match, []dedupKey, time.Time) {
	tsKey := in.MessageTS
	if tsKey == "" {
		tsKey = in.UnfurlID
	}
	now := s.nowFn()
	expiresAt := now.Add(s.cfg.DedupTTL)

	s.mu.Lock()
	defer s.mu.Unlock()

	for k, exp := range s.dedup {
		if !now.Before(exp) {
			delete(s.dedup, k)
		}
	}

	fresh := make([]Match, 0, len(matches))
	claimed := make([]dedupKey, 0, len(matches))
	for _, m := range matches {
		k := dedupKey{
			channel:   in.Channel,
			messageTS: tsKey,
			rawURL:    m.RawURL,
		}
		if exp, exists := s.dedup[k]; exists && now.Before(exp) {
			continue
		}
		s.dedup[k] = expiresAt
		fresh = append(fresh, m)
		claimed = append(claimed, k)
	}
	return fresh, claimed, expiresAt
}

func (s *Service) releaseDedupClaim(keys []dedupKey, claimedExpiresAt time.Time) {
	if len(keys) == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, k := range keys {
		if exp, exists := s.dedup[k]; exists && exp.Equal(claimedExpiresAt) {
			delete(s.dedup, k)
		}
	}
}

func (s *Service) fetchAllCards(ctx context.Context, cacheUserKey, pat string, matches []Match) map[string]slack.Attachment {
	now := s.nowFn().UTC()
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		unfurls = make(map[string]slack.Attachment, len(matches))
	)

	for _, m := range matches {
		wg.Add(1)
		go func(match Match) {
			defer wg.Done()
			att, ok := s.fetchSingleCard(ctx, cacheUserKey, pat, match, now)
			if !ok {
				return
			}
			mu.Lock()
			unfurls[match.RawURL] = att
			mu.Unlock()
		}(m)
	}
	wg.Wait()
	return unfurls
}

func (s *Service) fetchSingleCard(ctx context.Context, cacheUserKey, pat string, match Match, now time.Time) (slack.Attachment, bool) {
	ws, err := s.cache.ResolveWorkspaceBySlug(ctx, cacheUserKey, pat, match.WorkspaceSlug)
	if err != nil {
		s.logger.Warn("unfurl workspace resolution failed",
			slog.String("workspace_slug", match.WorkspaceSlug),
			slog.String("kind", string(match.Kind)),
			slog.String("error", err.Error()),
		)
		return slack.Attachment{}, false
	}

	switch match.Kind {
	case KindIssue:
		issue, err := s.mc.GetIssue(ctx, pat, ws.ID, match.ResourceID)
		if err != nil {
			s.logger.Warn("unfurl issue fetch failed",
				slog.String("workspace_id", ws.ID),
				slog.String("resource_id", match.ResourceID),
				slog.String("error", err.Error()),
			)
			return slack.Attachment{}, false
		}
		meta, metaErr := s.cache.WorkspaceMetadata(ctx, pat, ws.ID)
		if metaErr != nil {
			s.logger.Warn("unfurl workspace metadata fetch failed",
				slog.String("workspace_id", ws.ID),
				slog.String("error", metaErr.Error()),
			)
		}
		return FormatIssueCard(match.RawURL, issue, meta, now), true

	case KindProject:
		project, err := s.mc.GetProject(ctx, pat, ws.ID, match.ResourceID)
		if err != nil {
			s.logger.Warn("unfurl project fetch failed",
				slog.String("workspace_id", ws.ID),
				slog.String("resource_id", match.ResourceID),
				slog.String("error", err.Error()),
			)
			return slack.Attachment{}, false
		}
		var progressPtr *multica.ProjectProgress
		progress, progErr := s.mc.GetProjectProgress(ctx, pat, ws.ID, match.ResourceID)
		if progErr != nil {
			s.logger.Warn("unfurl project progress fetch failed",
				slog.String("workspace_id", ws.ID),
				slog.String("resource_id", match.ResourceID),
				slog.String("error", progErr.Error()),
			)
		} else {
			progressPtr = &progress
		}
		return FormatProjectCard(match.RawURL, project, progressPtr, now), true

	case KindDoc:
		doc, err := s.mc.GetDoc(ctx, pat, ws.ID, match.ResourceID)
		if err != nil {
			s.logger.Warn("unfurl doc fetch failed",
				slog.String("workspace_id", ws.ID),
				slog.String("resource_id", match.ResourceID),
				slog.String("error", err.Error()),
			)
			return slack.Attachment{}, false
		}
		return FormatDocCard(match.RawURL, doc, now), true

	default:
		return slack.Attachment{}, false
	}
}

func (s *Service) dispatchUnfurl(ctx context.Context, in LinkSharedInput, poster *store.User, unfurls map[string]slack.Attachment) error {
	if s.slack == nil {
		return errors.New("unfurl: slack client is nil")
	}

	send := func(token string) error {
		if in.UnfurlID != "" && in.Source != "" {
			return s.slack.UnfurlByID(ctx, token, in.UnfurlID, in.Source, unfurls)
		}
		return s.slack.UnfurlByTS(ctx, token, in.Channel, in.MessageTS, unfurls)
	}

	var lastErr error
	if s.cfg.BotToken != "" {
		err := send(s.cfg.BotToken)
		if err == nil {
			return nil
		}
		if !isRetryableSlackUnfurlError(err) {
			return fmt.Errorf("unfurl: slack chat.unfurl (bot): %w", err)
		}
		lastErr = err
	}

	tokens, err := s.collectUserTransportTokens(ctx, in, poster)
	if err != nil {
		return err
	}
	for _, tok := range tokens {
		err := send(tok)
		if err == nil {
			return nil
		}
		if !isRetryableSlackUnfurlError(err) {
			return fmt.Errorf("unfurl: slack chat.unfurl (user): %w", err)
		}
		lastErr = err
	}

	if lastErr != nil {
		return fmt.Errorf("unfurl: slack chat.unfurl failed: %w", lastErr)
	}
	return errors.New("unfurl: no slack token available for chat.unfurl")
}

// collectUserTransportTokens collects Slack xoxp- tokens solely for chat.unfurl transport
// (poster first, then authorizing user).
func (s *Service) collectUserTransportTokens(ctx context.Context, in LinkSharedInput, poster *store.User) ([]string, error) {
	var tokens []string
	seen := make(map[string]struct{}, 2)

	if poster != nil && poster.SlackUserToken != "" {
		seen[poster.SlackUserToken] = struct{}{}
		tokens = append(tokens, poster.SlackUserToken)
	}

	if in.AuthorizingUserID != "" && in.AuthorizingUserID != in.PosterUserID {
		authUser, err := s.lookupUser(ctx, in.AuthorizingUserID)
		if err != nil {
			return nil, err
		}
		if authUser != nil && authUser.SlackUserToken != "" {
			if _, ok := seen[authUser.SlackUserToken]; !ok {
				tokens = append(tokens, authUser.SlackUserToken)
			}
		}
	}
	return tokens, nil
}

func isRetryableSlackUnfurlError(err error) bool {
	if err == nil {
		return false
	}
	var slackErr slack.SlackErrorResponse
	if errors.As(err, &slackErr) {
		switch slackErr.Err {
		case "channel_not_found", "not_in_channel", "cannot_unfurl_url":
			return true
		}
	}
	var slackErrPtr *slack.SlackErrorResponse
	if errors.As(err, &slackErrPtr) && slackErrPtr != nil {
		switch slackErrPtr.Err {
		case "channel_not_found", "not_in_channel", "cannot_unfurl_url":
			return true
		}
	}
	msg := err.Error()
	return strings.Contains(msg, "channel_not_found") ||
		strings.Contains(msg, "not_in_channel") ||
		strings.Contains(msg, "cannot_unfurl_url")
}
