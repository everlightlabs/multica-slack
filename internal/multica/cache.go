package multica

import (
	"context"
	"crypto/sha256"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"time"
)

const defaultMetadataTTL = 60 * time.Second

// WorkspaceMeta holds indexed lookup maps for a single Multica workspace.
type WorkspaceMeta struct {
	MembersByUserID map[string]Member
	AgentsByID      map[string]Agent
	SquadsByID      map[string]Squad
	ProjectsByID    map[string]Project
	StatusesByKey   map[string]IssueStatus
}

type cachedWorkspaces struct {
	patDigest [32]byte
	bySlug    map[string]Workspace
	expiresAt time.Time
}

type cachedMeta struct {
	meta      WorkspaceMeta
	expiresAt time.Time
}

// MetadataCache caches per-user workspace lists and per-workspace entity lookup maps.
type MetadataCache struct {
	client *Client
	ttl    time.Duration
	logger *slog.Logger

	mu         sync.RWMutex
	workspaces map[string]cachedWorkspaces
	meta       map[string]cachedMeta
}

// NewMetadataCache constructs a MetadataCache with the given TTL (defaults to 60s when <= 0).
func NewMetadataCache(client *Client, ttl time.Duration) *MetadataCache {
	if ttl <= 0 {
		ttl = defaultMetadataTTL
	}
	return &MetadataCache{
		client:     client,
		ttl:        ttl,
		logger:     slog.Default(),
		workspaces: make(map[string]cachedWorkspaces),
		meta:       make(map[string]cachedMeta),
	}
}

// SetLogger configures the logger used for partial metadata fetch warnings.
func (c *MetadataCache) SetLogger(logger *slog.Logger) {
	if logger != nil {
		c.logger = logger
	}
}

// ResolveWorkspaceBySlug resolves a workspace slug accessible to pat for slackUserID,
// caching the user's accessible workspaces for the configured TTL.
func (c *MetadataCache) ResolveWorkspaceBySlug(ctx context.Context, slackUserID, pat, slug string) (Workspace, error) {
	patDigest := sha256.Sum256([]byte(pat))
	keySlug := strings.ToLower(strings.TrimSpace(slug))
	if keySlug == "" {
		return Workspace{}, ErrNotFound
	}

	now := time.Now()
	if slackUserID != "" {
		c.mu.RLock()
		entry, ok := c.workspaces[slackUserID]
		c.mu.RUnlock()
		if ok && entry.patDigest == patDigest && now.Before(entry.expiresAt) {
			if ws, found := entry.bySlug[keySlug]; found {
				return ws, nil
			}
			return Workspace{}, ErrNotFound
		}
	}

	list, err := c.client.ListWorkspaces(ctx, pat)
	if err != nil {
		return Workspace{}, err
	}

	bySlug := make(map[string]Workspace, len(list))
	for _, ws := range list {
		if ws.Slug != "" {
			bySlug[strings.ToLower(ws.Slug)] = ws
		}
	}

	if slackUserID != "" {
		c.mu.Lock()
		c.workspaces[slackUserID] = cachedWorkspaces{
			patDigest: patDigest,
			bySlug:    bySlug,
			expiresAt: time.Now().Add(c.ttl),
		}
		c.mu.Unlock()
	}

	ws, found := bySlug[keySlug]
	if !found {
		return Workspace{}, ErrNotFound
	}
	return ws, nil
}

// WorkspaceMetadata returns indexed workspace lookup maps, fetching and caching them for TTL.
// Partial metadata failures (e.g., /api/squads returning non-200) log a warning and keep empty non-nil maps.
func (c *MetadataCache) WorkspaceMetadata(ctx context.Context, pat, workspaceID string) (WorkspaceMeta, error) {
	now := time.Now()
	c.mu.RLock()
	entry, ok := c.meta[workspaceID]
	c.mu.RUnlock()
	if ok && now.Before(entry.expiresAt) {
		return entry.meta, nil
	}

	var (
		members  []Member
		agents   []Agent
		squads   []Squad
		projects []Project
		statuses []IssueStatus

		errMembers  error
		errAgents   error
		errSquads   error
		errProjects error
		errStatuses error
	)

	var wg sync.WaitGroup
	wg.Add(5)
	go func() {
		defer wg.Done()
		members, errMembers = c.client.ListMembers(ctx, pat, workspaceID)
	}()
	go func() {
		defer wg.Done()
		agents, errAgents = c.client.ListAgents(ctx, pat, workspaceID)
	}()
	go func() {
		defer wg.Done()
		squads, errSquads = c.client.ListSquads(ctx, pat, workspaceID)
	}()
	go func() {
		defer wg.Done()
		projects, errProjects = c.client.ListProjects(ctx, pat, workspaceID)
	}()
	go func() {
		defer wg.Done()
		statuses, errStatuses = c.client.ListIssueStatuses(ctx, pat, workspaceID)
	}()
	wg.Wait()

	errs := []error{errMembers, errAgents, errSquads, errProjects, errStatuses}
	for _, err := range errs {
		if errors.Is(err, ErrUnauthorized) {
			return WorkspaceMeta{}, err
		}
	}

	successCount := 0
	for _, err := range errs {
		if err == nil {
			successCount++
		}
	}
	if successCount == 0 {
		for _, err := range errs {
			if err != nil {
				return WorkspaceMeta{}, err
			}
		}
	}

	meta := WorkspaceMeta{
		MembersByUserID: make(map[string]Member, len(members)),
		AgentsByID:      make(map[string]Agent, len(agents)),
		SquadsByID:      make(map[string]Squad, len(squads)),
		ProjectsByID:    make(map[string]Project, len(projects)),
		StatusesByKey:   make(map[string]IssueStatus, len(statuses)),
	}

	if errMembers != nil {
		c.warnPartial(workspaceID, "members", errMembers)
	} else {
		for _, m := range members {
			key := m.UserID
			if key == "" {
				key = m.ID
			}
			if key != "" {
				meta.MembersByUserID[key] = m
			}
		}
	}

	if errAgents != nil {
		c.warnPartial(workspaceID, "agents", errAgents)
	} else {
		for _, a := range agents {
			if a.ID != "" {
				meta.AgentsByID[a.ID] = a
			}
		}
	}

	if errSquads != nil {
		c.warnPartial(workspaceID, "squads", errSquads)
	} else {
		for _, sq := range squads {
			if sq.ID != "" {
				meta.SquadsByID[sq.ID] = sq
			}
		}
	}

	if errProjects != nil {
		c.warnPartial(workspaceID, "projects", errProjects)
	} else {
		for _, p := range projects {
			if p.ID != "" {
				meta.ProjectsByID[p.ID] = p
			}
		}
	}

	if errStatuses != nil {
		c.warnPartial(workspaceID, "issue-statuses", errStatuses)
	} else {
		for _, st := range statuses {
			key := st.Key
			if key == "" {
				key = st.ID
			}
			if key != "" {
				meta.StatusesByKey[key] = st
			}
		}
	}

	c.mu.Lock()
	c.meta[workspaceID] = cachedMeta{
		meta:      meta,
		expiresAt: time.Now().Add(c.ttl),
	}
	c.mu.Unlock()

	return meta, nil
}

// InvalidateUser removes cached workspace slug mappings for slackUserID.
func (c *MetadataCache) InvalidateUser(slackUserID string) {
	c.mu.Lock()
	delete(c.workspaces, slackUserID)
	c.mu.Unlock()
}

func (c *MetadataCache) warnPartial(workspaceID, resource string, err error) {
	if c.logger == nil {
		return
	}
	c.logger.Warn("partial workspace metadata fetch failure",
		slog.String("workspace_id", workspaceID),
		slog.String("resource", resource),
		slog.String("error", err.Error()),
	)
}
