package slackhttp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/everlightlabs/multica-slack/internal/multica"
	"github.com/everlightlabs/multica-slack/internal/notify"
	"github.com/everlightlabs/multica-slack/internal/prefs"
	"github.com/everlightlabs/multica-slack/internal/store"
	"github.com/slack-go/slack"
)

func (s *Server) handleInteractions(w http.ResponseWriter, r *http.Request) {
	cb, err := slack.InteractionCallbackParse(r)
	if err != nil {
		http.Error(w, "invalid interaction payload", http.StatusBadRequest)
		return
	}

	teamID := firstNonEmpty(cb.Team.ID, cb.User.TeamID, cb.View.TeamID)
	if !s.checkAllowedTeam(w, teamID) {
		return
	}

	switch cb.Type {
	case slack.InteractionTypeBlockActions:
		s.handleBlockActions(r.Context(), w, cb, teamID)
	case slack.InteractionTypeViewSubmission:
		s.handleViewSubmission(r.Context(), w, cb, teamID)
	default:
		w.WriteHeader(http.StatusOK)
	}
}

func (s *Server) handleBlockActions(ctx context.Context, w http.ResponseWriter, cb slack.InteractionCallback, teamID string) {
	for _, action := range cb.ActionCallback.BlockActions {
		if action == nil {
			continue
		}
		switch action.ActionID {
		case ActionOpenPATModal:
			if err := s.openPATModal(ctx, cb.TriggerID, teamID); err != nil {
				s.logger.Warn("open PAT modal failed",
					slog.String("slack_user_id", cb.User.ID),
					slog.String("error", err.Error()),
				)
			}

		case ActionDisconnect:
			userID := cb.User.ID
			s.runAsync("disconnect", func(asyncCtx context.Context) {
				s.handleDisconnectAction(asyncCtx, userID)
			})

		case ActionSwitchWorkspace:
			userID := cb.User.ID
			selectedVal := action.SelectedOption.Value
			s.runAsync("switch_workspace", func(asyncCtx context.Context) {
				s.handleSwitchWorkspaceAction(asyncCtx, userID, selectedVal)
			})

		case ActionPauseNotifications:
			userID := cb.User.ID
			s.runAsync("pause_notifications", func(asyncCtx context.Context) {
				s.handlePauseAction(asyncCtx, userID)
			})

		case ActionResumeNotifications:
			userID := cb.User.ID
			s.runAsync("resume_notifications", func(asyncCtx context.Context) {
				s.handleResumeAction(asyncCtx, userID)
			})

		case ActionUpdatePrefs:
			userID := cb.User.ID
			selected := append([]slack.OptionBlockObject(nil), action.SelectedOptions...)
			s.runAsync("update_prefs", func(asyncCtx context.Context) {
				s.handleUpdatePrefsAction(asyncCtx, userID, selected)
			})

		case notify.ActionMarkRead:
			cbCopy := cb
			val := action.Value
			s.runAsync("mark_read", func(asyncCtx context.Context) {
				s.handleMarkReadAction(asyncCtx, cbCopy, val)
			})

		case notify.ActionOpenReply:
			s.handleOpenReplyAction(ctx, cb, action.Value)

		case ActionEnableOAuth, notify.ActionOpenInMultica:
			// URL buttons require only 200 OK acknowledgement.
		}
	}
	w.WriteHeader(http.StatusOK)
}

func (s *Server) handleDisconnectAction(ctx context.Context, slackUserID string) {
	if s.notify != nil {
		s.notify.DisconnectUser(slackUserID)
	}
	if s.store != nil {
		if err := s.store.DisconnectUser(ctx, slackUserID); err != nil && !errors.Is(err, store.ErrNotFound) {
			s.logger.Warn("disconnect user in store failed",
				slog.String("slack_user_id", slackUserID),
				slog.String("error", err.Error()),
			)
		}
	}
	if s.cache != nil {
		s.cache.InvalidateUser(slackUserID)
	}
	if err := s.publishAppHome(ctx, slackUserID); err != nil {
		s.logger.Warn("refresh app home after disconnect failed",
			slog.String("slack_user_id", slackUserID),
			slog.String("error", err.Error()),
		)
	}
}

func (s *Server) handleSwitchWorkspaceAction(ctx context.Context, slackUserID, selectedValue string) {
	wsID, wsSlug, ok := strings.Cut(selectedValue, "|")
	if !ok || strings.TrimSpace(wsID) == "" {
		return
	}
	var err error
	if s.notify != nil {
		err = s.notify.SwitchUserWorkspace(ctx, slackUserID, wsID, wsSlug)
	} else if s.store != nil {
		err = s.store.SwitchWorkspace(ctx, slackUserID, wsID, wsSlug, s.now().UTC())
	}
	if err != nil {
		s.logger.Warn("switch workspace failed",
			slog.String("slack_user_id", slackUserID),
			slog.String("workspace_id", wsID),
			slog.String("error", err.Error()),
		)
	}
	if err := s.publishAppHome(ctx, slackUserID); err != nil {
		s.logger.Warn("refresh app home after workspace switch failed",
			slog.String("slack_user_id", slackUserID),
			slog.String("error", err.Error()),
		)
	}
}

func (s *Server) handlePauseAction(ctx context.Context, slackUserID string) {
	var err error
	if s.notify != nil {
		err = s.notify.PauseUser(ctx, slackUserID)
	} else if s.store != nil {
		err = s.store.SetNotificationsEnabled(ctx, slackUserID, false, nil)
	}
	if err != nil {
		s.logger.Warn("pause notifications failed",
			slog.String("slack_user_id", slackUserID),
			slog.String("error", err.Error()),
		)
	}
	if err := s.publishAppHome(ctx, slackUserID); err != nil {
		s.logger.Warn("refresh app home after pause failed",
			slog.String("slack_user_id", slackUserID),
			slog.String("error", err.Error()),
		)
	}
}

func (s *Server) handleResumeAction(ctx context.Context, slackUserID string) {
	var err error
	if s.notify != nil {
		err = s.notify.ResumeUser(ctx, slackUserID)
	} else if s.store != nil {
		now := s.now().UTC()
		err = s.store.SetNotificationsEnabled(ctx, slackUserID, true, &now)
	}
	if err != nil {
		s.logger.Warn("resume notifications failed",
			slog.String("slack_user_id", slackUserID),
			slog.String("error", err.Error()),
		)
	}
	if err := s.publishAppHome(ctx, slackUserID); err != nil {
		s.logger.Warn("refresh app home after resume failed",
			slog.String("slack_user_id", slackUserID),
			slog.String("error", err.Error()),
		)
	}
}

func (s *Server) handleUpdatePrefsAction(ctx context.Context, slackUserID string, selected []slack.OptionBlockObject) {
	updated := make(prefs.Preferences, len(prefs.AllGroups))
	known := make(map[prefs.Group]struct{}, len(prefs.AllGroups))
	for _, g := range prefs.AllGroups {
		updated[g.Key] = false
		known[g.Key] = struct{}{}
	}
	for _, opt := range selected {
		g := prefs.Group(opt.Value)
		if _, ok := known[g]; ok {
			updated[g] = true
		}
	}

	raw, err := json.Marshal(updated)
	if err != nil {
		s.logger.Warn("marshal notification prefs failed",
			slog.String("slack_user_id", slackUserID),
			slog.String("error", err.Error()),
		)
		return
	}
	if s.store != nil {
		if err := s.store.SetNotificationPrefs(ctx, slackUserID, string(raw)); err != nil {
			s.logger.Warn("save notification prefs failed",
				slog.String("slack_user_id", slackUserID),
				slog.String("error", err.Error()),
			)
		}
	}
	if err := s.publishAppHome(ctx, slackUserID); err != nil {
		s.logger.Warn("refresh app home after prefs update failed",
			slog.String("slack_user_id", slackUserID),
			slog.String("error", err.Error()),
		)
	}
}

func (s *Server) handleMarkReadAction(ctx context.Context, cb slack.InteractionCallback, value string) {
	inboxItemID, workspaceID, _ := strings.Cut(value, "|")
	inboxItemID = strings.TrimSpace(inboxItemID)
	workspaceID = strings.TrimSpace(workspaceID)
	if inboxItemID == "" {
		return
	}

	user, err := s.loadUserOrNil(ctx, cb.User.ID)
	if err != nil || user == nil || user.MulticaPAT == "" {
		s.logger.Warn("mark read ignored: user not linked",
			slog.String("slack_user_id", cb.User.ID),
			slog.String("inbox_item_id", inboxItemID),
		)
		return
	}
	if workspaceID == "" {
		workspaceID = user.MulticaWorkspaceID
	}

	if s.mc != nil {
		if err := s.mc.MarkInboxRead(ctx, user.MulticaPAT, workspaceID, inboxItemID); err != nil {
			s.logger.Warn("multica MarkInboxRead failed",
				slog.String("slack_user_id", cb.User.ID),
				slog.String("inbox_item_id", inboxItemID),
				slog.String("error", err.Error()),
			)
			return
		}
	}

	channelID := firstNonEmpty(cb.Channel.ID, cb.Container.ChannelID)
	messageTS := firstNonEmpty(cb.Message.Timestamp, cb.Container.MessageTs, cb.MessageTs)
	blocks := cb.Message.Blocks.BlockSet
	fallback := cb.Message.Text

	if (len(blocks) == 0 || channelID == "" || messageTS == "") && s.store != nil {
		if rec, err := s.store.GetUnreadDelivery(ctx, cb.User.ID, inboxItemID); err == nil && rec != nil {
			if len(blocks) == 0 {
				if parsed, unmarshalErr := notify.UnmarshalBlocks(rec.CardBlocksJSON); unmarshalErr == nil {
					blocks = parsed
				}
			}
			fallback = firstNonEmpty(fallback, rec.CardFallback)
			channelID = firstNonEmpty(channelID, rec.SlackChannelID)
			messageTS = firstNonEmpty(messageTS, rec.SlackMessageTS)
		}
	}

	if s.store != nil {
		if err := s.store.MarkDeliveryRead(ctx, cb.User.ID, inboxItemID, s.now().UTC()); err != nil {
			s.logger.Warn("store MarkDeliveryRead failed",
				slog.String("slack_user_id", cb.User.ID),
				slog.String("inbox_item_id", inboxItemID),
				slog.String("error", err.Error()),
			)
		}
	}

	s.updateDMCardRead(ctx, cb.User.ID, inboxItemID, channelID, messageTS, fallback, blocks)
}

func (s *Server) handleOpenReplyAction(ctx context.Context, cb slack.InteractionCallback, value string) {
	parts := strings.Split(value, "|")
	var issueID, issueIdentifier, inboxItemID, workspaceID string
	if len(parts) > 0 {
		issueID = strings.TrimSpace(parts[0])
	}
	if len(parts) > 1 {
		issueIdentifier = strings.TrimSpace(parts[1])
	}
	if len(parts) > 2 {
		inboxItemID = strings.TrimSpace(parts[2])
	}
	if len(parts) > 3 {
		workspaceID = strings.TrimSpace(parts[3])
	}
	if issueID == "" || s.slack == nil || cb.TriggerID == "" {
		return
	}

	channelID := firstNonEmpty(cb.Channel.ID, cb.Container.ChannelID)
	messageTS := firstNonEmpty(cb.Message.Timestamp, cb.Container.MessageTs, cb.MessageTs)
	privateMeta := strings.Join([]string{
		issueID,
		issueIdentifier,
		inboxItemID,
		workspaceID,
		channelID,
		messageTS,
	}, "|")

	if _, err := s.slack.OpenViewContext(ctx, cb.TriggerID, buildReplyModalView(issueIdentifier, privateMeta)); err != nil {
		s.logger.Warn("open reply modal failed",
			slog.String("slack_user_id", cb.User.ID),
			slog.String("issue_id", issueID),
			slog.String("error", err.Error()),
		)
	}
}

func (s *Server) handleViewSubmission(ctx context.Context, w http.ResponseWriter, cb slack.InteractionCallback, teamID string) {
	switch cb.View.CallbackID {
	case CallbackPATModal:
		s.handlePATModalSubmission(ctx, w, cb, teamID)
	case CallbackReplyModal:
		s.handleReplyModalSubmission(w, cb)
	default:
		w.WriteHeader(http.StatusOK)
	}
}

func (s *Server) handlePATModalSubmission(ctx context.Context, w http.ResponseWriter, cb slack.InteractionCallback, teamID string) {
	var pat string
	if cb.View.State != nil {
		pat = strings.TrimSpace(cb.View.State.Values[BlockIDPAT][ActionIDPATInput].Value)
	}
	if pat == "" {
		writeJSON(w, http.StatusOK, slack.NewErrorsViewSubmissionResponse(map[string]string{
			BlockIDPAT: "Please enter a Multica personal access token.",
		}))
		return
	}

	if s.mc == nil {
		writeJSON(w, http.StatusOK, slack.NewErrorsViewSubmissionResponse(map[string]string{
			BlockIDPAT: "Multica client is not configured.",
		}))
		return
	}
	if s.store == nil {
		writeJSON(w, http.StatusOK, slack.NewErrorsViewSubmissionResponse(map[string]string{
			BlockIDPAT: "Credential store is not configured.",
		}))
		return
	}

	me, err := s.mc.GetMe(ctx, pat)
	if err != nil {
		s.logger.Warn("multica PAT validation failed",
			slog.String("slack_user_id", cb.User.ID),
			slog.String("error", err.Error()),
		)
		writeJSON(w, http.StatusOK, slack.NewErrorsViewSubmissionResponse(map[string]string{
			BlockIDPAT: "Invalid Multica PAT — check your token and try again.",
		}))
		return
	}

	workspaces, err := s.mc.ListWorkspaces(ctx, pat)
	if err != nil || len(workspaces) == 0 {
		msg := "No Multica workspaces found for this PAT."
		if err != nil {
			s.logger.Warn("multica list workspaces during PAT connect failed",
				slog.String("slack_user_id", cb.User.ID),
				slog.String("error", err.Error()),
			)
			msg = "Unable to load Multica workspaces for this PAT."
		}
		writeJSON(w, http.StatusOK, slack.NewErrorsViewSubmissionResponse(map[string]string{
			BlockIDPAT: msg,
		}))
		return
	}

	var existingWorkspaceID string
	if existing, err := s.loadUserOrNil(ctx, cb.User.ID); err != nil {
		s.logger.Warn("load existing user before PAT upsert failed",
			slog.String("slack_user_id", cb.User.ID),
			slog.String("error", err.Error()),
		)
	} else if existing != nil {
		existingWorkspaceID = strings.TrimSpace(existing.MulticaWorkspaceID)
	}

	selected := selectWorkspaceForPAT(workspaces, existingWorkspaceID, s.cfg.MulticaDefaultWorkspaceSlug)
	userTeamID := firstNonEmpty(teamID, cb.View.PrivateMetadata)
	if err := s.store.UpsertMulticaConnection(ctx, store.UpsertMulticaParams{
		SlackUserID:          cb.User.ID,
		SlackTeamID:          userTeamID,
		MulticaUserID:        me.ID,
		MulticaUserName:      firstNonEmpty(me.Name, me.Email, me.ID),
		MulticaWorkspaceID:   selected.ID,
		MulticaWorkspaceSlug: selected.Slug,
		MulticaPAT:           pat,
		NotifyAfter:          s.now().UTC(),
	}); err != nil {
		s.logger.Error("store upsert multica connection failed",
			slog.String("slack_user_id", cb.User.ID),
			slog.String("error", err.Error()),
		)
		writeJSON(w, http.StatusOK, slack.NewErrorsViewSubmissionResponse(map[string]string{
			BlockIDPAT: "Failed to save Multica credentials. Please try again.",
		}))
		return
	}

	if s.cache != nil {
		s.cache.InvalidateUser(cb.User.ID)
	}

	// Validation and persistence succeeded; acknowledge immediately so Slack closes
	// the modal within 3 seconds while WS connection and App Home refresh run async.
	w.WriteHeader(http.StatusOK)

	userID := cb.User.ID
	s.runAsync("pat_modal_connect", func(asyncCtx context.Context) {
		if s.notify != nil {
			if err := s.notify.ConnectUser(asyncCtx, userID); err != nil {
				s.logger.Warn("notify ConnectUser after PAT modal failed",
					slog.String("slack_user_id", userID),
					slog.String("error", err.Error()),
				)
			}
		}
		if err := s.publishAppHome(asyncCtx, userID); err != nil {
			s.logger.Warn("refresh app home after PAT modal failed",
				slog.String("slack_user_id", userID),
				slog.String("error", err.Error()),
			)
		}
	})
}

func (s *Server) handleReplyModalSubmission(w http.ResponseWriter, cb slack.InteractionCallback) {
	var comment string
	if cb.View.State != nil {
		comment = strings.TrimSpace(cb.View.State.Values[BlockIDReply][ActionIDReplyInput].Value)
	}
	if comment == "" {
		writeJSON(w, http.StatusOK, slack.NewErrorsViewSubmissionResponse(map[string]string{
			BlockIDReply: "Please enter a comment.",
		}))
		return
	}

	parts := strings.Split(cb.View.PrivateMetadata, "|")
	var issueID, issueIdentifier, inboxItemID, workspaceID, channelID, messageTS string
	if len(parts) > 0 {
		issueID = strings.TrimSpace(parts[0])
	}
	if len(parts) > 1 {
		issueIdentifier = strings.TrimSpace(parts[1])
	}
	if len(parts) > 2 {
		inboxItemID = strings.TrimSpace(parts[2])
	}
	if len(parts) > 3 {
		workspaceID = strings.TrimSpace(parts[3])
	}
	if len(parts) > 4 {
		channelID = strings.TrimSpace(parts[4])
	}
	if len(parts) > 5 {
		messageTS = strings.TrimSpace(parts[5])
	}

	// Acknowledge immediately so Slack closes the modal within 3 seconds.
	w.WriteHeader(http.StatusOK)

	if issueID == "" {
		return
	}

	userID := cb.User.ID
	s.runAsync("reply_modal", func(ctx context.Context) {
		user, err := s.loadUserOrNil(ctx, userID)
		if err != nil || user == nil || user.MulticaPAT == "" {
			s.logger.Warn("reply submission failed: user not linked",
				slog.String("slack_user_id", userID),
				slog.String("issue_id", issueID),
			)
			return
		}
		wsID := firstNonEmpty(workspaceID, user.MulticaWorkspaceID)
		if s.mc != nil {
			if err := s.mc.CreateComment(ctx, user.MulticaPAT, wsID, issueID, comment); err != nil {
				s.logger.Warn("multica CreateComment failed",
					slog.String("slack_user_id", userID),
					slog.String("issue_id", issueID),
					slog.String("error", err.Error()),
				)
				return
			}
		}

		var (
			cardBlocks    []slack.Block
			cardFallback  string
			hasUnreadCard bool
		)
		if inboxItemID != "" {
			if s.mc != nil {
				if err := s.mc.MarkInboxRead(ctx, user.MulticaPAT, wsID, inboxItemID); err != nil {
					s.logger.Warn("multica MarkInboxRead after reply failed",
						slog.String("slack_user_id", userID),
						slog.String("inbox_item_id", inboxItemID),
						slog.String("error", err.Error()),
					)
				}
			}
			if s.store != nil {
				if rec, err := s.store.GetUnreadDelivery(ctx, userID, inboxItemID); err == nil && rec != nil {
					if parsed, unmarshalErr := notify.UnmarshalBlocks(rec.CardBlocksJSON); unmarshalErr == nil {
						cardBlocks = parsed
					}
					cardFallback = rec.CardFallback
					channelID = firstNonEmpty(channelID, rec.SlackChannelID)
					messageTS = firstNonEmpty(messageTS, rec.SlackMessageTS)
					hasUnreadCard = len(cardBlocks) > 0 || strings.TrimSpace(cardFallback) != ""
				}
				if err := s.store.MarkDeliveryRead(ctx, userID, inboxItemID, s.now().UTC()); err != nil {
					s.logger.Warn("store MarkDeliveryRead after reply failed",
						slog.String("slack_user_id", userID),
						slog.String("inbox_item_id", inboxItemID),
						slog.String("error", err.Error()),
					)
				}
			}
			if hasUnreadCard {
				s.updateDMCardRead(ctx, userID, inboxItemID, channelID, messageTS, cardFallback, cardBlocks)
			}
		}

		if s.slack != nil && channelID != "" && messageTS != "" {
			confirmText := "Comment posted."
			if issueIdentifier != "" {
				confirmText = fmt.Sprintf("Comment posted to %s.", issueIdentifier)
			}
			if _, _, err := s.slack.PostMessageContext(
				ctx,
				channelID,
				slack.MsgOptionTS(messageTS),
				slack.MsgOptionText(confirmText, false),
			); err != nil {
				s.logger.Warn("post reply confirmation thread message failed",
					slog.String("slack_user_id", userID),
					slog.String("issue_id", issueID),
					slog.String("error", err.Error()),
				)
			}
		}
	})
}

func (s *Server) updateDMCardRead(ctx context.Context, slackUserID, inboxItemID, channelID, messageTS, fallback string, blocks []slack.Block) {
	if s.slack == nil || channelID == "" || messageTS == "" {
		return
	}
	if len(blocks) == 0 && strings.TrimSpace(fallback) == "" {
		return
	}
	if len(blocks) == 0 && fallback != "" {
		blocks = []slack.Block{
			slack.NewSectionBlock(
				slack.NewTextBlockObject(slack.MarkdownType, escapeSlackMrkdwn(fallback), false, false),
				nil,
				nil,
			),
		}
	}
	updated := notify.MarkBlocksRead(blocks)
	if _, _, _, err := s.slack.UpdateMessageContext(
		ctx,
		channelID,
		messageTS,
		slack.MsgOptionText(fallback, false),
		slack.MsgOptionBlocks(updated...),
	); err != nil {
		s.logger.Warn("update Slack DM card read state failed",
			slog.String("slack_user_id", slackUserID),
			slog.String("inbox_item_id", inboxItemID),
			slog.String("error", err.Error()),
		)
	}
}

func selectWorkspaceForPAT(workspaces []multica.Workspace, existingWorkspaceID, preferredSlug string) multica.Workspace {
	if existingWorkspaceID != "" {
		for _, ws := range workspaces {
			if strings.TrimSpace(ws.ID) == existingWorkspaceID {
				return ws
			}
		}
	}
	return selectDefaultWorkspace(workspaces, preferredSlug)
}

func selectDefaultWorkspace(workspaces []multica.Workspace, preferredSlug string) multica.Workspace {
	slug := strings.TrimSpace(preferredSlug)
	if slug != "" {
		for _, ws := range workspaces {
			if strings.EqualFold(strings.TrimSpace(ws.Slug), slug) {
				return ws
			}
		}
	}
	return workspaces[0]
}
