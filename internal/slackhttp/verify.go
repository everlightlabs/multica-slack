package slackhttp

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/slack-go/slack"
)

const maxSlackRequestBodyBytes = 1 << 20 // 1 MiB

// withSlackSignature validates Slack's HMAC-SHA256 request signature and replay window,
// enforcing a 1 MiB request body cap and restoring r.Body for downstream handlers.
func (s *Server) withSlackSignature(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if strings.TrimSpace(r.Header.Get("X-Slack-Signature")) == "" ||
			strings.TrimSpace(r.Header.Get("X-Slack-Request-Timestamp")) == "" {
			http.Error(w, "missing slack signature headers", http.StatusBadRequest)
			return
		}

		sv, err := slack.NewSecretsVerifier(r.Header, s.cfg.SlackSigningSecret)
		if err != nil {
			if errors.Is(err, slack.ErrExpiredTimestamp) {
				http.Error(w, "expired slack request timestamp", http.StatusUnauthorized)
				return
			}
			http.Error(w, "malformed slack signature headers", http.StatusBadRequest)
			return
		}

		r.Body = http.MaxBytesReader(w, r.Body, maxSlackRequestBodyBytes)
		body, err := io.ReadAll(r.Body)
		if err != nil {
			var maxErr *http.MaxBytesError
			if errors.As(err, &maxErr) {
				http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
				return
			}
			http.Error(w, "failed to read request body", http.StatusBadRequest)
			return
		}

		if _, err := sv.Write(body); err != nil {
			http.Error(w, "failed to verify signature", http.StatusInternalServerError)
			return
		}
		if err := sv.Ensure(); err != nil {
			http.Error(w, "invalid slack signature", http.StatusUnauthorized)
			return
		}

		r.Body = io.NopCloser(bytes.NewReader(body))
		r.ContentLength = int64(len(body))
		next(w, r)
	}
}

// checkAllowedTeam enforces SLACK_ALLOWED_TEAM_ID when configured, writing 403 on mismatch.
func (s *Server) checkAllowedTeam(w http.ResponseWriter, teamID string) bool {
	allowed := strings.TrimSpace(s.cfg.SlackAllowedTeamID)
	if allowed == "" {
		return true
	}
	if strings.TrimSpace(teamID) != allowed {
		http.Error(w, "forbidden slack workspace", http.StatusForbidden)
		return false
	}
	return true
}
