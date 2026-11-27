// Package unfurl implements Multica link matching, Slack card formatting,
// and privacy-safe link unfurling for Slack link_shared events.
package unfurl

import (
	"net/url"
	"strings"
)

const defaultAppBaseURL = "https://multica.ai"

// ResourceKind identifies the type of Multica resource referenced by a URL.
type ResourceKind string

const (
	KindIssue   ResourceKind = "issue"
	KindProject ResourceKind = "project"
	KindDoc     ResourceKind = "doc"
)

// Match describes a recognized Multica Cloud resource URL.
type Match struct {
	RawURL        string
	Kind          ResourceKind
	WorkspaceSlug string
	ResourceID    string
}

// MatchURL parses rawURL against appBaseURL and extracts a supported Multica resource match.
// Query parameters, fragments, and trailing slashes are ignored when matching; RawURL preserves rawURL.
func MatchURL(appBaseURL, rawURL string) (Match, bool) {
	baseRaw := strings.TrimSpace(appBaseURL)
	if baseRaw == "" {
		baseRaw = defaultAppBaseURL
	}
	baseRaw = strings.TrimRight(baseRaw, "/")

	baseParsed, err := url.Parse(baseRaw)
	if err != nil || baseParsed.Scheme == "" || baseParsed.Host == "" {
		return Match{}, false
	}

	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return Match{}, false
	}

	if !strings.EqualFold(parsed.Scheme, baseParsed.Scheme) || !strings.EqualFold(parsed.Host, baseParsed.Host) {
		return Match{}, false
	}

	basePath := strings.Trim(baseParsed.Path, "/")
	relPath := strings.Trim(parsed.Path, "/")
	if basePath != "" {
		if relPath == basePath || !strings.HasPrefix(relPath, basePath+"/") {
			return Match{}, false
		}
		relPath = strings.TrimPrefix(relPath, basePath+"/")
	}

	if relPath == "" {
		return Match{}, false
	}

	parts := strings.Split(relPath, "/")
	if len(parts) != 3 {
		return Match{}, false
	}
	slug, section, resourceID := parts[0], parts[1], parts[2]
	if slug == "" || resourceID == "" {
		return Match{}, false
	}

	var kind ResourceKind
	switch section {
	case "issues":
		kind = KindIssue
	case "projects":
		kind = KindProject
	case "docs":
		kind = KindDoc
	default:
		return Match{}, false
	}

	return Match{
		RawURL:        rawURL,
		Kind:          kind,
		WorkspaceSlug: slug,
		ResourceID:    resourceID,
	}, true
}
