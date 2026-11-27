package unfurl

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/everlightlabs/multica-slack/internal/crypto"
	"github.com/everlightlabs/multica-slack/internal/multica"
	"github.com/everlightlabs/multica-slack/internal/store"
	"github.com/slack-go/slack"
)

func openTestStore(t *testing.T) *store.Store {
	t.Helper()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatalf("generate key: %v", err)
	}
	box, err := crypto.NewBox(key)
	if err != nil {
		t.Fatalf("new box: %v", err)
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"), box)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})
	return st
}

type slackUnfurlCall struct {
	Method   string
	Token    string
	UnfurlID string
	Source   string
	Channel  string
	TS       string
	Unfurls  map[string]slack.Attachment
}

type mockSlackUnfurler struct {
	mu    sync.Mutex
	calls []slackUnfurlCall
	errFn func(call slackUnfurlCall) error
}

func (m *mockSlackUnfurler) UnfurlByID(_ context.Context, token, unfurlID, source string, unfurls map[string]slack.Attachment) error {
	call := slackUnfurlCall{
		Method:   "by_id",
		Token:    token,
		UnfurlID: unfurlID,
		Source:   source,
		Unfurls:  unfurls,
	}
	m.mu.Lock()
	m.calls = append(m.calls, call)
	fn := m.errFn
	m.mu.Unlock()
	if fn != nil {
		return fn(call)
	}
	return nil
}

func (m *mockSlackUnfurler) UnfurlByTS(_ context.Context, token, channel, ts string, unfurls map[string]slack.Attachment) error {
	call := slackUnfurlCall{
		Method:  "by_ts",
		Token:   token,
		Channel: channel,
		TS:      ts,
		Unfurls: unfurls,
	}
	m.mu.Lock()
	m.calls = append(m.calls, call)
	fn := m.errFn
	m.mu.Unlock()
	if fn != nil {
		return fn(call)
	}
	return nil
}

func (m *mockSlackUnfurler) snapshot() []slackUnfurlCall {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]slackUnfurlCall, len(m.calls))
	copy(out, m.calls)
	return out
}

func TestMatchURL(t *testing.T) {
	tests := []struct {
		name      string
		appURL    string
		rawURL    string
		wantOK    bool
		wantMatch Match
	}{
		{
			name:   "issue identifier",
			appURL: "https://multica.ai",
			rawURL: "https://multica.ai/acme/issues/ACME-42",
			wantOK: true,
			wantMatch: Match{
				RawURL:        "https://multica.ai/acme/issues/ACME-42",
				Kind:          KindIssue,
				WorkspaceSlug: "acme",
				ResourceID:    "ACME-42",
			},
		},
		{
			name:   "issue with query and fragment and trailing slash base",
			appURL: "https://multica.ai/",
			rawURL: "https://multica.ai/acme/issues/ACME-42?comment=abc#comment-1",
			wantOK: true,
			wantMatch: Match{
				RawURL:        "https://multica.ai/acme/issues/ACME-42?comment=abc#comment-1",
				Kind:          KindIssue,
				WorkspaceSlug: "acme",
				ResourceID:    "ACME-42",
			},
		},
		{
			name:   "project uuid",
			appURL: "",
			rawURL: "https://multica.ai/acme/projects/11111111-2222-3333-4444-555555555555",
			wantOK: true,
			wantMatch: Match{
				RawURL:        "https://multica.ai/acme/projects/11111111-2222-3333-4444-555555555555",
				Kind:          KindProject,
				WorkspaceSlug: "acme",
				ResourceID:    "11111111-2222-3333-4444-555555555555",
			},
		},
		{
			name:   "doc uuid with trailing slash",
			appURL: "https://multica.ai",
			rawURL: "https://multica.ai/acme/docs/aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee/",
			wantOK: true,
			wantMatch: Match{
				RawURL:        "https://multica.ai/acme/docs/aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee/",
				Kind:          KindDoc,
				WorkspaceSlug: "acme",
				ResourceID:    "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
			},
		},
		{
			name:   "marketing root ignored",
			appURL: "https://multica.ai",
			rawURL: "https://multica.ai",
			wantOK: false,
		},
		{
			name:   "settings path ignored",
			appURL: "https://multica.ai",
			rawURL: "https://multica.ai/acme/settings",
			wantOK: false,
		},
		{
			name:   "login path ignored",
			appURL: "https://multica.ai",
			rawURL: "https://multica.ai/login",
			wantOK: false,
		},
		{
			name:   "non-matching host ignored",
			appURL: "https://multica.ai",
			rawURL: "https://evil.example.com/acme/issues/ACME-42",
			wantOK: false,
		},
		{
			name:   "extra nested path ignored",
			appURL: "https://multica.ai",
			rawURL: "https://multica.ai/acme/issues/ACME-42/activity",
			wantOK: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := MatchURL(tc.appURL, tc.rawURL)
			if ok != tc.wantOK {
				t.Fatalf("MatchURL(%q, %q) ok = %v, want %v", tc.appURL, tc.rawURL, ok, tc.wantOK)
			}
			if ok && got != tc.wantMatch {
				t.Fatalf("MatchURL(%q, %q) = %+v, want %+v", tc.appURL, tc.rawURL, got, tc.wantMatch)
			}
		})
	}
}

func TestFormatIssueCard(t *testing.T) {
	now := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	meta := multica.WorkspaceMeta{
		MembersByUserID: map[string]multica.Member{
			"u-1": {ID: "m-1", UserID: "u-1", Name: "Alice Smith"},
		},
		ProjectsByID: map[string]multica.Project{
			"p-1": {ID: "p-1", Name: "Slack Integration"},
		},
		StatusesByKey: map[string]multica.IssueStatus{
			"in_review": {Key: "in_review", Name: "In Review", Category: "started"},
		},
	}

	colorTests := []struct {
		name           string
		status         string
		statusCategory string
		wantColor      string
	}{
		{name: "unstarted", status: "todo", statusCategory: "unstarted", wantColor: "#94A3B8"},
		{name: "in_progress", status: "in_progress", statusCategory: "started", wantColor: "#3B82F6"},
		{name: "in_review", status: "in_review", statusCategory: "started", wantColor: "#F59E0B"},
		{name: "blocked", status: "blocked", statusCategory: "started", wantColor: "#F59E0B"},
		{name: "custom started handoff", status: "qa_handoff", statusCategory: "started", wantColor: "#F59E0B"},
		{name: "done", status: "done", statusCategory: "completed", wantColor: "#22C55E"},
		{name: "cancelled", status: "cancelled", statusCategory: "cancelled", wantColor: "#6B7280"},
	}

	for _, tc := range colorTests {
		t.Run("color_"+tc.name, func(t *testing.T) {
			att := FormatIssueCard("https://multica.ai/acme/issues/ACME-42", multica.Issue{
				Identifier:     "ACME-42",
				Title:          "Ship unfurler",
				Status:         tc.status,
				StatusCategory: tc.statusCategory,
				UpdatedAt:      now.Add(-5 * time.Minute),
			}, meta, now)
			if att.Color != tc.wantColor {
				t.Fatalf("Color = %q, want %q", att.Color, tc.wantColor)
			}
		})
	}

	longMarkdown := "## Overview\nThis **bold** _italic_ `code` [link text](https://example.com) has > quoted & <special> text and goes on for quite a long time so that we verify truncation happens cleanly at or under two hundred characters total for Slack cards."
	att := FormatIssueCard("https://multica.ai/acme/issues/ACME-42", multica.Issue{
		Identifier:     "ACME-42",
		Title:          "Build <Slack> & Unfurl",
		Description:    longMarkdown,
		Status:         "in_review",
		StatusCategory: "started",
		Priority:       "high",
		ProjectID:      "p-1",
		AssigneeType:   "member",
		AssigneeID:     "u-1",
		Labels: []multica.Label{
			{Name: "backend"},
			{Name: "slack"},
			{Name: "v1"},
			{Name: "extra-fourth"},
		},
		UpdatedAt: now.Add(-15 * time.Minute),
	}, meta, now)

	if att.Fallback == "" {
		t.Fatal("expected non-empty Fallback")
	}

	// Verify blocks structure: title section, description section, fields section, context footer.
	if len(att.Blocks.BlockSet) != 4 {
		t.Fatalf("expected 4 blocks, got %d", len(att.Blocks.BlockSet))
	}

	titleSec, ok := att.Blocks.BlockSet[0].(*slack.SectionBlock)
	if !ok || titleSec.Text == nil {
		t.Fatalf("expected title section block at index 0, got %T", att.Blocks.BlockSet[0])
	}
	if !strings.Contains(titleSec.Text.Text, "<https://multica.ai/acme/issues/ACME-42|ACME-42 — Build &lt;Slack&gt; &amp; Unfurl>") {
		t.Fatalf("unexpected linked title text: %q", titleSec.Text.Text)
	}

	descSec, ok := att.Blocks.BlockSet[1].(*slack.SectionBlock)
	if !ok || descSec.Text == nil {
		t.Fatalf("expected description section block at index 1, got %T", att.Blocks.BlockSet[1])
	}
	if strings.Contains(descSec.Text.Text, "**") || strings.Contains(descSec.Text.Text, "##") || strings.Contains(descSec.Text.Text, "](https://") {
		t.Fatalf("expected markdown stripped from description, got %q", descSec.Text.Text)
	}

	// Empty description should be omitted (3 blocks instead of 4).
	emptyDescAtt := FormatIssueCard("https://multica.ai/acme/issues/ACME-3582", multica.Issue{
		Identifier:     "ACME-3582",
		Title:          "No description",
		Status:         "todo",
		StatusCategory: "unstarted",
		UpdatedAt:      now,
	}, meta, now)
	if len(emptyDescAtt.Blocks.BlockSet) != 3 {
		t.Fatalf("expected 3 blocks when description is empty, got %d", len(emptyDescAtt.Blocks.BlockSet))
	}

	fieldsSec, ok := att.Blocks.BlockSet[2].(*slack.SectionBlock)
	if !ok || len(fieldsSec.Fields) != 4 {
		t.Fatalf("expected 4 fields in fields section, got %d", len(fieldsSec.Fields))
	}
	joinedFields := fieldsSec.Fields[0].Text + "|" + fieldsSec.Fields[1].Text + "|" + fieldsSec.Fields[2].Text + "|" + fieldsSec.Fields[3].Text
	for _, want := range []string{"Status", "In Review", "Assignee", "Alice Smith", "Priority", "High", "Project", "Slack Integration"} {
		if !strings.Contains(joinedFields, want) {
			t.Fatalf("expected %q in fields, got %q", want, joinedFields)
		}
	}

	ctxBlk, ok := att.Blocks.BlockSet[3].(*slack.ContextBlock)
	if !ok || len(ctxBlk.ContextElements.Elements) == 0 {
		t.Fatalf("expected context footer block at index 3, got %T", att.Blocks.BlockSet[3])
	}
	ctxJSON, _ := json.Marshal(ctxBlk)
	ctxStr := string(ctxJSON)
	if !strings.Contains(ctxStr, "Updated 15m ago") {
		t.Fatalf("expected Updated 15m ago in context footer, got %s", ctxStr)
	}
	if strings.Contains(ctxStr, "extra-fourth") {
		t.Fatalf("expected at most 3 labels in context footer, got %s", ctxStr)
	}

	// Plain-text stripped snippet must stay <= 200 runes before Slack entity escaping.
	plainSnippet := StripAndTruncateMarkdown(longMarkdown, 200)
	if len([]rune(plainSnippet)) > 200 {
		t.Fatalf("expected stripped description <= 200 runes, got %d (%q)", len([]rune(plainSnippet)), plainSnippet)
	}
}

func TestFormatProjectAndDocCards(t *testing.T) {
	now := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)

	projAtt := FormatProjectCard("https://multica.ai/acme/projects/p-1", multica.Project{
		ID:          "p-1",
		Name:        "Multica Slack",
		Description: "## Goal\nUnfurl links and stream DM notifications.",
		Status:      "active",
		UpdatedAt:   now.Add(-2 * time.Hour),
	}, &multica.ProjectProgress{
		ProjectID: "p-1",
		Total:     8,
		Completed: 5,
	}, now)
	if projAtt.Color != "#6366F1" {
		t.Fatalf("project Color = %q, want #6366F1", projAtt.Color)
	}
	projJSON, _ := json.Marshal(projAtt)
	if !strings.Contains(string(projJSON), "5/8") || !strings.Contains(string(projJSON), "Updated 2h ago") {
		t.Fatalf("unexpected project card JSON: %s", string(projJSON))
	}

	docAtt := FormatDocCard("https://multica.ai/acme/docs/d-1", multica.Doc{
		ID:        "d-1",
		Title:     "Runbook",
		Content:   "# Runbook\nRestart the worker with `docker compose restart`.",
		UpdatedAt: now.Add(-48 * time.Hour),
	}, now)
	if docAtt.Color != "#8B5CF6" {
		t.Fatalf("doc Color = %q, want #8B5CF6", docAtt.Color)
	}
	docJSON, _ := json.Marshal(docAtt)
	if !strings.Contains(string(docJSON), "Updated 2d ago") || !strings.Contains(string(docJSON), "Runbook") {
		t.Fatalf("unexpected doc card JSON: %s", string(docJSON))
	}
}

func TestHandleLinkSharedDedupWithin30s(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	if err := st.UpsertMulticaConnection(ctx, store.UpsertMulticaParams{
		SlackUserID:          "U1",
		SlackTeamID:          "T1",
		MulticaUserID:        "mu-1",
		MulticaUserName:      "Alice",
		MulticaWorkspaceID:   "ws-1",
		MulticaWorkspaceSlug: "acme",
		MulticaPAT:           "mul_user_1",
	}); err != nil {
		t.Fatalf("upsert user: %v", err)
	}

	var issueCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/workspaces":
			_ = json.NewEncoder(w).Encode([]multica.Workspace{{ID: "ws-1", Name: "Everlight", Slug: "acme"}})
		case "/api/issues/ACME-42":
			issueCalls.Add(1)
			_ = json.NewEncoder(w).Encode(multica.Issue{
				ID:             "i-1",
				Identifier:     "ACME-42",
				Title:          "Dedup test",
				Status:         "in_progress",
				StatusCategory: "started",
				UpdatedAt:      time.Now().UTC(),
			})
		case "/api/workspaces/ws-1/members", "/api/agents", "/api/squads", "/api/projects", "/api/issue-statuses":
			_, _ = w.Write([]byte("[]"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	mc := multica.NewClient(srv.URL, srv.Client())
	cache := multica.NewMetadataCache(mc, time.Minute)
	slackMock := &mockSlackUnfurler{}
	svc := NewService(ServiceConfig{
		AppBaseURL: "https://multica.ai",
		BotToken:   "xoxb-test",
		DedupTTL:   30 * time.Second,
	}, st, mc, cache, slackMock, nil)

	in := LinkSharedInput{
		Channel:      "C123",
		MessageTS:    "1700000000.000100",
		UnfurlID:     "U123",
		Source:       "conversations_history",
		PosterUserID: "U1",
		Links:        []string{"https://multica.ai/acme/issues/ACME-42"},
	}

	if err := svc.HandleLinkShared(ctx, in); err != nil {
		t.Fatalf("first HandleLinkShared: %v", err)
	}
	if err := svc.HandleLinkShared(ctx, in); err != nil {
		t.Fatalf("second HandleLinkShared: %v", err)
	}

	if got := issueCalls.Load(); got != 1 {
		t.Fatalf("Multica GetIssue called %d times, want 1", got)
	}
	if calls := slackMock.snapshot(); len(calls) != 1 {
		t.Fatalf("Slack chat.unfurl called %d times, want 1", len(calls))
	}
}

func TestHandleLinkSharedDMFallbackPreservesPosterPAT(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()

	// Poster A has Multica PAT only (no Slack xoxp- token).
	if err := st.UpsertMulticaConnection(ctx, store.UpsertMulticaParams{
		SlackUserID:          "U_POSTER_A",
		SlackTeamID:          "T1",
		MulticaUserID:        "mu-a",
		MulticaUserName:      "Alice",
		MulticaWorkspaceID:   "ws-1",
		MulticaWorkspaceSlug: "acme",
		MulticaPAT:           "mul_poster_a_secret",
	}); err != nil {
		t.Fatalf("upsert poster A: %v", err)
	}

	// Recipient B has both a different Multica PAT and a Slack xoxp- token.
	if err := st.UpsertMulticaConnection(ctx, store.UpsertMulticaParams{
		SlackUserID:          "U_RECIP_B",
		SlackTeamID:          "T1",
		MulticaUserID:        "mu-b",
		MulticaUserName:      "Bob",
		MulticaWorkspaceID:   "ws-1",
		MulticaWorkspaceSlug: "acme",
		MulticaPAT:           "mul_recip_b_secret",
	}); err != nil {
		t.Fatalf("upsert recipient B: %v", err)
	}
	if err := st.SetSlackUserToken(ctx, "U_RECIP_B", "T1", "xoxp-recip-b-token"); err != nil {
		t.Fatalf("set slack token B: %v", err)
	}

	var authHeaders []string
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		authHeaders = append(authHeaders, r.Header.Get("Authorization"))
		mu.Unlock()

		switch r.URL.Path {
		case "/api/workspaces":
			_ = json.NewEncoder(w).Encode([]multica.Workspace{{ID: "ws-1", Name: "Everlight", Slug: "acme"}})
		case "/api/issues/ACME-42":
			_ = json.NewEncoder(w).Encode(multica.Issue{
				ID:             "i-1",
				Identifier:     "ACME-42",
				Title:          "Private project issue",
				Status:         "todo",
				StatusCategory: "unstarted",
				UpdatedAt:      time.Now().UTC(),
			})
		case "/api/workspaces/ws-1/members", "/api/agents", "/api/squads", "/api/projects", "/api/issue-statuses":
			_, _ = w.Write([]byte("[]"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	mc := multica.NewClient(srv.URL, srv.Client())
	cache := multica.NewMetadataCache(mc, time.Minute)
	slackMock := &mockSlackUnfurler{
		errFn: func(call slackUnfurlCall) error {
			if call.Token == "xoxb-bot" {
				return slack.SlackErrorResponse{Err: "channel_not_found"}
			}
			return nil
		},
	}

	svc := NewService(ServiceConfig{
		AppBaseURL: "https://multica.ai",
		BotToken:   "xoxb-bot",
	}, st, mc, cache, slackMock, nil)

	err := svc.HandleLinkShared(ctx, LinkSharedInput{
		Channel:           "D999",
		MessageTS:         "1700000001.000200",
		UnfurlID:          "unf-1",
		Source:            "composer",
		PosterUserID:      "U_POSTER_A",
		AuthorizingUserID: "U_RECIP_B",
		Links:             []string{"https://multica.ai/acme/issues/ACME-42"},
	})
	if err != nil {
		t.Fatalf("HandleLinkShared: %v", err)
	}

	calls := slackMock.snapshot()
	if len(calls) != 2 {
		t.Fatalf("expected 2 Slack calls (bot then recipient xoxp-), got %d", len(calls))
	}
	if calls[0].Method != "by_id" || calls[0].Token != "xoxb-bot" {
		t.Fatalf("first call = %+v, want by_id with xoxb-bot", calls[0])
	}
	if calls[1].Method != "by_id" || calls[1].Token != "xoxp-recip-b-token" {
		t.Fatalf("second call = %+v, want by_id with xoxp-recip-b-token", calls[1])
	}

	mu.Lock()
	defer mu.Unlock()
	if len(authHeaders) == 0 {
		t.Fatal("expected Multica API requests")
	}
	for _, h := range authHeaders {
		if h != "Bearer mul_poster_a_secret" {
			t.Fatalf("Multica request used %q, must strictly use poster A's PAT", h)
		}
	}
}

func TestHandleLinkSharedUnlinkedUserAndMax5Cap(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()

	var multicaCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		multicaCalls.Add(1)
		switch {
		case r.URL.Path == "/api/workspaces":
			_ = json.NewEncoder(w).Encode([]multica.Workspace{{ID: "ws-1", Name: "Everlight", Slug: "acme"}})
		case strings.HasPrefix(r.URL.Path, "/api/issues/ACME-"):
			id := strings.TrimPrefix(r.URL.Path, "/api/issues/")
			_ = json.NewEncoder(w).Encode(multica.Issue{
				ID:             id,
				Identifier:     id,
				Title:          "Issue " + id,
				Status:         "todo",
				StatusCategory: "unstarted",
				UpdatedAt:      time.Now().UTC(),
			})
		case r.URL.Path == "/api/workspaces/ws-1/members" ||
			r.URL.Path == "/api/agents" ||
			r.URL.Path == "/api/squads" ||
			r.URL.Path == "/api/projects" ||
			r.URL.Path == "/api/issue-statuses":
			_, _ = w.Write([]byte("[]"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	mc := multica.NewClient(srv.URL, srv.Client())
	cache := multica.NewMetadataCache(mc, time.Minute)
	slackMock := &mockSlackUnfurler{}
	svc := NewService(ServiceConfig{
		AppBaseURL: "https://multica.ai",
		BotToken:   "xoxb-bot",
	}, st, mc, cache, slackMock, nil)

	// Part 1: unlinked poster with empty FallbackPAT skips silently.
	err := svc.HandleLinkShared(ctx, LinkSharedInput{
		Channel:      "C1",
		MessageTS:    "1700000002.000100",
		PosterUserID: "U_UNLINKED",
		Links:        []string{"https://multica.ai/acme/issues/ACME-1"},
	})
	if err != nil {
		t.Fatalf("unlinked HandleLinkShared: %v", err)
	}
	if multicaCalls.Load() != 0 || len(slackMock.snapshot()) != 0 {
		t.Fatalf("expected 0 Multica and 0 Slack calls for unlinked poster, got %d / %d", multicaCalls.Load(), len(slackMock.snapshot()))
	}

	// Part 2: linked poster with 7 issue links fetches at most 5.
	if err := st.UpsertMulticaConnection(ctx, store.UpsertMulticaParams{
		SlackUserID:          "U_LINKED",
		SlackTeamID:          "T1",
		MulticaUserID:        "mu-1",
		MulticaUserName:      "Alice",
		MulticaWorkspaceID:   "ws-1",
		MulticaWorkspaceSlug: "acme",
		MulticaPAT:           "mul_linked",
	}); err != nil {
		t.Fatalf("upsert linked user: %v", err)
	}

	links := []string{
		"https://multica.ai/acme/issues/ACME-1",
		"https://multica.ai/acme/issues/ACME-2",
		"https://multica.ai/acme/issues/ACME-3",
		"https://multica.ai/acme/issues/ACME-4",
		"https://multica.ai/acme/issues/ACME-5",
		"https://multica.ai/acme/issues/ACME-6",
		"https://multica.ai/acme/issues/ACME-7",
	}
	if err := svc.HandleLinkShared(ctx, LinkSharedInput{
		Channel:      "C1",
		MessageTS:    "1700000003.000100",
		PosterUserID: "U_LINKED",
		Links:        links,
	}); err != nil {
		t.Fatalf("7 links HandleLinkShared: %v", err)
	}

	calls := slackMock.snapshot()
	if len(calls) != 1 {
		t.Fatalf("expected 1 Slack call, got %d", len(calls))
	}
	if calls[0].Method != "by_ts" {
		t.Fatalf("expected UnfurlByTS when UnfurlID/Source empty, got %s", calls[0].Method)
	}
	if len(calls[0].Unfurls) != 5 {
		t.Fatalf("expected max 5 unfurled links, got %d", len(calls[0].Unfurls))
	}
	for _, excluded := range []string{links[5], links[6]} {
		if _, ok := calls[0].Unfurls[excluded]; ok {
			t.Fatalf("unexpected 6th/7th link unfurled: %s", excluded)
		}
	}
}

func TestHandleLinkSharedMultiWorkspaceSlug(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()

	// Poster's default notification workspace is ws-default, but they paste a link from ws-second.
	if err := st.UpsertMulticaConnection(ctx, store.UpsertMulticaParams{
		SlackUserID:          "U_MULTI",
		SlackTeamID:          "T1",
		MulticaUserID:        "mu-1",
		MulticaUserName:      "Alice",
		MulticaWorkspaceID:   "ws-default",
		MulticaWorkspaceSlug: "default-org",
		MulticaPAT:           "mul_multi",
	}); err != nil {
		t.Fatalf("upsert user: %v", err)
	}

	var gotWorkspaceHeader string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/workspaces":
			_ = json.NewEncoder(w).Encode([]multica.Workspace{
				{ID: "ws-default", Name: "Default Org", Slug: "default-org"},
				{ID: "ws-second", Name: "Second Org", Slug: "second-org"},
			})
		case "/api/issues/SEC-42":
			gotWorkspaceHeader = r.Header.Get("X-Workspace-ID")
			_ = json.NewEncoder(w).Encode(multica.Issue{
				ID:             "i-sec-42",
				Identifier:     "SEC-42",
				Title:          "Cross-workspace link",
				Status:         "done",
				StatusCategory: "completed",
				UpdatedAt:      time.Now().UTC(),
			})
		case "/api/workspaces/ws-second/members", "/api/agents", "/api/squads", "/api/projects", "/api/issue-statuses":
			_, _ = w.Write([]byte("[]"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	mc := multica.NewClient(srv.URL, srv.Client())
	cache := multica.NewMetadataCache(mc, time.Minute)
	slackMock := &mockSlackUnfurler{}
	svc := NewService(ServiceConfig{
		AppBaseURL: "https://multica.ai",
		BotToken:   "xoxb-bot",
	}, st, mc, cache, slackMock, nil)

	err := svc.HandleLinkShared(ctx, LinkSharedInput{
		Channel:      "C777",
		MessageTS:    "1700000004.000100",
		PosterUserID: "U_MULTI",
		Links:        []string{"https://multica.ai/second-org/issues/SEC-42"},
	})
	if err != nil {
		t.Fatalf("HandleLinkShared: %v", err)
	}

	if gotWorkspaceHeader != "ws-second" {
		t.Fatalf("X-Workspace-ID = %q, want ws-second", gotWorkspaceHeader)
	}
	calls := slackMock.snapshot()
	if len(calls) != 1 {
		t.Fatalf("expected 1 Slack unfurl call, got %d", len(calls))
	}
	att, ok := calls[0].Unfurls["https://multica.ai/second-org/issues/SEC-42"]
	if !ok {
		t.Fatal("expected unfurl entry for second-org issue URL")
	}
	if att.Color != "#22C55E" {
		t.Fatalf("Color = %q, want #22C55E", att.Color)
	}
}

func TestHandleLinkSharedRetryOrderPosterThenAuthorizerAndNonRetryable(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()

	if err := st.UpsertMulticaConnection(ctx, store.UpsertMulticaParams{
		SlackUserID:          "U_POSTER",
		SlackTeamID:          "T1",
		MulticaUserID:        "mu-p",
		MulticaUserName:      "Poster",
		MulticaWorkspaceID:   "ws-1",
		MulticaWorkspaceSlug: "acme",
		MulticaPAT:           "mul_poster",
	}); err != nil {
		t.Fatalf("upsert poster: %v", err)
	}
	if err := st.SetSlackUserToken(ctx, "U_POSTER", "T1", "xoxp-poster"); err != nil {
		t.Fatalf("set poster slack token: %v", err)
	}
	if err := st.SetSlackUserToken(ctx, "U_AUTH", "T1", "xoxp-auth"); err != nil {
		t.Fatalf("set auth slack token: %v", err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/workspaces":
			_ = json.NewEncoder(w).Encode([]multica.Workspace{{ID: "ws-1", Name: "Everlight", Slug: "acme"}})
		case "/api/docs/doc-1":
			_ = json.NewEncoder(w).Encode(multica.Doc{
				ID:        "doc-1",
				Title:     "Spec",
				Content:   "Details",
				UpdatedAt: time.Now().UTC(),
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	mc := multica.NewClient(srv.URL, srv.Client())
	cache := multica.NewMetadataCache(mc, time.Minute)

	// Retryable errors on bot and poster -> succeeds on authorizer token.
	slackMock := &mockSlackUnfurler{
		errFn: func(call slackUnfurlCall) error {
			switch call.Token {
			case "xoxb-bot":
				return errors.New("not_in_channel")
			case "xoxp-poster":
				return errors.New("cannot_unfurl_url")
			default:
				return nil
			}
		},
	}

	svc := NewService(ServiceConfig{
		AppBaseURL: "https://multica.ai",
		BotToken:   "xoxb-bot",
	}, st, mc, cache, slackMock, nil)

	err := svc.HandleLinkShared(ctx, LinkSharedInput{
		Channel:           "C_PRIV",
		MessageTS:         "1700000005.000100",
		PosterUserID:      "U_POSTER",
		AuthorizingUserID: "U_AUTH",
		Links:             []string{"https://multica.ai/acme/docs/doc-1"},
	})
	if err != nil {
		t.Fatalf("HandleLinkShared: %v", err)
	}
	calls := slackMock.snapshot()
	if len(calls) != 3 {
		t.Fatalf("expected 3 Slack calls (bot, poster xoxp, auth xoxp), got %d", len(calls))
	}
	if calls[0].Token != "xoxb-bot" || calls[1].Token != "xoxp-poster" || calls[2].Token != "xoxp-auth" {
		t.Fatalf("unexpected token sequence: %+v", []string{calls[0].Token, calls[1].Token, calls[2].Token})
	}
}

func TestHandleLinkSharedReleasesDedupOnFailure(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()

	// Poster A has Multica PAT only (no Slack xoxp- token).
	if err := st.UpsertMulticaConnection(ctx, store.UpsertMulticaParams{
		SlackUserID:          "U_POSTER_A",
		SlackTeamID:          "T1",
		MulticaUserID:        "mu-a",
		MulticaUserName:      "Alice",
		MulticaWorkspaceID:   "ws-1",
		MulticaWorkspaceSlug: "acme",
		MulticaPAT:           "mul_poster_a",
	}); err != nil {
		t.Fatalf("upsert poster A: %v", err)
	}
	// Recipient B has Slack xoxp- token only.
	if err := st.SetSlackUserToken(ctx, "U_RECIP_B", "T1", "xoxp-recip-b"); err != nil {
		t.Fatalf("set slack token B: %v", err)
	}

	var returnNotFound atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/workspaces":
			_ = json.NewEncoder(w).Encode([]multica.Workspace{{ID: "ws-1", Name: "Everlight", Slug: "acme"}})
		case "/api/issues/ACME-42":
			if returnNotFound.Load() {
				http.NotFound(w, r)
				return
			}
			_ = json.NewEncoder(w).Encode(multica.Issue{
				ID:             "i-1",
				Identifier:     "ACME-42",
				Title:          "Retry after failure",
				Status:         "in_progress",
				StatusCategory: "started",
				UpdatedAt:      time.Now().UTC(),
			})
		case "/api/issues/search":
			_, _ = w.Write([]byte("[]"))
		case "/api/workspaces/ws-1/members", "/api/agents", "/api/squads", "/api/projects", "/api/issue-statuses":
			_, _ = w.Write([]byte("[]"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	mc := multica.NewClient(srv.URL, srv.Client())
	cache := multica.NewMetadataCache(mc, time.Minute)
	slackMock := &mockSlackUnfurler{
		errFn: func(call slackUnfurlCall) error {
			if call.Token == "xoxb-bot" {
				return slack.SlackErrorResponse{Err: "channel_not_found"}
			}
			return nil
		},
	}

	svc := NewService(ServiceConfig{
		AppBaseURL: "https://multica.ai",
		BotToken:   "xoxb-bot",
		DedupTTL:   30 * time.Second,
	}, st, mc, cache, slackMock, nil)

	link := "https://multica.ai/acme/issues/ACME-42"

	// Case 1: Zero cards produced (transient 404) releases dedup claim so retry succeeds.
	returnNotFound.Store(true)
	if err := svc.HandleLinkShared(ctx, LinkSharedInput{
		Channel:           "D999",
		MessageTS:         "1700000010.000100",
		PosterUserID:      "U_POSTER_A",
		AuthorizingUserID: "U_RECIP_B",
		Links:             []string{link},
	}); err != nil {
		t.Fatalf("expected nil on 0 cards, got %v", err)
	}
	if len(slackMock.snapshot()) != 0 {
		t.Fatalf("expected 0 Slack calls when 0 cards produced")
	}

	// Case 2: Poster's own event arrives first and dispatchUnfurl fails (channel_not_found, no xoxp- for A).
	returnNotFound.Store(false)
	err := svc.HandleLinkShared(ctx, LinkSharedInput{
		Channel:           "D999",
		MessageTS:         "1700000010.000100",
		UnfurlID:          "unf-a",
		Source:            "composer",
		PosterUserID:      "U_POSTER_A",
		AuthorizingUserID: "U_POSTER_A",
		Links:             []string{link},
	})
	if err == nil {
		t.Fatal("expected error when bot fails with channel_not_found and poster has no xoxp-")
	}

	// Case 3: Recipient B's event for the same (channel, message_ts, url) arrives within 30s and succeeds.
	if err := svc.HandleLinkShared(ctx, LinkSharedInput{
		Channel:           "D999",
		MessageTS:         "1700000010.000100",
		UnfurlID:          "unf-b",
		Source:            "composer",
		PosterUserID:      "U_POSTER_A",
		AuthorizingUserID: "U_RECIP_B",
		Links:             []string{link},
	}); err != nil {
		t.Fatalf("expected recipient B event to succeed after A's failed attempt, got %v", err)
	}

	calls := slackMock.snapshot()
	if len(calls) != 3 {
		t.Fatalf("expected 3 Slack calls (A bot fail, B bot fail, B xoxp succeed), got %d", len(calls))
	}
	if calls[2].Token != "xoxp-recip-b" || calls[2].UnfurlID != "unf-b" {
		t.Fatalf("expected final call with B's xoxp- and unf-b, got %+v", calls[2])
	}

	// Case 4: Duplicate event after success is suppressed within 30s.
	if err := svc.HandleLinkShared(ctx, LinkSharedInput{
		Channel:           "D999",
		MessageTS:         "1700000010.000100",
		UnfurlID:          "unf-b-dup",
		Source:            "composer",
		PosterUserID:      "U_POSTER_A",
		AuthorizingUserID: "U_RECIP_B",
		Links:             []string{link},
	}); err != nil {
		t.Fatalf("duplicate after success: %v", err)
	}
	if len(slackMock.snapshot()) != 3 {
		t.Fatalf("expected duplicate after success to be deduplicated, got %d calls", len(slackMock.snapshot()))
	}
}

func TestFormatterRuneSafetyAndURLEscaping(t *testing.T) {
	if got := humanizeToken("über_dringend"); got != "Über Dringend" {
		t.Fatalf("humanizeToken multibyte: got %q, want %q", got, "Über Dringend")
	}
	got := formatLinkedTitle("https://multica.ai/ws/issues/ACME-1?a=1&b=2>x|y", "A & B")
	want := "*<https://multica.ai/ws/issues/ACME-1?a=1&amp;b=2%3Ex%7Cy|A &amp; B>*"
	if got != want {
		t.Fatalf("formatLinkedTitle: got %q, want %q", got, want)
	}
}
