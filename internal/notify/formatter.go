// Package notify manages per-user Multica WebSocket streams, inbox catch-up,
// read-state synchronization, and Slack Block Kit DM notification formatting.
package notify

import (
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/everlightlabs/multica-slack/internal/multica"
	"github.com/slack-go/slack"
)

var (
	reMentionLink   = regexp.MustCompile(`\[([^\]]+)\]\(mention://([^/\s)]+)/[^)\s]+\)`)
	reMarkdownImage = regexp.MustCompile(`!\[([^\]]*)\]\([^)]+\)`)
	reMarkdownLink  = regexp.MustCompile(`\[([^\]]+)\]\([^)\s]+\)`)
	reCodeFence     = regexp.MustCompile("(?m)^```[^\\n]*\\n?")
)

const (
	// ActionMarkRead is the Slack Block Kit action_id for marking an inbox item read.
	ActionMarkRead = "multica_mark_read"
	// ActionOpenReply is the Slack Block Kit action_id for opening the comment reply modal.
	ActionOpenReply = "multica_open_reply"
	// ActionOpenInMultica is the Slack Block Kit action_id for the external Multica link button.
	ActionOpenInMultica = "multica_open_in_multica"

	actionsBlockID    = "multica_actions"
	readStatusBlockID = "multica_read_status"
	readStatusText    = "✓ Read"
	maxBodyQuoteRunes = 298 // leaves 2 runes for "> " blockquote prefix (total <= 300)
)

// FormatNotificationBlocks builds the Slack DM fallback text and Block Kit blocks for a
// Multica inbox notification item. When markedRead is true, the Mark Read button is omitted
// and a "✓ Read" context indicator is included.
func FormatNotificationBlocks(appBaseURL, workspaceSlug string, item multica.InboxItem, meta multica.WorkspaceMeta, markedRead bool) (fallback string, blocks []slack.Block) {
	actor := resolveActorName(item, meta)
	verb := actionVerbForType(item.Type)
	targetURL := buildItemURL(appBaseURL, workspaceSlug, item)
	identifier := strings.TrimSpace(item.IssueIdentifier)
	title := strings.TrimSpace(item.Title)

	headerText, fallbackText := buildHeaderAndFallback(actor, verb, targetURL, identifier, title)
	blocks = append(blocks, slack.NewSectionBlock(
		slack.NewTextBlockObject(slack.MarkdownType, headerText, false, false),
		nil,
		nil,
	))

	if body := formatBodyQuote(item.Body); body != "" {
		blocks = append(blocks, slack.NewSectionBlock(
			slack.NewTextBlockObject(slack.MarkdownType, body, false, false),
			nil,
			nil,
		))
	}

	if ctxLine := buildContextLine(item, meta); ctxLine != "" {
		blocks = append(blocks, slack.NewContextBlock(
			"",
			slack.NewTextBlockObject(slack.MarkdownType, ctxLine, false, false),
		))
	}

	var buttons []slack.BlockElement
	if targetURL != "" {
		openBtn := slack.NewButtonBlockElement(
			ActionOpenInMultica,
			"",
			slack.NewTextBlockObject(slack.PlainTextType, "Open in Multica", false, false),
		).WithURL(targetURL)
		buttons = append(buttons, openBtn)
	}
	if !markedRead {
		markReadValue := item.ID + "|" + item.WorkspaceID
		buttons = append(buttons, slack.NewButtonBlockElement(
			ActionMarkRead,
			markReadValue,
			slack.NewTextBlockObject(slack.PlainTextType, "Mark Read", false, false),
		))
	}
	if strings.TrimSpace(item.IssueID) != "" {
		replyValue := strings.Join([]string{
			strings.TrimSpace(item.IssueID),
			identifier,
			item.ID,
			item.WorkspaceID,
		}, "|")
		buttons = append(buttons, slack.NewButtonBlockElement(
			ActionOpenReply,
			replyValue,
			slack.NewTextBlockObject(slack.PlainTextType, "Reply", false, false),
		))
	}

	if len(buttons) > 0 {
		blocks = append(blocks, slack.NewActionBlock(actionsBlockID, buttons...))
	}
	if markedRead {
		blocks = append(blocks, newReadStatusContextBlock())
	}

	return fallbackText, blocks
}

// MarkBlocksRead returns a copy of blocks with the multica_mark_read button stripped from any
// actions block (dropping the actions block if empty) and a "✓ Read" context indicator appended
// idempotently.
func MarkBlocksRead(blocks []slack.Block) []slack.Block {
	out := make([]slack.Block, 0, len(blocks)+1)
	hasReadMarker := false

	for _, b := range blocks {
		switch blk := b.(type) {
		case *slack.ActionBlock:
			if updated := stripMarkReadFromActionBlock(blk); updated != nil {
				out = append(out, updated)
			}
		case slack.ActionBlock:
			if updated := stripMarkReadFromActionBlock(&blk); updated != nil {
				out = append(out, updated)
			}
		case *slack.ContextBlock:
			if contextBlockHasReadMarker(blk) {
				hasReadMarker = true
			}
			out = append(out, blk)
		case slack.ContextBlock:
			if contextBlockHasReadMarker(&blk) {
				hasReadMarker = true
			}
			out = append(out, blk)
		default:
			out = append(out, b)
		}
	}

	if !hasReadMarker {
		out = append(out, newReadStatusContextBlock())
	}
	return out
}

// MarshalBlocks serializes a Block Kit block slice into JSON for persistence.
func MarshalBlocks(blocks []slack.Block) (string, error) {
	raw, err := json.Marshal(slack.Blocks{BlockSet: blocks})
	if err != nil {
		return "", fmt.Errorf("notify: marshal blocks: %w", err)
	}
	return string(raw), nil
}

// UnmarshalBlocks deserializes a persisted Block Kit JSON array back into typed slack.Block values.
func UnmarshalBlocks(raw string) ([]slack.Block, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil, nil
	}
	var wrapper slack.Blocks
	if err := json.Unmarshal([]byte(trimmed), &wrapper); err != nil {
		return nil, fmt.Errorf("notify: unmarshal blocks: %w", err)
	}
	return wrapper.BlockSet, nil
}

// formatAuthFailedDM builds the single reconnect-prompt DM sent when a user's Multica PAT fails auth.
func formatAuthFailedDM() (fallback string, blocks []slack.Block) {
	fallback = "Your Multica personal access token is no longer valid. Open the Multica app Home tab to update your PAT and resume notifications."
	blocks = []slack.Block{
		slack.NewSectionBlock(
			slack.NewTextBlockObject(
				slack.MarkdownType,
				"*Multica connection needs attention*\nYour Multica personal access token (PAT) was rejected or revoked, so real-time notifications have been paused. Open the *Home* tab of this app to update your PAT and reconnect.",
				false,
				false,
			),
			nil,
			nil,
		),
	}
	return fallback, blocks
}

func newReadStatusContextBlock() *slack.ContextBlock {
	return slack.NewContextBlock(
		readStatusBlockID,
		slack.NewTextBlockObject(slack.MarkdownType, readStatusText, false, false),
	)
}

func stripMarkReadFromActionBlock(ab *slack.ActionBlock) *slack.ActionBlock {
	if ab == nil || ab.Elements == nil || len(ab.Elements.ElementSet) == 0 {
		return nil
	}
	kept := make([]slack.BlockElement, 0, len(ab.Elements.ElementSet))
	for _, el := range ab.Elements.ElementSet {
		switch btn := el.(type) {
		case *slack.ButtonBlockElement:
			if btn != nil && btn.ActionID == ActionMarkRead {
				continue
			}
		case slack.ButtonBlockElement:
			if btn.ActionID == ActionMarkRead {
				continue
			}
		}
		kept = append(kept, el)
	}
	if len(kept) == 0 {
		return nil
	}
	return slack.NewActionBlock(ab.BlockID, kept...)
}

func contextBlockHasReadMarker(cb *slack.ContextBlock) bool {
	if cb == nil {
		return false
	}
	if cb.BlockID == readStatusBlockID {
		return true
	}
	for _, el := range cb.ContextElements.Elements {
		switch txt := el.(type) {
		case *slack.TextBlockObject:
			if txt != nil && strings.Contains(txt.Text, readStatusText) {
				return true
			}
		case slack.TextBlockObject:
			if strings.Contains(txt.Text, readStatusText) {
				return true
			}
		}
	}
	return false
}

func resolveActorName(item multica.InboxItem, meta multica.WorkspaceMeta) string {
	actorID := strings.TrimSpace(item.ActorID)
	actorType := strings.ToLower(strings.TrimSpace(item.ActorType))

	if actorID != "" {
		switch actorType {
		case "agent":
			if a, ok := meta.AgentsByID[actorID]; ok && strings.TrimSpace(a.Name) != "" {
				return strings.TrimSpace(a.Name)
			}
		case "squad":
			if sq, ok := meta.SquadsByID[actorID]; ok && strings.TrimSpace(sq.Name) != "" {
				return strings.TrimSpace(sq.Name)
			}
		default:
			if m, ok := meta.MembersByUserID[actorID]; ok && strings.TrimSpace(m.Name) != "" {
				return strings.TrimSpace(m.Name)
			}
			if a, ok := meta.AgentsByID[actorID]; ok && strings.TrimSpace(a.Name) != "" {
				return strings.TrimSpace(a.Name)
			}
			if sq, ok := meta.SquadsByID[actorID]; ok && strings.TrimSpace(sq.Name) != "" {
				return strings.TrimSpace(sq.Name)
			}
		}
	}

	if actorType != "" {
		return humanizeToken(actorType)
	}
	return "Multica"
}

func actionVerbForType(inboxType string) string {
	switch inboxType {
	case "mentioned", "doc_mentioned":
		return "mentioned you in"
	case "issue_assigned":
		return "assigned you to"
	case "unassigned":
		return "unassigned you from"
	case "assignee_changed":
		return "changed assignee on"
	case "new_comment", "doc_comment":
		return "commented on"
	case "status_changed":
		return "updated status on"
	case "task_failed":
		return "reported a failed task on"
	case "app_action_result":
		return "completed an action on"
	case "app_action_requested":
		return "requested an action on"
	case "app_action_reminder":
		return "reminded you about"
	case "app_connect_requested":
		return "requested a connection for"
	case "children_done":
		return "completed sub-issues on"
	case "blockers_done":
		return "resolved blockers on"
	case "priority_changed":
		return "changed priority on"
	case "start_date_changed":
		return "changed start date on"
	case "due_date_changed":
		return "changed due date on"
	case "issue_created":
		return "created"
	case "reaction_added":
		return "reacted to"
	case "issue_subscribed":
		return "subscribed you to"
	case "project_owner_joined":
		return "joined"
	default:
		return "updated"
	}
}

func buildItemURL(appBaseURL, workspaceSlug string, item multica.InboxItem) string {
	base := strings.TrimRight(strings.TrimSpace(appBaseURL), "/")
	slug := strings.TrimSpace(workspaceSlug)
	if base == "" || slug == "" {
		return ""
	}
	escSlug := url.PathEscape(slug)
	if id := strings.TrimSpace(item.IssueIdentifier); id != "" {
		return fmt.Sprintf("%s/%s/issues/%s", base, escSlug, url.PathEscape(id))
	}
	if id := strings.TrimSpace(item.IssueID); id != "" {
		return fmt.Sprintf("%s/%s/issues/%s", base, escSlug, url.PathEscape(id))
	}
	if docID := extractDocID(item.Details); docID != "" {
		return fmt.Sprintf("%s/%s/docs/%s", base, escSlug, url.PathEscape(docID))
	}
	return fmt.Sprintf("%s/%s/inbox", base, escSlug)
}

func extractDocID(details json.RawMessage) string {
	if len(details) == 0 {
		return ""
	}
	var payload struct {
		DocID      string `json:"doc_id"`
		DocumentID string `json:"document_id"`
	}
	if err := json.Unmarshal(details, &payload); err != nil {
		return ""
	}
	if strings.TrimSpace(payload.DocID) != "" {
		return strings.TrimSpace(payload.DocID)
	}
	return strings.TrimSpace(payload.DocumentID)
}

func buildHeaderAndFallback(actor, verb, targetURL, identifier, title string) (header string, fallback string) {
	escActor := escapeSlackMrkdwn(actor)
	escTitle := escapeSlackMrkdwn(title)
	escID := escapeSlackMrkdwn(identifier)

	switch {
	case identifier != "" && title != "":
		fallback = fmt.Sprintf("%s %s %s — %s", actor, verb, identifier, title)
		if targetURL != "" {
			header = fmt.Sprintf("*%s* %s *<%s|%s>* — %s", escActor, verb, escapeSlackURL(targetURL), escID, escTitle)
		} else {
			header = fmt.Sprintf("*%s* %s *%s* — %s", escActor, verb, escID, escTitle)
		}
	case identifier != "":
		fallback = fmt.Sprintf("%s %s %s", actor, verb, identifier)
		if targetURL != "" {
			header = fmt.Sprintf("*%s* %s *<%s|%s>*", escActor, verb, escapeSlackURL(targetURL), escID)
		} else {
			header = fmt.Sprintf("*%s* %s *%s*", escActor, verb, escID)
		}
	case title != "":
		fallback = fmt.Sprintf("%s %s %s", actor, verb, title)
		if targetURL != "" {
			header = fmt.Sprintf("*%s* %s *<%s|%s>*", escActor, verb, escapeSlackURL(targetURL), escTitle)
		} else {
			header = fmt.Sprintf("*%s* %s %s", escActor, verb, escTitle)
		}
	default:
		fallback = fmt.Sprintf("%s %s Multica", actor, verb)
		if targetURL != "" {
			header = fmt.Sprintf("*%s* %s *<%s|Multica>*", escActor, verb, escapeSlackURL(targetURL))
		} else {
			header = fmt.Sprintf("*%s* %s Multica", escActor, verb)
		}
	}
	return header, fallback
}

func formatBodyQuote(raw string) string {
	cleaned := cleanNotificationMarkdown(raw)
	compact := strings.Join(strings.Fields(cleaned), " ")
	if compact == "" {
		return ""
	}
	truncated := truncateRunes(compact, maxBodyQuoteRunes)
	return "> " + escapeSlackMrkdwn(truncated)
}

func cleanNotificationMarkdown(raw string) string {
	s := strings.ReplaceAll(raw, "\r\n", "\n")
	s = reCodeFence.ReplaceAllString(s, "")
	s = reMentionLink.ReplaceAllStringFunc(s, func(match string) string {
		parts := reMentionLink.FindStringSubmatch(match)
		if len(parts) < 3 {
			return match
		}
		label := strings.TrimSpace(parts[1])
		kind := strings.ToLower(strings.TrimSpace(parts[2]))
		if label == "" {
			return ""
		}
		switch kind {
		case "member", "agent", "squad":
			if !strings.HasPrefix(label, "@") {
				return "@" + label
			}
			return label
		default:
			return label
		}
	})
	s = reMarkdownImage.ReplaceAllString(s, "$1")
	s = reMarkdownLink.ReplaceAllString(s, "$1")
	s = strings.NewReplacer("**", "", "__", "").Replace(s)
	return s
}

func truncateRunes(s string, maxRunes int) string {
	if maxRunes <= 0 || utf8.RuneCountInString(s) <= maxRunes {
		return s
	}
	runes := []rune(s)
	if maxRunes <= 3 {
		return string(runes[:maxRunes])
	}
	return strings.TrimSpace(string(runes[:maxRunes-3])) + "..."
}

func buildContextLine(item multica.InboxItem, meta multica.WorkspaceMeta) string {
	var parts []string
	if item.IssueProjectID != "" {
		if p, ok := meta.ProjectsByID[item.IssueProjectID]; ok && strings.TrimSpace(p.Name) != "" {
			parts = append(parts, escapeSlackMrkdwn(strings.TrimSpace(p.Name)))
		}
	}
	if status := resolveStatusName(item.IssueStatus, meta); status != "" {
		parts = append(parts, escapeSlackMrkdwn(status))
	}
	if prio := strings.TrimSpace(item.IssuePriority); prio != "" && !strings.EqualFold(prio, "none") {
		parts = append(parts, escapeSlackMrkdwn(humanizeToken(prio)))
	}
	return strings.Join(parts, " · ")
}

func resolveStatusName(statusKey string, meta multica.WorkspaceMeta) string {
	trimmed := strings.TrimSpace(statusKey)
	if trimmed == "" {
		return ""
	}
	if st, ok := meta.StatusesByKey[trimmed]; ok && strings.TrimSpace(st.Name) != "" {
		return strings.TrimSpace(st.Name)
	}
	return humanizeToken(trimmed)
}

func escapeSlackMrkdwn(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}

func escapeSlackURL(rawURL string) string {
	return strings.NewReplacer("&", "&amp;", ">", "%3E", "|", "%7C").Replace(rawURL)
}

func humanizeToken(s string) string {
	parts := strings.FieldsFunc(strings.TrimSpace(s), func(r rune) bool {
		return r == '_' || r == '-' || r == ' '
	})
	for i, part := range parts {
		if part == "" {
			continue
		}
		lower := strings.ToLower(part)
		r, size := utf8.DecodeRuneInString(lower)
		if r == utf8.RuneError && size == 0 {
			continue
		}
		parts[i] = strings.ToUpper(string(r)) + lower[size:]
	}
	return strings.Join(parts, " ")
}
