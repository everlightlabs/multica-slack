package unfurl

import (
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/everlightlabs/multica-slack/internal/multica"
	"github.com/slack-go/slack"
)

const (
	colorUnstarted  = "#94A3B8"
	colorInProgress = "#3B82F6"
	colorWarning    = "#F59E0B"
	colorDone       = "#22C55E"
	colorCancelled  = "#6B7280"
	colorProject    = "#6366F1"
	colorDoc        = "#8B5CF6"

	maxSnippetRunes = 200
	maxCardLabels   = 3
)

var (
	reCodeFence      = regexp.MustCompile("(?m)^```[^\\n]*\\n?")
	reMarkdownImage  = regexp.MustCompile(`!\[([^\]]*)\]\([^)]+\)`)
	reMarkdownLink   = regexp.MustCompile(`\[([^\]]+)\]\([^)]+\)`)
	reLinePrefix     = regexp.MustCompile(`(?m)^(?:#{1,6}\s+|>\s*|[-*+]\s+|\d+\.\s+)`)
	reHorizontalRule = regexp.MustCompile(`(?m)^(?:---|\*\*\*|___)\s*$`)
	reItalicUnder    = regexp.MustCompile(`(^|[\s(\["'])_([^_\n]+)_($|[\s.,!?;:)\]"'])`)
	reItalicStar     = regexp.MustCompile(`(^|[\s(\["'])\*([^*\n]+)\*($|[\s.,!?;:)\]"'])`)
)

// FormatIssueCard builds a GitHub-style Slack Attachment for a Multica issue.
func FormatIssueCard(rawURL string, issue multica.Issue, meta multica.WorkspaceMeta, now time.Time) slack.Attachment {
	titleText := strings.TrimSpace(issue.Title)
	if issue.Identifier != "" && titleText != "" {
		titleText = issue.Identifier + " — " + titleText
	} else if issue.Identifier != "" {
		titleText = issue.Identifier
	}
	if titleText == "" {
		titleText = "Multica Issue"
	}

	statusLabel := resolveIssueStatusLabel(issue, meta)
	blocks := []slack.Block{
		slack.NewSectionBlock(
			slack.NewTextBlockObject(slack.MarkdownType, formatLinkedTitle(rawURL, titleText), false, false),
			nil,
			nil,
		),
	}

	if snippet := StripAndTruncateMarkdown(issue.Description, maxSnippetRunes); snippet != "" {
		blocks = append(blocks, slack.NewSectionBlock(
			slack.NewTextBlockObject(slack.MarkdownType, escapeSlackMrkdwn(snippet), false, false),
			nil,
			nil,
		))
	}

	fields := []*slack.TextBlockObject{
		mrkdwnField("Status", statusLabel),
		mrkdwnField("Assignee", resolveIssueAssignee(issue, meta)),
		mrkdwnField("Priority", formatPriority(issue.Priority)),
		mrkdwnField("Project", resolveIssueProject(issue, meta)),
	}
	blocks = append(blocks, slack.NewSectionBlock(nil, fields, nil))

	footerParts := []string{"Updated " + FormatRelativeTime(issue.UpdatedAt, now)}
	if labels := collectLabels(issue.Labels, maxCardLabels); len(labels) > 0 {
		footerParts = append(footerParts, strings.Join(labels, ", "))
	}
	blocks = append(blocks, slack.NewContextBlock(
		"",
		slack.NewTextBlockObject(slack.MarkdownType, strings.Join(footerParts, " · "), false, false),
	))

	fallback := titleText
	if statusLabel != "" {
		fallback = fmt.Sprintf("%s (%s)", titleText, statusLabel)
	}

	return slack.Attachment{
		Color:    resolveIssueStatusColor(issue, meta),
		Fallback: fallback,
		Blocks:   slack.Blocks{BlockSet: blocks},
	}
}

// FormatProjectCard builds a GitHub-style Slack Attachment for a Multica project.
func FormatProjectCard(rawURL string, project multica.Project, progress *multica.ProjectProgress, now time.Time) slack.Attachment {
	titleText := strings.TrimSpace(project.Name)
	if titleText == "" {
		titleText = "Multica Project"
	}

	blocks := []slack.Block{
		slack.NewSectionBlock(
			slack.NewTextBlockObject(slack.MarkdownType, formatLinkedTitle(rawURL, titleText), false, false),
			nil,
			nil,
		),
	}

	if snippet := StripAndTruncateMarkdown(project.Description, maxSnippetRunes); snippet != "" {
		blocks = append(blocks, slack.NewSectionBlock(
			slack.NewTextBlockObject(slack.MarkdownType, escapeSlackMrkdwn(snippet), false, false),
			nil,
			nil,
		))
	}

	statusDisplay := "Active"
	if strings.TrimSpace(project.Status) != "" {
		statusDisplay = humanizeToken(project.Status)
	}
	progressDisplay := "—"
	if progress != nil {
		if progress.Total > 0 {
			pct := (progress.Completed * 100) / progress.Total
			progressDisplay = fmt.Sprintf("%d/%d issues (%d%%)", progress.Completed, progress.Total, pct)
		} else {
			progressDisplay = fmt.Sprintf("%d/%d issues", progress.Completed, progress.Total)
		}
	}

	blocks = append(blocks, slack.NewSectionBlock(nil, []*slack.TextBlockObject{
		mrkdwnField("Status", statusDisplay),
		mrkdwnField("Progress", progressDisplay),
	}, nil))

	if !project.UpdatedAt.IsZero() {
		blocks = append(blocks, slack.NewContextBlock(
			"",
			slack.NewTextBlockObject(slack.MarkdownType, "Updated "+FormatRelativeTime(project.UpdatedAt, now), false, false),
		))
	}

	return slack.Attachment{
		Color:    colorProject,
		Fallback: "Project: " + titleText,
		Blocks:   slack.Blocks{BlockSet: blocks},
	}
}

// FormatDocCard builds a GitHub-style Slack Attachment for a Multica doc.
func FormatDocCard(rawURL string, doc multica.Doc, now time.Time) slack.Attachment {
	titleText := strings.TrimSpace(doc.Title)
	if titleText == "" {
		titleText = "Multica Doc"
	}

	blocks := []slack.Block{
		slack.NewSectionBlock(
			slack.NewTextBlockObject(slack.MarkdownType, formatLinkedTitle(rawURL, titleText), false, false),
			nil,
			nil,
		),
	}

	if snippet := StripAndTruncateMarkdown(doc.Content, maxSnippetRunes); snippet != "" {
		blocks = append(blocks, slack.NewSectionBlock(
			slack.NewTextBlockObject(slack.MarkdownType, escapeSlackMrkdwn(snippet), false, false),
			nil,
			nil,
		))
	}

	if !doc.UpdatedAt.IsZero() {
		blocks = append(blocks, slack.NewContextBlock(
			"",
			slack.NewTextBlockObject(slack.MarkdownType, "Updated "+FormatRelativeTime(doc.UpdatedAt, now), false, false),
		))
	}

	return slack.Attachment{
		Color:    colorDoc,
		Fallback: "Doc: " + titleText,
		Blocks:   slack.Blocks{BlockSet: blocks},
	}
}

// StripAndTruncateMarkdown converts Markdown text to compact single-line plain text
// bounded to at most maxRunes runes.
func StripAndTruncateMarkdown(s string, maxRunes int) string {
	if strings.TrimSpace(s) == "" || maxRunes <= 0 {
		return ""
	}

	clean := strings.ReplaceAll(s, "\r\n", "\n")
	clean = reCodeFence.ReplaceAllString(clean, "")
	clean = reHorizontalRule.ReplaceAllString(clean, "")
	clean = reMarkdownImage.ReplaceAllString(clean, "$1")
	clean = reMarkdownLink.ReplaceAllString(clean, "$1")
	clean = reLinePrefix.ReplaceAllString(clean, "")
	clean = strings.NewReplacer("**", "", "__", "", "~~", "", "`", "").Replace(clean)
	clean = reItalicUnder.ReplaceAllString(clean, "${1}${2}${3}")
	clean = reItalicStar.ReplaceAllString(clean, "${1}${2}${3}")

	clean = strings.Join(strings.Fields(clean), " ")
	if clean == "" {
		return ""
	}

	if utf8.RuneCountInString(clean) <= maxRunes {
		return clean
	}

	runes := []rune(clean)
	if maxRunes <= 3 {
		return string(runes[:maxRunes])
	}
	trimmed := strings.TrimSpace(string(runes[:maxRunes-3]))
	return trimmed + "..."
}

// FormatRelativeTime formats t relative to now in compact human-readable form.
func FormatRelativeTime(t, now time.Time) string {
	if t.IsZero() {
		return "recently"
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}

	d := now.Sub(t)
	if d < time.Minute {
		return "just now"
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	}
	if d < 24*time.Hour {
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}
	if d < 30*24*time.Hour {
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
	return t.UTC().Format("Jan 2, 2006")
}

func resolveIssueStatusColor(issue multica.Issue, meta multica.WorkspaceMeta) string {
	statusKey := strings.ToLower(strings.TrimSpace(issue.Status))
	category := strings.ToLower(strings.TrimSpace(issue.StatusCategory))
	if category == "" {
		if st, ok := meta.StatusesByKey[issue.Status]; ok {
			category = strings.ToLower(strings.TrimSpace(st.Category))
		}
	}

	switch {
	case statusKey == "cancelled" || statusKey == "canceled" || category == "cancelled" || category == "canceled":
		return colorCancelled
	case statusKey == "done" || statusKey == "closed" || statusKey == "completed" || category == "completed" || category == "done":
		return colorDone
	case statusKey == "in_review" || statusKey == "blocked" || statusKey == "handoff" ||
		strings.Contains(statusKey, "review") || strings.Contains(statusKey, "blocked") || strings.Contains(statusKey, "handoff"):
		return colorWarning
	case statusKey == "in_progress":
		return colorInProgress
	case statusKey == "backlog" || statusKey == "todo" || category == "unstarted" || category == "backlog":
		return colorUnstarted
	case category == "started":
		return colorWarning
	default:
		return colorUnstarted
	}
}

func resolveIssueStatusLabel(issue multica.Issue, meta multica.WorkspaceMeta) string {
	if name := strings.TrimSpace(issue.StatusName); name != "" {
		return name
	}
	if st, ok := meta.StatusesByKey[issue.Status]; ok && strings.TrimSpace(st.Name) != "" {
		return strings.TrimSpace(st.Name)
	}
	if strings.TrimSpace(issue.Status) != "" {
		return humanizeToken(issue.Status)
	}
	return "Unknown"
}

func resolveIssueAssignee(issue multica.Issue, meta multica.WorkspaceMeta) string {
	id := strings.TrimSpace(issue.AssigneeID)
	if id == "" {
		return "Unassigned"
	}

	switch strings.ToLower(strings.TrimSpace(issue.AssigneeType)) {
	case "agent":
		if a, ok := meta.AgentsByID[id]; ok && a.Name != "" {
			return a.Name
		}
	case "squad":
		if sq, ok := meta.SquadsByID[id]; ok && sq.Name != "" {
			return sq.Name
		}
	default:
		if m, ok := meta.MembersByUserID[id]; ok && m.Name != "" {
			return m.Name
		}
		if a, ok := meta.AgentsByID[id]; ok && a.Name != "" {
			return a.Name
		}
		if sq, ok := meta.SquadsByID[id]; ok && sq.Name != "" {
			return sq.Name
		}
	}
	return "Assigned"
}

func resolveIssueProject(issue multica.Issue, meta multica.WorkspaceMeta) string {
	if issue.ProjectID == "" {
		return "None"
	}
	if p, ok := meta.ProjectsByID[issue.ProjectID]; ok && strings.TrimSpace(p.Name) != "" {
		return strings.TrimSpace(p.Name)
	}
	return "None"
}

func formatPriority(priority string) string {
	trimmed := strings.TrimSpace(priority)
	if trimmed == "" {
		return "None"
	}
	return humanizeToken(trimmed)
}

func collectLabels(labels []multica.Label, limit int) []string {
	if len(labels) == 0 || limit <= 0 {
		return nil
	}
	out := make([]string, 0, min(len(labels), limit))
	for _, lbl := range labels {
		name := strings.TrimSpace(lbl.Name)
		if name == "" {
			continue
		}
		out = append(out, escapeSlackMrkdwn(name))
		if len(out) >= limit {
			break
		}
	}
	return out
}

func mrkdwnField(label, value string) *slack.TextBlockObject {
	return slack.NewTextBlockObject(
		slack.MarkdownType,
		fmt.Sprintf("*%s*\n%s", escapeSlackMrkdwn(label), escapeSlackMrkdwn(value)),
		false,
		false,
	)
}

func formatLinkedTitle(rawURL, title string) string {
	safeURL := strings.NewReplacer("&", "&amp;", ">", "%3E", "|", "%7C").Replace(rawURL)
	return fmt.Sprintf("*<%s|%s>*", safeURL, escapeSlackMrkdwn(title))
}

func escapeSlackMrkdwn(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
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
