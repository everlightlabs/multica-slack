package prefs

import (
	"testing"
)

func TestAllGroupsAndDefaults(t *testing.T) {
	if len(AllGroups) != 9 {
		t.Fatalf("expected 9 preference groups, got %d", len(AllGroups))
	}

	defs := Default()
	if len(defs) != 9 {
		t.Fatalf("expected 9 default entries, got %d", len(defs))
	}

	wantDefaults := map[Group]bool{
		GroupMentions:      true,
		GroupAssignments:   true,
		GroupComments:      true,
		GroupStatusChanges: true,
		GroupAgentActivity: true,
		GroupSystemAlerts:  true,
		GroupUpdates:       false,
		GroupNewIssues:     false,
		GroupReactions:     false,
	}

	for g, want := range wantDefaults {
		got, ok := defs[g]
		if !ok {
			t.Fatalf("missing default group %q", g)
		}
		if got != want {
			t.Fatalf("default[%q] = %v, want %v", g, got, want)
		}
	}

	for _, meta := range AllGroups {
		if meta.Label == "" {
			t.Fatalf("group %q has empty label", meta.Key)
		}
		if meta.DefaultEnabled != wantDefaults[meta.Key] {
			t.Fatalf("AllGroups[%q].DefaultEnabled = %v, want %v", meta.Key, meta.DefaultEnabled, wantDefaults[meta.Key])
		}
	}
}

func TestGroupForTypeExactMapping(t *testing.T) {
	cases := map[string]Group{
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
		"app_action_requested":         GroupSystemAlerts,
		"app_action_reminder":          GroupSystemAlerts,
		"app_connect_requested":        GroupSystemAlerts,
		"children_done":                GroupSystemAlerts,
		"blockers_done":                GroupSystemAlerts,
		"autopilot_paused":             GroupSystemAlerts,
		"autopilot_quota_exceeded":     GroupSystemAlerts,
		"app_tool_calls_quota_exceeded": GroupSystemAlerts,
		"issue_subscribed":             GroupSystemAlerts,
		"quick_create_failed":          GroupSystemAlerts,
		"quick_create_done":            GroupSystemAlerts,
		"quick_create_unconfirmed":     GroupSystemAlerts,
		"project_owner_joined":         GroupSystemAlerts,
		// updates
		"priority_changed":   GroupUpdates,
		"start_date_changed": GroupUpdates,
		"due_date_changed":   GroupUpdates,
		// new_issues
		"issue_created": GroupNewIssues,
		// reactions
		"reaction_added": GroupReactions,
	}

	for inboxType, want := range cases {
		if got := GroupForType(inboxType); got != want {
			t.Errorf("GroupForType(%q) = %q, want %q", inboxType, got, want)
		}
	}
}

func TestUnknownFutureTypeMapsToSystemAlertsAndDeliversByDefault(t *testing.T) {
	if got := GroupForType("future_alert"); got != GroupSystemAlerts {
		t.Fatalf("GroupForType(future_alert) = %q, want %q", got, GroupSystemAlerts)
	}
	if !ShouldDeliver("future_alert", Default()) {
		t.Fatal("expected unknown future_alert to deliver under Default()")
	}

	muted := Default()
	muted[GroupSystemAlerts] = false
	if ShouldDeliver("future_alert", muted) {
		t.Fatal("expected unknown future_alert to respect muted system_alerts")
	}
}

func TestParseFillsMissingKeysWithDefaults(t *testing.T) {
	parsed := Parse(`{"mentions":false,"reactions":true}`)
	if parsed[GroupMentions] != false {
		t.Fatalf("expected mentions=false, got %v", parsed[GroupMentions])
	}
	if parsed[GroupReactions] != true {
		t.Fatalf("expected reactions=true, got %v", parsed[GroupReactions])
	}
	if parsed[GroupAssignments] != true {
		t.Fatalf("expected missing assignments to default to true")
	}
	if parsed[GroupUpdates] != false {
		t.Fatalf("expected missing updates to default to false")
	}

	// Empty or malformed JSON falls back safely to defaults.
	if got := Parse(""); !got[GroupMentions] || got[GroupReactions] {
		t.Fatalf("Parse(\"\") did not return defaults: %+v", got)
	}
	if got := Parse("{not-json"); !got[GroupMentions] || got[GroupReactions] {
		t.Fatalf("Parse(invalid) did not return defaults: %+v", got)
	}
}

func TestShouldDeliver(t *testing.T) {
	p := Default()
	if !ShouldDeliver("mentioned", p) {
		t.Fatal("expected mentioned to deliver by default")
	}
	if ShouldDeliver("reaction_added", p) {
		t.Fatal("expected reaction_added to be muted by default")
	}
	if ShouldDeliver("priority_changed", p) {
		t.Fatal("expected priority_changed to be muted by default")
	}
	if ShouldDeliver("issue_created", p) {
		t.Fatal("expected issue_created to be muted by default")
	}

	p[GroupReactions] = true
	if !ShouldDeliver("reaction_added", p) {
		t.Fatal("expected reaction_added to deliver when reactions enabled")
	}

	// Nil map falls back to defaults.
	if !ShouldDeliver("mentioned", nil) {
		t.Fatal("expected nil prefs to fall back to default for mentioned")
	}
	if ShouldDeliver("reaction_added", nil) {
		t.Fatal("expected nil prefs to fall back to default for reaction_added")
	}
}
