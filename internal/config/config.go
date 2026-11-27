package config

import (
	"encoding/base64"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
)

const (
	defaultPort          = 3020
	defaultDataDir       = "/data"
	defaultMulticaAPIURL = "https://api.multica.ai"
	defaultMulticaAppURL = "https://multica.ai"
	defaultVersion       = "dev"
	encryptionKeyBytes   = 32
)

// Config holds runtime configuration loaded from environment variables.
type Config struct {
	Port                        int
	PublicBaseURL               string
	DataDir                     string
	EncryptionKey               []byte
	SlackBotToken               string
	SlackSigningSecret          string
	SlackClientID               string
	SlackClientSecret           string
	SlackAllowedTeamID          string
	MulticaAPIURL               string
	MulticaAppURL               string
	MulticaDefaultWorkspaceSlug string
	MulticaFallbackPAT          string
	Version                     string
}

// Load reads and validates configuration using lookup (or os.LookupEnv if nil).
func Load(lookup func(string) (string, bool)) (Config, error) {
	if lookup == nil {
		lookup = os.LookupEnv
	}

	get := func(key string) string {
		v, ok := lookup(key)
		if !ok {
			return ""
		}
		return strings.TrimSpace(v)
	}

	require := func(key string) (string, error) {
		v := get(key)
		if v == "" {
			return "", fmt.Errorf("config: missing required environment variable %s", key)
		}
		return v, nil
	}

	defaultOr := func(key, fallback string) string {
		if v := get(key); v != "" {
			return v
		}
		return fallback
	}

	port := defaultPort
	if rawPort := get("PORT"); rawPort != "" {
		parsed, err := strconv.Atoi(rawPort)
		if err != nil || parsed < 1 || parsed > 65535 {
			return Config{}, fmt.Errorf("config: invalid PORT %q", rawPort)
		}
		port = parsed
	}

	rawPublicURL, err := require("PUBLIC_BASE_URL")
	if err != nil {
		return Config{}, err
	}
	publicBaseURL, err := normalizeHTTPURL("PUBLIC_BASE_URL", rawPublicURL)
	if err != nil {
		return Config{}, err
	}

	rawEncKey, err := require("ENCRYPTION_KEY")
	if err != nil {
		return Config{}, err
	}
	encKey, err := base64.StdEncoding.DecodeString(rawEncKey)
	if err != nil {
		return Config{}, fmt.Errorf("config: ENCRYPTION_KEY must be valid base64: %w", err)
	}
	if len(encKey) != encryptionKeyBytes {
		return Config{}, fmt.Errorf("config: ENCRYPTION_KEY must decode to %d bytes, got %d", encryptionKeyBytes, len(encKey))
	}

	slackBotToken, err := require("SLACK_BOT_TOKEN")
	if err != nil {
		return Config{}, err
	}
	slackSigningSecret, err := require("SLACK_SIGNING_SECRET")
	if err != nil {
		return Config{}, err
	}
	slackClientID, err := require("SLACK_CLIENT_ID")
	if err != nil {
		return Config{}, err
	}
	slackClientSecret, err := require("SLACK_CLIENT_SECRET")
	if err != nil {
		return Config{}, err
	}

	multicaAPIURL, err := normalizeHTTPURL("MULTICA_API_URL", defaultOr("MULTICA_API_URL", defaultMulticaAPIURL))
	if err != nil {
		return Config{}, err
	}
	multicaAppURL, err := normalizeHTTPURL("MULTICA_APP_URL", defaultOr("MULTICA_APP_URL", defaultMulticaAppURL))
	if err != nil {
		return Config{}, err
	}

	return Config{
		Port:                        port,
		PublicBaseURL:               publicBaseURL,
		DataDir:                     defaultOr("DATA_DIR", defaultDataDir),
		EncryptionKey:               encKey,
		SlackBotToken:               slackBotToken,
		SlackSigningSecret:          slackSigningSecret,
		SlackClientID:               slackClientID,
		SlackClientSecret:           slackClientSecret,
		SlackAllowedTeamID:          get("SLACK_ALLOWED_TEAM_ID"),
		MulticaAPIURL:               multicaAPIURL,
		MulticaAppURL:               multicaAppURL,
		MulticaDefaultWorkspaceSlug: get("MULTICA_DEFAULT_WORKSPACE_SLUG"),
		MulticaFallbackPAT:          get("MULTICA_FALLBACK_PAT"),
		Version:                     defaultOr("VERSION", defaultVersion),
	}, nil
}

func normalizeHTTPURL(name, raw string) (string, error) {
	trimmed := strings.TrimRight(raw, "/")
	parsed, err := url.Parse(trimmed)
	if err != nil {
		return "", fmt.Errorf("config: invalid %s: %w", name, err)
	}
	if (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return "", fmt.Errorf("config: %s must be an http(s) URL with a host", name)
	}
	return trimmed, nil
}
