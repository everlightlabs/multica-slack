// Package prefs defines Multica inbox notification preference groups and
// maps Multica inbox event types 1:1 to user-configurable Slack toggles.
package prefs

import (
	"encoding/json"
	"strings"
)

// Group identifies a user-toggleable notification preference category.
type Group string

const (
	GroupMentions      Group = "mentions"
	GroupAssignments   Group = "assignments"
	GroupComments      Group = "comments"
	GroupStatusChanges Group = "status_changes"
	GroupAgentActivity Group = "agent_activity"
	GroupSystemAlerts  Group = "system_alerts"
	GroupUpdates       Group = "updates"
	GroupNewIssues     Group = "new_issues"
	GroupReactions     Group = "reactions"
)

// GroupMeta describes a preference group for UI rendering and default initialization.
type GroupMeta struct {
	Key            Group
	Label          string
	DefaultEnabled bool
}

// AllGroups lists all preference groups in App Home display order.
var AllGroups = []GroupMeta{
	{Key: GroupMentions, Label: "Mentions", DefaultEnabled: true},
	{Key: GroupAssignments, Label: "Assignments", DefaultEnabled: true},
	{Key: GroupComments, Label: "Comments", DefaultEnabled: true},
	{Key: GroupStatusChanges, Label: "Status changes", DefaultEnabled: true},
	{Key: GroupAgentActivity, Label: "Agent activity & failures", DefaultEnabled: true},
	{Key: GroupSystemAlerts, Label: "Action-required & workspace alerts", DefaultEnabled: true},
	{Key: GroupUpdates, Label: "Issue field updates", DefaultEnabled: false},
	{Key: GroupNewIssues, Label: "New issues", DefaultEnabled: false},
	{Key: GroupReactions, Label: "Emoji reactions", DefaultEnabled: false},
}

var defaultByGroup = func() map[Group]bool {
	m := make(map[Group]bool, len(AllGroups))
	for _, g := range AllGroups {
		m[g.Key] = g.DefaultEnabled
	}
	return m
}()

var typeToGroup = map[string]Group{
	// mentions
	"mentioned":     GroupMentions,
	"doc_mentioned": GroupMentions,
	// assignments
	"issue_assigned":   GroupAssignments,
	"unassigned":       GroupAssignments,
	"assignee_changed": GroupAssignments,
	// comments
	"new_comment": GroupComments,
	"doc_comment": GroupComments,
	// status_changes
	"status_changed": GroupStatusChanges,
	// agent_activity
	"task_failed":       GroupAgentActivity,
	"app_action_result": GroupAgentActivity,
	// system_alerts
	"app_action_requested":          GroupSystemAlerts,
	"app_action_reminder":           GroupSystemAlerts,
	"app_connect_requested":         GroupSystemAlerts,
	"children_done":                 GroupSystemAlerts,
	"blockers_done":                 GroupSystemAlerts,
	"autopilot_paused":              GroupSystemAlerts,
	"autopilot_quota_exceeded":      GroupSystemAlerts,
	"app_tool_calls_quota_exceeded": GroupSystemAlerts,
	"issue_subscribed":              GroupSystemAlerts,
	"quick_create_failed":           GroupSystemAlerts,
	"quick_create_done":             GroupSystemAlerts,
	"quick_create_unconfirmed":      GroupSystemAlerts,
	"project_owner_joined":          GroupSystemAlerts,
	// updates
	"priority_changed":   GroupUpdates,
	"start_date_changed": GroupUpdates,
	"due_date_changed":   GroupUpdates,
	// new_issues
	"issue_created": GroupNewIssues,
	// reactions
	"reaction_added": GroupReactions,
}

// Preferences maps each Group to whether Slack DM delivery is enabled.
type Preferences map[Group]bool

// Default returns a fresh Preferences map initialized with default group settings.
func Default() Preferences {
	out := make(Preferences, len(AllGroups))
	for _, g := range AllGroups {
		out[g.Key] = g.DefaultEnabled
	}
	return out
}

// Parse decodes a JSON preference object and fills any missing groups with defaults.
// Empty or invalid JSON falls back to Default().
func Parse(raw string) Preferences {
	out := Default()
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return out
	}

	var decoded map[Group]bool
	if err := json.Unmarshal([]byte(trimmed), &decoded); err != nil {
		return out
	}
	for _, g := range AllGroups {
		if v, ok := decoded[g.Key]; ok {
			out[g.Key] = v
		}
	}
	return out
}

// GroupForType maps a Multica inbox event type to its preference Group.
// Unknown future types map to GroupSystemAlerts so new alerts are not silently swallowed.
func GroupForType(inboxType string) Group {
	if g, ok := typeToGroup[inboxType]; ok {
		return g
	}
	return GroupSystemAlerts
}

// ShouldDeliver reports whether an inbox event of inboxType should be delivered under p.
func ShouldDeliver(inboxType string, p Preferences) bool {
	g := GroupForType(inboxType)
	if p != nil {
		if enabled, ok := p[g]; ok {
			return enabled
		}
	}
	return defaultByGroup[g]
}
