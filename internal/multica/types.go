// Package multica provides a typed Multica Cloud REST client, wire models,
// and short-TTL in-memory metadata caching.
package multica

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

var (
	// ErrUnauthorized indicates the Multica PAT was rejected (HTTP 401 or 403).
	ErrUnauthorized = errors.New("multica: unauthorized")
	// ErrNotFound indicates the requested Multica resource does not exist (HTTP 404).
	ErrNotFound = errors.New("multica: not found")
)

// APIError captures a non-2xx HTTP response without exposing credentials.
type APIError struct {
	StatusCode int
	Method     string
	Path       string
	Message    string
	Err        error
}

func (e *APIError) Error() string {
	if e.Message != "" {
		return fmt.Sprintf("multica: %s %s returned %d: %s", e.Method, e.Path, e.StatusCode, e.Message)
	}
	return fmt.Sprintf("multica: %s %s returned %d", e.Method, e.Path, e.StatusCode)
}

func (e *APIError) Unwrap() error {
	return e.Err
}

// Me represents the authenticated Multica user returned by GET /api/me.
type Me struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Email     string `json:"email"`
	AvatarURL string `json:"avatar_url"`
}

// Workspace represents a Multica workspace accessible to a user.
type Workspace struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Slug string `json:"slug"`
}

// Label represents an issue label. It decodes both string labels and structured objects.
type Label struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Color string `json:"color"`
}

// UnmarshalJSON accepts either a JSON string ("bug") or an object ({"id","name","color"}).
func (l *Label) UnmarshalJSON(data []byte) error {
	var asString string
	if err := json.Unmarshal(data, &asString); err == nil {
		l.Name = asString
		return nil
	}
	type labelAlias Label
	var obj labelAlias
	if err := json.Unmarshal(data, &obj); err != nil {
		return err
	}
	*l = Label(obj)
	return nil
}

// Issue represents a Multica issue from REST endpoints.
type Issue struct {
	ID             string    `json:"id"`
	Identifier     string    `json:"identifier"`
	Title          string    `json:"title"`
	Description    string    `json:"description"`
	Status         string    `json:"status"`
	StatusCategory string    `json:"status_category"`
	StatusName     string    `json:"status_name"`
	Priority       string    `json:"priority"`
	ProjectID      string    `json:"project_id"`
	AssigneeType   string    `json:"assignee_type"`
	AssigneeID     string    `json:"assignee_id"`
	Labels         []Label   `json:"labels"`
	UpdatedAt      time.Time `json:"updated_at"`
}

// Project represents a Multica project.
type Project struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	Status      string    `json:"status"`
	Color       string    `json:"color"`
	Icon        string    `json:"icon"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// UnmarshalJSON supports both "name" and Multica Cloud's "title" field.
func (p *Project) UnmarshalJSON(data []byte) error {
	type projectAlias Project
	var raw struct {
		projectAlias
		Title string `json:"title"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	*p = Project(raw.projectAlias)
	if p.Name == "" && raw.Title != "" {
		p.Name = raw.Title
	}
	return nil
}

// ProjectProgress represents aggregate completion metrics for a project.
type ProjectProgress struct {
	ProjectID  string `json:"project_id"`
	Total      int    `json:"total"`
	Completed  int    `json:"completed"`
	InProgress int    `json:"in_progress"`
	Cancelled  int    `json:"cancelled"`
}

// UnmarshalJSON supports flat total/completed/total_issues/completed_issues fields as well as
// Multica Cloud's nested {"progress":{"scope","completed","started","cancelled"}} response.
func (p *ProjectProgress) UnmarshalJSON(data []byte) error {
	var raw struct {
		ProjectID       string `json:"project_id"`
		Total           *int   `json:"total"`
		TotalIssues     *int   `json:"total_issues"`
		Completed       *int   `json:"completed"`
		CompletedIssues *int   `json:"completed_issues"`
		InProgress      *int   `json:"in_progress"`
		Cancelled       *int   `json:"cancelled"`
		Progress        *struct {
			Scope     int `json:"scope"`
			Completed int `json:"completed"`
			Started   int `json:"started"`
			Cancelled int `json:"cancelled"`
		} `json:"progress"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	p.ProjectID = raw.ProjectID
	if raw.Progress != nil {
		p.Total = raw.Progress.Scope
		p.Completed = raw.Progress.Completed
		p.InProgress = raw.Progress.Started
		p.Cancelled = raw.Progress.Cancelled
	}
	if raw.InProgress != nil {
		p.InProgress = *raw.InProgress
	}
	if raw.Cancelled != nil {
		p.Cancelled = *raw.Cancelled
	}
	if raw.Total != nil {
		p.Total = *raw.Total
	} else if raw.TotalIssues != nil {
		p.Total = *raw.TotalIssues
	}
	if raw.Completed != nil {
		p.Completed = *raw.Completed
	} else if raw.CompletedIssues != nil {
		p.Completed = *raw.CompletedIssues
	}
	return nil
}

// Doc represents a Multica document.
type Doc struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	Content   string    `json:"content"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Member represents a human workspace member.
type Member struct {
	ID        string `json:"id"`
	UserID    string `json:"user_id"`
	Name      string `json:"name"`
	Email     string `json:"email"`
	Role      string `json:"role"`
	AvatarURL string `json:"avatar_url"`
}

// Agent represents an AI agent configured in a workspace.
type Agent struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Runtime   string `json:"runtime"`
	Status    string `json:"status"`
	AvatarURL string `json:"avatar_url"`
}

// Squad represents a workspace squad/team.
type Squad struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Slug        string `json:"slug"`
	Description string `json:"description"`
}

// IssueStatus represents a custom or built-in issue status definition.
type IssueStatus struct {
	ID       string `json:"id"`
	Key      string `json:"key"`
	Name     string `json:"name"`
	Category string `json:"category"`
	Color    string `json:"color"`
	Position int    `json:"position"`
}

// InboxItem represents an inbox notification item returned by GET /api/inbox and inbox:new.
type InboxItem struct {
	ID                string          `json:"id"`
	WorkspaceID       string          `json:"workspace_id"`
	RecipientID       string          `json:"recipient_id"`
	Type              string          `json:"type"`
	Severity          string          `json:"severity"`
	Title             string          `json:"title"`
	Body              string          `json:"body"`
	Read              bool            `json:"read"`
	Archived          bool            `json:"archived"`
	CreatedAt         time.Time       `json:"created_at"`
	IssueID           string          `json:"issue_id"`
	IssueIdentifier   string          `json:"issue_identifier"`
	IssueStatus       string          `json:"issue_status"`
	IssuePriority     string          `json:"issue_priority"`
	IssueProjectID    string          `json:"issue_project_id"`
	IssueAssigneeType string          `json:"issue_assignee_type"`
	IssueAssigneeID   string          `json:"issue_assignee_id"`
	IssueDueDate      string          `json:"issue_due_date"`
	IssueLabelIDs     []string        `json:"issue_label_ids"`
	ActorType         string          `json:"actor_type"`
	ActorID           string          `json:"actor_id"`
	Details           json.RawMessage `json:"details"`
}

// WSEnvelope represents a Multica WebSocket message frame.
type WSEnvelope struct {
	Type      string          `json:"type"`
	Payload   json.RawMessage `json:"payload"`
	ActorID   string          `json:"actor_id"`
	ActorType string          `json:"actor_type"`
	Error     string          `json:"error,omitempty"`
}
