package slackhttp

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"

	"github.com/everlightlabs/multica-slack/internal/unfurl"
	"github.com/slack-go/slack/slackevents"
)

type eventEnvelope struct {
	Type           string               `json:"type"`
	Challenge      string               `json:"challenge"`
	TeamID         string               `json:"team_id"`
	EventID        string               `json:"event_id"`
	Event          json.RawMessage      `json:"event"`
	Authorizations []eventAuthorization `json:"authorizations"`
}

type eventAuthorization struct {
	TeamID string `json:"team_id"`
	UserID string `json:"user_id"`
}

type innerEventType struct {
	Type string `json:"type"`
}

type linkSharedEventPayload struct {
	Type      string `json:"type"`
	User      string `json:"user"`
	Channel   string `json:"channel"`
	MessageTS string `json:"message_ts"`
	TS        string `json:"ts"`
	UnfurlID  string `json:"unfurl_id"`
	Source    string `json:"source"`
	Links     []struct {
		Domain string `json:"domain"`
		URL    string `json:"url"`
	} `json:"links"`
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "failed to read event payload", http.StatusBadRequest)
		return
	}

	var env eventEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		http.Error(w, "invalid event JSON", http.StatusBadRequest)
		return
	}

	switch env.Type {
	case slackevents.URLVerification:
		if env.TeamID != "" && !s.checkAllowedTeam(w, env.TeamID) {
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"challenge": env.Challenge})
		return

	case slackevents.CallbackEvent:
		teamID := env.TeamID
		if teamID == "" && len(env.Authorizations) > 0 {
			teamID = env.Authorizations[0].TeamID
		}
		if !s.checkAllowedTeam(w, teamID) {
			return
		}

		var inner innerEventType
		if err := json.Unmarshal(env.Event, &inner); err != nil {
			http.Error(w, "invalid inner event JSON", http.StatusBadRequest)
			return
		}

		switch inner.Type {
		case "link_shared":
			var ev linkSharedEventPayload
			if err := json.Unmarshal(env.Event, &ev); err != nil {
				http.Error(w, "invalid link_shared event", http.StatusBadRequest)
				return
			}
			authUserID := ""
			if len(env.Authorizations) > 0 {
				authUserID = env.Authorizations[0].UserID
			}
			links := make([]string, 0, len(ev.Links))
			for _, l := range ev.Links {
				if l.URL != "" {
					links = append(links, l.URL)
				}
			}
			input := unfurl.LinkSharedInput{
				Channel:           ev.Channel,
				MessageTS:         firstNonEmpty(ev.MessageTS, ev.TS),
				UnfurlID:          ev.UnfurlID,
				Source:            ev.Source,
				PosterUserID:      ev.User,
				AuthorizingUserID: authUserID,
				Links:             links,
			}
			if s.unfurl != nil {
				s.runAsync("link_shared", func(ctx context.Context) {
					if err := s.unfurl.HandleLinkShared(ctx, input); err != nil {
						s.logger.Warn("handle link_shared failed",
							slog.String("channel", input.Channel),
							slog.String("poster_user_id", input.PosterUserID),
							slog.String("error", err.Error()),
						)
					}
				})
			}

		case "app_home_opened":
			var ev slackevents.AppHomeOpenedEvent
			if err := json.Unmarshal(env.Event, &ev); err != nil {
				http.Error(w, "invalid app_home_opened event", http.StatusBadRequest)
				return
			}
			if ev.User != "" && (ev.Tab == "" || ev.Tab == "home") {
				userID := ev.User
				s.runAsync("app_home_opened", func(ctx context.Context) {
					if err := s.publishAppHome(ctx, userID); err != nil {
						s.logger.Warn("publish app home failed",
							slog.String("slack_user_id", userID),
							slog.String("error", err.Error()),
						)
					}
				})
			}
		}

		w.WriteHeader(http.StatusOK)
		return

	default:
		w.WriteHeader(http.StatusOK)
	}
}
