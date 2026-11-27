package slackhttp

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"html"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/everlightlabs/multica-slack/internal/crypto"
	"github.com/everlightlabs/multica-slack/internal/store"
	"github.com/slack-go/slack"
)

const (
	slackOAuthAuthorizeURL = "https://slack.com/oauth/v2/authorize"
	slackOAuthUserScopes   = "links:read,links:write"
	oauthStartTokenTTL     = 10 * time.Minute
	oauthStateTTL          = 10 * time.Minute
	oauthStateBytes        = 32
)

func (s *Server) handleOAuthStart(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if s.store == nil {
		http.Error(w, "service unavailable", http.StatusServiceUnavailable)
		return
	}

	token := strings.TrimSpace(r.URL.Query().Get("token"))
	slackUserID, err := crypto.VerifyStartToken(s.cfg.EncryptionKey, token, s.now(), oauthStartTokenTTL)
	if err != nil {
		http.Error(w, "invalid or expired authorization link", http.StatusBadRequest)
		return
	}

	state, err := randomHex(oauthStateBytes)
	if err != nil {
		s.logger.Error("generate oauth state failed", slog.String("error", err.Error()))
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	expiresAt := s.now().UTC().Add(oauthStateTTL)
	if err := s.store.CreateOAuthState(r.Context(), state, slackUserID, expiresAt); err != nil {
		s.logger.Error("persist oauth state failed",
			slog.String("slack_user_id", slackUserID),
			slog.String("error", err.Error()),
		)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	q := url.Values{}
	q.Set("client_id", s.cfg.SlackClientID)
	q.Set("user_scope", slackOAuthUserScopes)
	q.Set("redirect_uri", s.oauthRedirectURI())
	q.Set("state", state)

	http.Redirect(w, r, slackOAuthAuthorizeURL+"?"+q.Encode(), http.StatusFound)
}

func (s *Server) handleOAuthCallback(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if s.store == nil {
		writeOAuthHTML(w, http.StatusServiceUnavailable, "Service Unavailable", "Authorization service is temporarily unavailable. Please try again.")
		return
	}

	if slackErr := strings.TrimSpace(r.URL.Query().Get("error")); slackErr != "" {
		writeOAuthHTML(w, http.StatusBadRequest, "Authorization Cancelled", "Slack authorization was not completed ("+slackErr+"). You can return to Slack and try again from the Multica app Home tab.")
		return
	}

	state := strings.TrimSpace(r.URL.Query().Get("state"))
	code := strings.TrimSpace(r.URL.Query().Get("code"))
	if state == "" || code == "" {
		writeOAuthHTML(w, http.StatusBadRequest, "Invalid Callback", "Missing OAuth state or authorization code.")
		return
	}

	expectedUserID, err := s.store.ConsumeOAuthState(r.Context(), state, s.now().UTC())
	if errors.Is(err, store.ErrNotFound) {
		writeOAuthHTML(w, http.StatusBadRequest, "Link Expired", "This authorization link has expired or was already used. Return to the Multica app Home tab in Slack to start again.")
		return
	}
	if err != nil {
		s.logger.Error("consume oauth state failed", slog.String("error", err.Error()))
		writeOAuthHTML(w, http.StatusInternalServerError, "Authorization Error", "Unable to verify authorization state. Please try again.")
		return
	}

	var opts []slack.OAuthOption
	if s.slackAPIURL != "" {
		opts = append(opts, slack.OAuthOptionAPIURL(s.slackAPIURL))
	}

	oauthResp, err := slack.GetOAuthV2ResponseContext(
		r.Context(),
		s.httpClient,
		s.cfg.SlackClientID,
		s.cfg.SlackClientSecret,
		code,
		s.oauthRedirectURI(),
		opts...,
	)
	if err != nil {
		s.logger.Warn("slack oauth.v2.access failed",
			slog.String("slack_user_id", expectedUserID),
			slog.String("error", err.Error()),
		)
		writeOAuthHTML(w, http.StatusBadGateway, "Authorization Failed", "Slack did not complete the OAuth token exchange. Please try again from Slack.")
		return
	}

	if !s.checkAllowedTeam(w, oauthResp.Team.ID) {
		return
	}

	if strings.TrimSpace(oauthResp.AuthedUser.AccessToken) == "" {
		s.logger.Warn("slack oauth response missing authed_user access_token",
			slog.String("slack_user_id", expectedUserID),
		)
		writeOAuthHTML(w, http.StatusBadGateway, "Authorization Incomplete", "Slack did not return a user link-unfurling token. Please try again.")
		return
	}

	slackUserID := expectedUserID
	if authedID := strings.TrimSpace(oauthResp.AuthedUser.ID); authedID != "" {
		if authedID != expectedUserID {
			s.logger.Warn("slack oauth authed_user mismatch",
				slog.String("expected_user_id", expectedUserID),
				slog.String("authed_user_id", authedID),
			)
			http.Error(w, "oauth user mismatch", http.StatusForbidden)
			return
		}
		slackUserID = authedID
	}

	if err := s.store.SetSlackUserToken(r.Context(), slackUserID, oauthResp.Team.ID, oauthResp.AuthedUser.AccessToken); err != nil {
		s.logger.Error("save slack user token failed",
			slog.String("slack_user_id", slackUserID),
			slog.String("error", err.Error()),
		)
		writeOAuthHTML(w, http.StatusInternalServerError, "Save Failed", "Unable to store your Slack authorization. Please try again.")
		return
	}

	if err := s.publishAppHome(r.Context(), slackUserID); err != nil {
		s.logger.Warn("refresh app home after oauth failed",
			slog.String("slack_user_id", slackUserID),
			slog.String("error", err.Error()),
		)
	}

	writeOAuthHTML(w, http.StatusOK, "DM & Channel Unfurling Enabled", "Multica links will now unfurl in your 1:1 DMs and uninvited channels. You can close this window and return to Slack.")
}

func (s *Server) oauthRedirectURI() string {
	return strings.TrimRight(s.cfg.PublicBaseURL, "/") + "/slack/oauth/callback"
}

func randomHex(nBytes int) (string, error) {
	buf := make([]byte, nBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

func writeOAuthHTML(w http.ResponseWriter, status int, title, body string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>%s — Multica for Slack</title>
<style>
body { font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, sans-serif; margin: 3rem auto; max-width: 32rem; padding: 0 1.25rem; color: #111827; line-height: 1.5; }
h1 { font-size: 1.35rem; margin-bottom: 0.75rem; }
p { color: #374151; }
</style>
</head>
<body>
<h1>%s</h1>
<p>%s</p>
</body>
</html>`, html.EscapeString(title), html.EscapeString(title), html.EscapeString(body))
}
