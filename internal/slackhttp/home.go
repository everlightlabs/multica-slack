package slackhttp

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"unicode/utf8"

	"github.com/everlightlabs/multica-slack/internal/crypto"
	"github.com/everlightlabs/multica-slack/internal/multica"
	"github.com/everlightlabs/multica-slack/internal/prefs"
	"github.com/everlightlabs/multica-slack/internal/store"
	"github.com/slack-go/slack"
)

const (
	// ActionOpenPATModal opens the Multica PAT connection modal.
	ActionOpenPATModal = "multica_open_pat_modal"
	// ActionDisconnect disconnects a user's Multica PAT and Slack user token.
	ActionDisconnect = "multica_disconnect"
	// ActionSwitchWorkspace changes the user's active notification workspace.
	ActionSwitchWorkspace = "multica_switch_workspace"
	// ActionEnableOAuth is the URL button starting Slack per-user OAuth for DM unfurling.
	ActionEnableOAuth = "multica_enable_oauth"
	// ActionPauseNotifications pauses DM notifications and closes the user's WS stream.
	ActionPauseNotifications = "multica_pause_notifications"
	// ActionResumeNotifications resumes DM notifications and restarts the user's WS stream.
	ActionResumeNotifications = "multica_resume_notifications"
	// ActionUpdatePrefs updates the user's notification preference checkboxes.
	ActionUpdatePrefs = "multica_update_prefs"

	// CallbackPATModal identifies the PAT connect/update modal submission.
	CallbackPATModal = "multica_pat_modal"
	// CallbackReplyModal identifies the issue comment reply modal submission.
	CallbackReplyModal = "multica_reply_modal"

	// BlockIDPAT is the InputBlock block_id for the PAT text input.
	BlockIDPAT = "pat_block"
	// ActionIDPATInput is the action_id for the PAT text input.
	ActionIDPATInput = "pat_input"

	// BlockIDReply is the InputBlock block_id for the comment reply textarea.
	BlockIDReply = "reply_block"
	// ActionIDReplyInput is the action_id for the comment reply textarea.
	ActionIDReplyInput = "reply_input"
)

// RefreshAppHome asynchronously republishes the user's App Home tab.
func (s *Server) RefreshAppHome(slackUserID string) {
	if strings.TrimSpace(slackUserID) == "" {
		return
	}
	s.runAsync("ws_state_home_refresh", func(asyncCtx context.Context) {
		if err := s.publishAppHome(asyncCtx, slackUserID); err != nil {
			s.logger.Warn("refresh app home on ws state change failed",
				slog.String("slack_user_id", slackUserID),
				slog.String("error", err.Error()),
			)
		}
	})
}

func (s *Server) publishAppHome(ctx context.Context, slackUserID string) error {
	if s.slack == nil || strings.TrimSpace(slackUserID) == "" {
		return nil
	}
	view, err := s.buildHomeView(ctx, slackUserID)
	if err != nil {
		return err
	}
	_, err = s.slack.PublishViewContext(ctx, slack.PublishViewContextRequest{
		UserID: slackUserID,
		View:   view,
	})
	if err != nil {
		return fmt.Errorf("slackhttp: views.publish for %s: %w", slackUserID, err)
	}
	return nil
}

func (s *Server) buildHomeView(ctx context.Context, slackUserID string) (slack.HomeTabViewRequest, error) {
	user, err := s.loadUserOrNil(ctx, slackUserID)
	if err != nil {
		return slack.HomeTabViewRequest{}, err
	}

	var blocks []slack.Block
	blocks = append(blocks, slack.NewHeaderBlock(
		slack.NewTextBlockObject(slack.PlainTextType, "Multica for Slack", false, false),
	))

	// Section 1: Multica Account
	isLinkedOK := user != nil && user.MulticaPAT != "" && user.AuthStatus == store.AuthOK
	isAuthFailed := user != nil && user.AuthStatus == store.AuthFailed

	switch {
	case isLinkedOK:
		name := firstNonEmpty(user.MulticaUserName, user.MulticaUserID, "Multica user")
		wsSlug := firstNonEmpty(user.MulticaWorkspaceSlug, user.MulticaWorkspaceID, "unknown")
		wsState := s.userWSState(user)
		accountText := fmt.Sprintf(
			"*Multica Account*\nConnected as %s · %s (WS: %s)",
			escapeSlackMrkdwn(name),
			escapeSlackMrkdwn(wsSlug),
			wsState,
		)
		blocks = append(blocks, slack.NewSectionBlock(
			slack.NewTextBlockObject(slack.MarkdownType, accountText, false, false),
			nil,
			nil,
		))

		if workspaces := s.loadUserWorkspaces(ctx, user); len(workspaces) > 1 {
			blocks = append(blocks, buildWorkspaceSelectorBlock(user, workspaces))
		}

		updateBtn := slack.NewButtonBlockElement(
			ActionOpenPATModal,
			"update",
			slack.NewTextBlockObject(slack.PlainTextType, "Update PAT", false, false),
		)
		disconnectBtn := slack.NewButtonBlockElement(
			ActionDisconnect,
			"disconnect",
			slack.NewTextBlockObject(slack.PlainTextType, "Disconnect", false, false),
		).WithStyle(slack.StyleDanger)
		blocks = append(blocks, slack.NewActionBlock("multica_account_actions", updateBtn, disconnectBtn))

	case isAuthFailed:
		blocks = append(blocks, slack.NewSectionBlock(
			slack.NewTextBlockObject(slack.MarkdownType, "*Multica Account*\nPAT invalid — please reconnect", false, false),
			nil,
			nil,
		))
		updateBtn := slack.NewButtonBlockElement(
			ActionOpenPATModal,
			"update",
			slack.NewTextBlockObject(slack.PlainTextType, "Update PAT", false, false),
		).WithStyle(slack.StylePrimary)
		disconnectBtn := slack.NewButtonBlockElement(
			ActionDisconnect,
			"disconnect",
			slack.NewTextBlockObject(slack.PlainTextType, "Disconnect", false, false),
		).WithStyle(slack.StyleDanger)
		blocks = append(blocks, slack.NewActionBlock("multica_account_actions", updateBtn, disconnectBtn))

	default:
		blocks = append(blocks, slack.NewSectionBlock(
			slack.NewTextBlockObject(slack.MarkdownType, "*Multica Account*\nNot connected — link your Multica personal access token (`mul_...`) to unfurl links and receive real-time DM notifications.", false, false),
			nil,
			nil,
		))
		connectBtn := slack.NewButtonBlockElement(
			ActionOpenPATModal,
			"connect",
			slack.NewTextBlockObject(slack.PlainTextType, "Connect PAT", false, false),
		).WithStyle(slack.StylePrimary)
		blocks = append(blocks, slack.NewActionBlock("multica_account_actions", connectBtn))
	}

	blocks = append(blocks, slack.NewDividerBlock())

	// Section 2: 1:1 DM & Uninvited-Channel Unfurling
	hasUserToken := user != nil && user.SlackUserToken != ""
	step2Unlocked := isLinkedOK || strings.TrimSpace(s.cfg.MulticaFallbackPAT) != ""

	switch {
	case hasUserToken:
		blocks = append(blocks, slack.NewSectionBlock(
			slack.NewTextBlockObject(slack.MarkdownType, "*1:1 DM & Uninvited-Channel Unfurling*\nEnabled — Multica links unfurl automatically in your DMs and private/uninvited channels.", false, false),
			nil,
			nil,
		))
	case step2Unlocked:
		startToken := crypto.SignStartToken(s.cfg.EncryptionKey, slackUserID, s.now())
		oauthURL := strings.TrimRight(s.cfg.PublicBaseURL, "/") + "/slack/oauth/start?token=" + url.QueryEscape(startToken)
		blocks = append(blocks, slack.NewSectionBlock(
			slack.NewTextBlockObject(slack.MarkdownType, "*1:1 DM & Uninvited-Channel Unfurling*\nAuthorize Slack link scopes (`links:read`, `links:write`) so Multica links unfurl in 1:1 DMs and channels the bot is not in.", false, false),
			nil,
			nil,
		))
		enableBtn := slack.NewButtonBlockElement(
			ActionEnableOAuth,
			"enable_oauth",
			slack.NewTextBlockObject(slack.PlainTextType, "Enable DM & Channel Unfurling", false, false),
		).WithURL(oauthURL)
		blocks = append(blocks, slack.NewActionBlock("multica_oauth_actions", enableBtn))
	default:
		blocks = append(blocks, slack.NewSectionBlock(
			slack.NewTextBlockObject(slack.MarkdownType, "*1:1 DM & Uninvited-Channel Unfurling*\nConnect your Multica PAT above first to enable unfurling in 1:1 DMs and uninvited channels.", false, false),
			nil,
			nil,
		))
	}

	// Section 3: Notification Preferences (shown when linked)
	if isLinkedOK || isAuthFailed {
		blocks = append(blocks, slack.NewDividerBlock())

		prefHeading := "*Notification Preferences*\nDelivery is active."
		var toggleBtn *slack.ButtonBlockElement
		if user.NotificationsEnabled {
			toggleBtn = slack.NewButtonBlockElement(
				ActionPauseNotifications,
				"pause",
				slack.NewTextBlockObject(slack.PlainTextType, "Pause Notifications", false, false),
			)
		} else {
			prefHeading = "*Notification Preferences*\nDelivery is paused. Items arriving while paused will not be backfilled on resume."
			toggleBtn = slack.NewButtonBlockElement(
				ActionResumeNotifications,
				"resume",
				slack.NewTextBlockObject(slack.PlainTextType, "Resume Notifications", false, false),
			).WithStyle(slack.StylePrimary)
		}

		blocks = append(blocks, slack.NewSectionBlock(
			slack.NewTextBlockObject(slack.MarkdownType, prefHeading, false, false),
			nil,
			nil,
		))
		blocks = append(blocks, slack.NewActionBlock("multica_notify_toggle", toggleBtn))

		userPrefs := prefs.Parse(user.NotificationPrefsJSON)
		options := make([]*slack.OptionBlockObject, 0, len(prefs.AllGroups))
		initial := make([]*slack.OptionBlockObject, 0, len(prefs.AllGroups))
		for _, g := range prefs.AllGroups {
			opt := slack.NewOptionBlockObject(
				string(g.Key),
				slack.NewTextBlockObject(slack.PlainTextType, g.Label, false, false),
				nil,
			)
			options = append(options, opt)
			if userPrefs[g.Key] {
				initial = append(initial, opt)
			}
		}

		checkboxes := slack.NewCheckboxGroupsBlockElement(ActionUpdatePrefs, options...)
		if len(initial) > 0 {
			checkboxes.InitialOptions = initial
		}
		blocks = append(blocks, slack.NewActionBlock("multica_prefs_block", checkboxes))
	}

	return slack.HomeTabViewRequest{
		Type:   slack.VTHomeTab,
		Blocks: slack.Blocks{BlockSet: blocks},
	}, nil
}

func (s *Server) loadUserOrNil(ctx context.Context, slackUserID string) (*store.User, error) {
	if s.store == nil || slackUserID == "" {
		return nil, nil
	}
	u, err := s.store.GetUser(ctx, slackUserID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return u, nil
}

func (s *Server) loadUserWorkspaces(ctx context.Context, user *store.User) []multica.Workspace {
	if s.mc == nil || user == nil || user.MulticaPAT == "" {
		return nil
	}
	workspaces, err := s.mc.ListWorkspaces(ctx, user.MulticaPAT)
	if err != nil {
		s.logger.Warn("load workspaces for app home failed",
			slog.String("slack_user_id", user.SlackUserID),
			slog.String("error", err.Error()),
		)
		return nil
	}
	return workspaces
}

func (s *Server) userWSState(user *store.User) string {
	if user == nil || user.MulticaPAT == "" || user.AuthStatus == store.AuthDisconnected {
		return "disconnected"
	}
	if user.AuthStatus == store.AuthFailed {
		return "auth_failed"
	}
	if !user.NotificationsEnabled {
		return "paused"
	}
	if s.notify != nil && s.notify.IsConnected(user.SlackUserID) {
		return "connected"
	}
	return "reconnecting"
}

func buildWorkspaceSelectorBlock(user *store.User, workspaces []multica.Workspace) *slack.SectionBlock {
	options := make([]*slack.OptionBlockObject, 0, len(workspaces))
	var initial *slack.OptionBlockObject
	for _, ws := range workspaces {
		label := truncatePlainText(firstNonEmpty(ws.Name, ws.Slug, ws.ID), 75)
		val := ws.ID + "|" + ws.Slug
		opt := slack.NewOptionBlockObject(
			val,
			slack.NewTextBlockObject(slack.PlainTextType, label, false, false),
			nil,
		)
		options = append(options, opt)
		if (user.MulticaWorkspaceID != "" && ws.ID == user.MulticaWorkspaceID) ||
			(user.MulticaWorkspaceID == "" && strings.EqualFold(ws.Slug, user.MulticaWorkspaceSlug)) {
			initial = opt
		}
	}

	sel := slack.NewOptionsSelectBlockElement(
		slack.OptTypeStatic,
		slack.NewTextBlockObject(slack.PlainTextType, "Select workspace", false, false),
		ActionSwitchWorkspace,
		options...,
	)
	if initial != nil {
		sel.InitialOption = initial
	}

	return slack.NewSectionBlock(
		slack.NewTextBlockObject(slack.MarkdownType, "*Notification Workspace*\nChoose which Multica workspace streams inbox notifications to your DMs.", false, false),
		nil,
		slack.NewAccessory(sel),
	)
}

func buildPATModalView(teamID string) slack.ModalViewRequest {
	patInput := slack.NewPlainTextInputBlockElement(
		slack.NewTextBlockObject(slack.PlainTextType, "mul_...", false, false),
		ActionIDPATInput,
	)
	inputBlock := slack.NewInputBlock(
		BlockIDPAT,
		slack.NewTextBlockObject(slack.PlainTextType, "Multica Personal Access Token (PAT)", false, false),
		slack.NewTextBlockObject(slack.PlainTextType, "Create a PAT in Multica Settings → API Tokens. Stored encrypted with AES-256-GCM.", false, false),
		patInput,
	)
	return slack.ModalViewRequest{
		Type:            slack.VTModal,
		CallbackID:      CallbackPATModal,
		PrivateMetadata: teamID,
		Title:           slack.NewTextBlockObject(slack.PlainTextType, "Connect Multica", false, false),
		Submit:          slack.NewTextBlockObject(slack.PlainTextType, "Connect", false, false),
		Close:           slack.NewTextBlockObject(slack.PlainTextType, "Cancel", false, false),
		Blocks:          slack.Blocks{BlockSet: []slack.Block{inputBlock}},
	}
}

func buildReplyModalView(issueIdentifier, privateMetadata string) slack.ModalViewRequest {
	title := "Reply to Issue"
	if id := strings.TrimSpace(issueIdentifier); id != "" {
		candidate := "Reply to " + id
		if utf8.RuneCountInString(candidate) <= 24 {
			title = candidate
		}
	}

	replyInput := slack.NewPlainTextInputBlockElement(
		slack.NewTextBlockObject(slack.PlainTextType, "Write your comment...", false, false),
		ActionIDReplyInput,
	)
	replyInput.Multiline = true

	inputBlock := slack.NewInputBlock(
		BlockIDReply,
		slack.NewTextBlockObject(slack.PlainTextType, "Comment", false, false),
		nil,
		replyInput,
	)
	return slack.ModalViewRequest{
		Type:            slack.VTModal,
		CallbackID:      CallbackReplyModal,
		PrivateMetadata: privateMetadata,
		Title:           slack.NewTextBlockObject(slack.PlainTextType, title, false, false),
		Submit:          slack.NewTextBlockObject(slack.PlainTextType, "Post Comment", false, false),
		Close:           slack.NewTextBlockObject(slack.PlainTextType, "Cancel", false, false),
		Blocks:          slack.Blocks{BlockSet: []slack.Block{inputBlock}},
	}
}

func (s *Server) openPATModal(ctx context.Context, triggerID, teamID string) error {
	if s.slack == nil || strings.TrimSpace(triggerID) == "" {
		return nil
	}
	_, err := s.slack.OpenViewContext(ctx, triggerID, buildPATModalView(teamID))
	if err != nil {
		return fmt.Errorf("slackhttp: views.open PAT modal: %w", err)
	}
	return nil
}

func truncatePlainText(s string, maxRunes int) string {
	if maxRunes <= 0 || utf8.RuneCountInString(s) <= maxRunes {
		return s
	}
	runes := []rune(s)
	if maxRunes <= 1 {
		return string(runes[:maxRunes])
	}
	return string(runes[:maxRunes-1]) + "…"
}
