package slackhttp

import (
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/everlightlabs/multica-slack/internal/prefs"
	"github.com/everlightlabs/multica-slack/internal/store"
	"github.com/slack-go/slack"
)

type slashCommandResponse struct {
	ResponseType string `json:"response_type"`
	Text         string `json:"text"`
}

func (s *Server) handleCommands(w http.ResponseWriter, r *http.Request) {
	cmd, err := slack.SlashCommandParse(r)
	if err != nil {
		http.Error(w, "invalid slash command payload", http.StatusBadRequest)
		return
	}
	if !s.checkAllowedTeam(w, cmd.TeamID) {
		return
	}

	sub := strings.ToLower(strings.TrimSpace(cmd.Text))
	switch sub {
	case "", "connect":
		if err := s.openPATModal(r.Context(), cmd.TriggerID, cmd.TeamID); err != nil {
			s.logger.Warn("open PAT modal from slash command failed",
				slog.String("slack_user_id", cmd.UserID),
				slog.String("error", err.Error()),
			)
			writeJSON(w, http.StatusOK, slashCommandResponse{
				ResponseType: "ephemeral",
				Text:         "Unable to open the Multica connection modal right now. Please open the app Home tab and try again.",
			})
			return
		}
		w.WriteHeader(http.StatusOK)

	case "status":
		user, err := s.loadUserOrNil(r.Context(), cmd.UserID)
		if err != nil {
			s.logger.Warn("load user for /multica status failed",
				slog.String("slack_user_id", cmd.UserID),
				slog.String("error", err.Error()),
			)
			writeJSON(w, http.StatusOK, slashCommandResponse{
				ResponseType: "ephemeral",
				Text:         "Unable to load your Multica status right now.",
			})
			return
		}
		writeJSON(w, http.StatusOK, slashCommandResponse{
			ResponseType: "ephemeral",
			Text:         s.formatStatusSummary(user),
		})

	default:
		writeJSON(w, http.StatusOK, slashCommandResponse{
			ResponseType: "ephemeral",
			Text:         "Usage: `/multica` or `/multica connect` to connect your Multica PAT, or `/multica status` to view your connection and notification status.",
		})
	}
}

func (s *Server) formatStatusSummary(user *store.User) string {
	var b strings.Builder
	b.WriteString("*Multica for Slack — Status*\n")

	if user == nil || user.MulticaPAT == "" || user.AuthStatus == store.AuthDisconnected {
		b.WriteString("• *Account:* Not connected (run `/multica connect` or open the Home tab)\n")
	} else if user.AuthStatus == store.AuthFailed {
		b.WriteString("• *Account:* PAT invalid — please reconnect via `/multica connect`\n")
	} else {
		name := firstNonEmpty(user.MulticaUserName, user.MulticaUserID, "Multica user")
		wsSlug := firstNonEmpty(user.MulticaWorkspaceSlug, user.MulticaWorkspaceID, "unknown")
		fmt.Fprintf(&b, "• *Account:* Connected as %s\n", escapeSlackMrkdwn(name))
		fmt.Fprintf(&b, "• *Workspace:* %s\n", escapeSlackMrkdwn(wsSlug))
		fmt.Fprintf(&b, "• *WebSocket:* %s\n", s.userWSState(user))
	}

	dmState := "Not enabled"
	if user != nil && user.SlackUserToken != "" {
		dmState = "Enabled"
	}
	fmt.Fprintf(&b, "• *DM & Uninvited-Channel Unfurling:* %s\n", dmState)

	if user != nil && user.MulticaPAT != "" && user.AuthStatus != store.AuthDisconnected {
		userPrefs := prefs.Parse(user.NotificationPrefsJSON)
		enabledLabels := make([]string, 0, len(prefs.AllGroups))
		for _, g := range prefs.AllGroups {
			if userPrefs[g.Key] {
				enabledLabels = append(enabledLabels, g.Label)
			}
		}
		categories := "None"
		if len(enabledLabels) > 0 {
			categories = strings.Join(enabledLabels, ", ")
		}
		fmt.Fprintf(&b, "• *Enabled Notifications:* %s", categories)
	}

	return strings.TrimRight(b.String(), "\n")
}
