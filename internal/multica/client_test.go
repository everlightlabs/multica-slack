package multica

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestClientHeadersAndAuthErrors(t *testing.T) {
	var seenAuth, seenWorkspace string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenAuth = r.Header.Get("Authorization")
		seenWorkspace = r.Header.Get("X-Workspace-ID")

		if r.URL.Path == "/api/me" {
			if seenAuth == "Bearer mul_bad" {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"error":"invalid token"}`))
				return
			}
			_ = json.NewEncoder(w).Encode(Me{ID: "u1", Name: "Alice", Email: "alice@example.com"})
			return
		}
		if r.URL.Path == "/api/projects/p1" {
			_ = json.NewEncoder(w).Encode(Project{ID: "p1", Name: "Core"})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	client := NewClient(srv.URL, srv.Client())
	ctx := context.Background()

	me, err := client.GetMe(ctx, "mul_test")
	if err != nil {
		t.Fatalf("GetMe failed: %v", err)
	}
	if me.ID != "u1" || seenAuth != "Bearer mul_test" {
		t.Fatalf("unexpected GetMe result (%+v) or auth header (%q)", me, seenAuth)
	}

	proj, err := client.GetProject(ctx, "mul_test", "ws-1", "p1")
	if err != nil {
		t.Fatalf("GetProject failed: %v", err)
	}
	if proj.ID != "p1" || seenAuth != "Bearer mul_test" || seenWorkspace != "ws-1" {
		t.Fatalf("unexpected GetProject result (%+v) or headers (auth=%q ws=%q)", proj, seenAuth, seenWorkspace)
	}

	_, err = client.GetMe(ctx, "mul_bad")
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("expected ErrUnauthorized on 401, got %v", err)
	}
}

func TestGetIssueIdentifierSearchFallback(t *testing.T) {
	var directHits, searchHits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Workspace-ID") != "ws-1" {
			t.Fatalf("missing X-Workspace-ID header: %q", r.Header.Get("X-Workspace-ID"))
		}
		switch r.URL.Path {
		case "/api/issues/ACME-42":
			directHits.Add(1)
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"invalid uuid"}`))
		case "/api/issues/search":
			searchHits.Add(1)
			if q := r.URL.Query().Get("q"); q != "ACME-42" {
				t.Fatalf("unexpected search query q=%q", q)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"issues": []Issue{
					{ID: "id-other", Identifier: "ACME-420", Title: "Wrong match"},
					{ID: "id-exact", Identifier: "ACME-42", Title: "Exact match", Status: "in_progress"},
				},
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	client := NewClient(srv.URL, srv.Client())
	issue, err := client.GetIssue(context.Background(), "mul_test", "ws-1", "ACME-42")
	if err != nil {
		t.Fatalf("GetIssue fallback failed: %v", err)
	}
	if issue.ID != "id-exact" || issue.Identifier != "ACME-42" {
		t.Fatalf("got wrong issue: %+v", issue)
	}
	if directHits.Load() != 1 || searchHits.Load() != 1 {
		t.Fatalf("expected 1 direct hit and 1 search hit, got direct=%d search=%d", directHits.Load(), searchHits.Load())
	}
}

func TestListInboxAndMutations(t *testing.T) {
	var commentContent string
	var readHit, commentHit atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/inbox":
			_, _ = w.Write([]byte(`[
				{
					"id": "inb-1",
					"workspace_id": "ws-1",
					"recipient_id": "u-1",
					"type": "mentioned",
					"severity": "info",
					"title": "Mentioned you",
					"body": "Please review",
					"read": false,
					"archived": false,
					"created_at": "2026-11-26T12:00:00Z",
					"issue_id": "iss-1",
					"issue_identifier": "ACME-42",
					"issue_status": "in_progress",
					"issue_priority": "high",
					"issue_project_id": "proj-1",
					"actor_type": "member",
					"actor_id": "u-2"
				}
			]`))
		case r.Method == http.MethodPost && r.URL.Path == "/api/inbox/inb-1/read":
			readHit.Add(1)
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && r.URL.Path == "/api/issues/iss-1/comments":
			commentHit.Add(1)
			var body struct {
				Content string `json:"content"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatalf("decode comment body: %v", err)
			}
			commentContent = body.Content
			w.WriteHeader(http.StatusCreated)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	client := NewClient(srv.URL, srv.Client())
	ctx := context.Background()

	items, err := client.ListInbox(ctx, "mul_test", "ws-1")
	if err != nil {
		t.Fatalf("ListInbox failed: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("expected 1 inbox item, got %d", len(items))
	}
	it := items[0]
	if it.IssueIdentifier != "ACME-42" || it.IssueStatus != "in_progress" || it.IssueProjectID != "proj-1" || it.ActorType != "member" || it.ActorID != "u-2" {
		t.Fatalf("unexpected decoded inbox item: %+v", it)
	}

	if err := client.MarkInboxRead(ctx, "mul_test", "ws-1", "inb-1"); err != nil {
		t.Fatalf("MarkInboxRead failed: %v", err)
	}
	if readHit.Load() != 1 {
		t.Fatalf("expected 1 read hit, got %d", readHit.Load())
	}

	if err := client.CreateComment(ctx, "mul_test", "ws-1", "iss-1", "Looks good!"); err != nil {
		t.Fatalf("CreateComment failed: %v", err)
	}
	if commentHit.Load() != 1 || commentContent != "Looks good!" {
		t.Fatalf("expected comment hit=1 content='Looks good!', got hit=%d content=%q", commentHit.Load(), commentContent)
	}
}

func TestMetadataCacheTTLAndPartialFailure(t *testing.T) {
	var memberHits, squadHits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/workspaces/ws-1/members":
			memberHits.Add(1)
			_ = json.NewEncoder(w).Encode([]Member{{ID: "m1", UserID: "u1", Name: "Alice"}})
		case "/api/agents":
			_ = json.NewEncoder(w).Encode([]Agent{{ID: "a1", Name: "Polaris"}})
		case "/api/squads":
			squadHits.Add(1)
			w.WriteHeader(http.StatusInternalServerError)
		case "/api/projects":
			_ = json.NewEncoder(w).Encode([]Project{{ID: "p1", Name: "Slack App"}})
		case "/api/issue-statuses":
			_ = json.NewEncoder(w).Encode([]IssueStatus{{ID: "s1", Key: "in_progress", Name: "In Progress", Category: "started"}})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	client := NewClient(srv.URL, srv.Client())
	cache := NewMetadataCache(client, 40*time.Millisecond)
	ctx := context.Background()

	meta1, err := cache.WorkspaceMetadata(ctx, "mul_test", "ws-1")
	if err != nil {
		t.Fatalf("WorkspaceMetadata failed on partial squad error: %v", err)
	}
	if meta1.MembersByUserID["u1"].Name != "Alice" {
		t.Fatalf("missing member u1: %+v", meta1.MembersByUserID)
	}
	if meta1.AgentsByID["a1"].Name != "Polaris" {
		t.Fatalf("missing agent a1: %+v", meta1.AgentsByID)
	}
	if meta1.ProjectsByID["p1"].Name != "Slack App" {
		t.Fatalf("missing project p1: %+v", meta1.ProjectsByID)
	}
	if meta1.StatusesByKey["in_progress"].Name != "In Progress" {
		t.Fatalf("missing status in_progress: %+v", meta1.StatusesByKey)
	}
	if meta1.SquadsByID == nil {
		t.Fatal("expected SquadsByID map to be non-nil on partial fetch failure")
	}

	// Second call within TTL must hit cache.
	_, err = cache.WorkspaceMetadata(ctx, "mul_test", "ws-1")
	if err != nil {
		t.Fatalf("second WorkspaceMetadata failed: %v", err)
	}
	if memberHits.Load() != 1 || squadHits.Load() != 1 {
		t.Fatalf("expected cache hit (1 request each), got members=%d squads=%d", memberHits.Load(), squadHits.Load())
	}

	// Wait for TTL expiry and verify refresh.
	time.Sleep(60 * time.Millisecond)
	_, err = cache.WorkspaceMetadata(ctx, "mul_test", "ws-1")
	if err != nil {
		t.Fatalf("refreshed WorkspaceMetadata failed: %v", err)
	}
	if memberHits.Load() != 2 {
		t.Fatalf("expected cache refresh after TTL expiry (2 member hits), got %d", memberHits.Load())
	}
}

func TestResolveWorkspaceBySlugAndInvalidateUser(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/workspaces" {
			hits.Add(1)
			_ = json.NewEncoder(w).Encode([]Workspace{
				{ID: "ws-1", Name: "Acme Corp", Slug: "acme"},
				{ID: "ws-2", Name: "Sandbox", Slug: "sandbox"},
			})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	client := NewClient(srv.URL, srv.Client())
	cache := NewMetadataCache(client, time.Minute)
	ctx := context.Background()

	ws, err := cache.ResolveWorkspaceBySlug(ctx, "U1", "mul_test", "Acme")
	if err != nil {
		t.Fatalf("ResolveWorkspaceBySlug failed: %v", err)
	}
	if ws.ID != "ws-1" {
		t.Fatalf("got workspace %+v, want ws-1", ws)
	}

	// Cached lookup for another slug of same user should not hit server again.
	ws2, err := cache.ResolveWorkspaceBySlug(ctx, "U1", "mul_test", "sandbox")
	if err != nil || ws2.ID != "ws-2" {
		t.Fatalf("cached second slug failed: ws=%+v err=%v", ws2, err)
	}
	if hits.Load() != 1 {
		t.Fatalf("expected 1 /api/workspaces hit, got %d", hits.Load())
	}

	if _, err := cache.ResolveWorkspaceBySlug(ctx, "U1", "mul_test", "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound for missing slug, got %v", err)
	}

	cache.InvalidateUser("U1")
	if _, err := cache.ResolveWorkspaceBySlug(ctx, "U1", "mul_test", "sandbox"); err != nil {
		t.Fatalf("post-invalidate ResolveWorkspaceBySlug failed: %v", err)
	}
	if hits.Load() != 2 {
		t.Fatalf("expected 2 hits after InvalidateUser, got %d", hits.Load())
	}
}

func TestEndpointsAndErrorRedaction(t *testing.T) {
	var searchCalls atomic.Int32
	uuid := "123e4567-e89b-12d3-a456-426614174000"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/issues/" + uuid:
			w.WriteHeader(http.StatusNotFound)
		case "/api/issues/ACME-99":
			w.WriteHeader(http.StatusNotFound)
		case "/api/issues/search":
			searchCalls.Add(1)
			// Flat array format with case-insensitive match.
			_ = json.NewEncoder(w).Encode([]Issue{
				{ID: "i-99", Identifier: "acme-99", Title: "Lowercase match", Labels: []Label{{Name: "bug"}}},
			})
		case "/api/projects/p1/progress":
			_, _ = w.Write([]byte(`{"project_id":"p1","total_issues":10,"completed_issues":4}`))
		case "/api/docs/d1":
			_ = json.NewEncoder(w).Encode(Doc{ID: "d1", Title: "Runbook", Content: "Steps"})
		case "/api/forbidden":
			w.WriteHeader(http.StatusForbidden)
		default:
			w.WriteHeader(http.StatusForbidden)
		}
	}))
	defer srv.Close()

	client := NewClient(srv.URL, srv.Client())
	ctx := context.Background()

	// UUID 404 must not trigger search fallback.
	if _, err := client.GetIssue(ctx, "mul_secret_token", "ws-1", uuid); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound for missing UUID, got %v", err)
	}
	if searchCalls.Load() != 0 {
		t.Fatalf("expected 0 search calls for UUID 404, got %d", searchCalls.Load())
	}

	// Non-UUID 404 falls back to search and handles flat array + case-insensitive match.
	iss, err := client.GetIssue(ctx, "mul_secret_token", "ws-1", "ACME-99")
	if err != nil {
		t.Fatalf("GetIssue flat search failed: %v", err)
	}
	if iss.ID != "i-99" || len(iss.Labels) != 1 || iss.Labels[0].Name != "bug" {
		t.Fatalf("unexpected issue decoded: %+v", iss)
	}

	prog, err := client.GetProjectProgress(ctx, "mul_secret_token", "ws-1", "p1")
	if err != nil {
		t.Fatalf("GetProjectProgress failed: %v", err)
	}
	if prog.Total != 10 || prog.Completed != 4 {
		t.Fatalf("unexpected progress: %+v", prog)
	}

	doc, err := client.GetDoc(ctx, "mul_secret_token", "ws-1", "d1")
	if err != nil || doc.Title != "Runbook" {
		t.Fatalf("GetDoc failed: doc=%+v err=%v", doc, err)
	}

	// 403 maps to ErrUnauthorized and never leaks the PAT.
	_, err = client.GetMe(ctx, "mul_secret_token")
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("expected ErrUnauthorized on 403, got %v", err)
	}
}
