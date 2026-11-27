package multica

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	defaultHTTPTimeout = 15 * time.Second
	maxResponseBytes   = 4 << 20 // 4 MiB
	maxErrorBodyBytes  = 4096
)

// Client calls Multica Cloud REST endpoints with per-request PAT authentication.
type Client struct {
	apiURL     string
	httpClient *http.Client
}

// NewClient constructs a Multica REST Client targeting apiURL.
func NewClient(apiURL string, httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: defaultHTTPTimeout}
	}
	return &Client{
		apiURL:     strings.TrimRight(strings.TrimSpace(apiURL), "/"),
		httpClient: httpClient,
	}
}

// GetMe returns the authenticated user profile.
func (c *Client) GetMe(ctx context.Context, pat string) (Me, error) {
	var out Me
	err := c.doJSON(ctx, http.MethodGet, "/api/me", pat, "", nil, &out)
	return out, err
}

// ListWorkspaces lists all Multica workspaces accessible to pat.
func (c *Client) ListWorkspaces(ctx context.Context, pat string) ([]Workspace, error) {
	var out []Workspace
	err := c.doJSON(ctx, http.MethodGet, "/api/workspaces", pat, "", nil, &out)
	return out, err
}

// GetIssue fetches an issue by UUID or identifier (for example "ACME-42").
// On 400/404 for a non-UUID identifier, it falls back to /api/issues/search?q=...
// and selects the exact case-insensitive identifier match.
func (c *Client) GetIssue(ctx context.Context, pat, workspaceID, idOrIdentifier string) (Issue, error) {
	target := strings.TrimSpace(idOrIdentifier)
	if target == "" {
		return Issue{}, ErrNotFound
	}

	var issue Issue
	err := c.doJSON(ctx, http.MethodGet, "/api/issues/"+url.PathEscape(target), pat, workspaceID, nil, &issue)
	if err == nil {
		return issue, nil
	}

	var apiErr *APIError
	if !isUUID(target) && errors.As(err, &apiErr) && (apiErr.StatusCode == http.StatusBadRequest || apiErr.StatusCode == http.StatusNotFound) {
		return c.searchIssueExact(ctx, pat, workspaceID, target)
	}
	return Issue{}, err
}

func (c *Client) searchIssueExact(ctx context.Context, pat, workspaceID, identifier string) (Issue, error) {
	raw, err := c.doRaw(ctx, http.MethodGet, "/api/issues/search?q="+url.QueryEscape(identifier), pat, workspaceID, nil)
	if err != nil {
		return Issue{}, err
	}

	candidates, err := decodeSearchIssues(raw)
	if err != nil {
		return Issue{}, fmt.Errorf("multica: decode issue search: %w", err)
	}
	for _, candidate := range candidates {
		if strings.EqualFold(candidate.Identifier, identifier) {
			return candidate, nil
		}
	}
	return Issue{}, ErrNotFound
}

func decodeSearchIssues(raw []byte) ([]Issue, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil, nil
	}
	if trimmed[0] == '[' {
		var list []Issue
		if err := json.Unmarshal(trimmed, &list); err != nil {
			return nil, err
		}
		return list, nil
	}
	var wrapped struct {
		Issues []Issue `json:"issues"`
	}
	if err := json.Unmarshal(trimmed, &wrapped); err != nil {
		return nil, err
	}
	return wrapped.Issues, nil
}

// GetProject fetches project metadata by ID.
func (c *Client) GetProject(ctx context.Context, pat, workspaceID, projectID string) (Project, error) {
	var out Project
	err := c.doJSON(ctx, http.MethodGet, "/api/projects/"+url.PathEscape(projectID), pat, workspaceID, nil, &out)
	return out, err
}

// GetProjectProgress fetches progress counts for a project.
func (c *Client) GetProjectProgress(ctx context.Context, pat, workspaceID, projectID string) (ProjectProgress, error) {
	var out ProjectProgress
	err := c.doJSON(ctx, http.MethodGet, "/api/projects/"+url.PathEscape(projectID)+"/progress", pat, workspaceID, nil, &out)
	return out, err
}

// GetDoc fetches a document by ID.
func (c *Client) GetDoc(ctx context.Context, pat, workspaceID, docID string) (Doc, error) {
	var out Doc
	err := c.doJSON(ctx, http.MethodGet, "/api/docs/"+url.PathEscape(docID), pat, workspaceID, nil, &out)
	return out, err
}

// ListInbox fetches active inbox items for the user in workspaceID.
func (c *Client) ListInbox(ctx context.Context, pat, workspaceID string) ([]InboxItem, error) {
	raw, err := c.doRaw(ctx, http.MethodGet, "/api/inbox", pat, workspaceID, nil)
	if err != nil {
		return nil, err
	}
	items, err := decodeListOrWrapped[InboxItem](raw, "items")
	if err != nil {
		return nil, fmt.Errorf("multica: decode GET /api/inbox: %w", err)
	}
	return items, nil
}

// MarkInboxRead marks an inbox item as read.
func (c *Client) MarkInboxRead(ctx context.Context, pat, workspaceID, itemID string) error {
	return c.doJSON(ctx, http.MethodPost, "/api/inbox/"+url.PathEscape(itemID)+"/read", pat, workspaceID, nil, nil)
}

// CreateComment posts a comment on an issue on behalf of the authenticated user.
func (c *Client) CreateComment(ctx context.Context, pat, workspaceID, issueID, content string) error {
	body := struct {
		Content string `json:"content"`
	}{Content: content}
	return c.doJSON(ctx, http.MethodPost, "/api/issues/"+url.PathEscape(issueID)+"/comments", pat, workspaceID, body, nil)
}

// ListMembers lists workspace members.
func (c *Client) ListMembers(ctx context.Context, pat, workspaceID string) ([]Member, error) {
	var out []Member
	err := c.doJSON(ctx, http.MethodGet, "/api/workspaces/"+url.PathEscape(workspaceID)+"/members", pat, workspaceID, nil, &out)
	return out, err
}

// ListAgents lists agents in the workspace.
func (c *Client) ListAgents(ctx context.Context, pat, workspaceID string) ([]Agent, error) {
	var out []Agent
	err := c.doJSON(ctx, http.MethodGet, "/api/agents", pat, workspaceID, nil, &out)
	return out, err
}

// ListSquads lists squads in the workspace.
func (c *Client) ListSquads(ctx context.Context, pat, workspaceID string) ([]Squad, error) {
	var out []Squad
	err := c.doJSON(ctx, http.MethodGet, "/api/squads", pat, workspaceID, nil, &out)
	return out, err
}

// ListProjects lists projects in the workspace.
func (c *Client) ListProjects(ctx context.Context, pat, workspaceID string) ([]Project, error) {
	raw, err := c.doRaw(ctx, http.MethodGet, "/api/projects", pat, workspaceID, nil)
	if err != nil {
		return nil, err
	}
	projects, err := decodeListOrWrapped[Project](raw, "projects")
	if err != nil {
		return nil, fmt.Errorf("multica: decode GET /api/projects: %w", err)
	}
	return projects, nil
}

// ListIssueStatuses lists configured issue statuses in the workspace.
func (c *Client) ListIssueStatuses(ctx context.Context, pat, workspaceID string) ([]IssueStatus, error) {
	raw, err := c.doRaw(ctx, http.MethodGet, "/api/issue-statuses", pat, workspaceID, nil)
	if err != nil {
		return nil, err
	}
	statuses, err := decodeListOrWrapped[IssueStatus](raw, "statuses", "issue_statuses")
	if err != nil {
		return nil, fmt.Errorf("multica: decode GET /api/issue-statuses: %w", err)
	}
	return statuses, nil
}

func decodeListOrWrapped[T any](raw []byte, keys ...string) ([]T, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil, nil
	}
	if trimmed[0] == '[' {
		var list []T
		if err := json.Unmarshal(trimmed, &list); err != nil {
			return nil, err
		}
		return list, nil
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &obj); err != nil {
		return nil, err
	}
	for _, key := range keys {
		if field, ok := obj[key]; ok {
			var list []T
			if err := json.Unmarshal(field, &list); err != nil {
				return nil, err
			}
			return list, nil
		}
	}
	return nil, fmt.Errorf("expected array or object containing one of %v", keys)
}

func (c *Client) doJSON(ctx context.Context, method, pathWithQuery, pat, workspaceID string, reqBody any, out any) error {
	raw, err := c.doRaw(ctx, method, pathWithQuery, pat, workspaceID, reqBody)
	if err != nil {
		return err
	}
	if out == nil || len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("multica: decode %s %s: %w", method, cleanPath(pathWithQuery), err)
	}
	return nil
}

func (c *Client) doRaw(ctx context.Context, method, pathWithQuery, pat, workspaceID string, reqBody any) ([]byte, error) {
	if c == nil || c.apiURL == "" {
		return nil, errors.New("multica: uninitialized client")
	}
	if strings.TrimSpace(pat) == "" {
		return nil, ErrUnauthorized
	}

	var bodyReader io.Reader
	if reqBody != nil {
		encoded, err := json.Marshal(reqBody)
		if err != nil {
			return nil, fmt.Errorf("multica: encode request body: %w", err)
		}
		bodyReader = bytes.NewReader(encoded)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.apiURL+pathWithQuery, bodyReader)
	if err != nil {
		return nil, fmt.Errorf("multica: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+pat)
	req.Header.Set("Accept", "application/json")
	if reqBody != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if workspaceID != "" {
		req.Header.Set("X-Workspace-ID", workspaceID)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("multica: %s %s: %w", method, cleanPath(pathWithQuery), err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return nil, fmt.Errorf("multica: read response %s %s: %w", method, cleanPath(pathWithQuery), err)
	}

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return body, nil
	}

	var sentinel error
	switch resp.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden:
		sentinel = ErrUnauthorized
	case http.StatusNotFound:
		sentinel = ErrNotFound
	}

	return nil, &APIError{
		StatusCode: resp.StatusCode,
		Method:     method,
		Path:       cleanPath(pathWithQuery),
		Message:    extractErrorMessage(body),
		Err:        sentinel,
	}
}

func cleanPath(pathWithQuery string) string {
	if idx := strings.IndexByte(pathWithQuery, '?'); idx >= 0 {
		return pathWithQuery[:idx]
	}
	return pathWithQuery
}

func extractErrorMessage(body []byte) string {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return ""
	}
	if len(trimmed) > maxErrorBodyBytes {
		trimmed = trimmed[:maxErrorBodyBytes]
	}
	var payload struct {
		Error   string `json:"error"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(trimmed, &payload); err == nil {
		if payload.Error != "" {
			return payload.Error
		}
		if payload.Message != "" {
			return payload.Message
		}
	}
	return string(trimmed)
}

func isUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			if (c < '0' || c > '9') && (c < 'a' || c > 'f') && (c < 'A' || c > 'F') {
				return false
			}
		}
	}
	return true
}
