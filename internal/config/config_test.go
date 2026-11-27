package config_test

import (
	"encoding/base64"
	"strings"
	"testing"

	"github.com/everlightlabs/multica-slack/internal/config"
)

func validBaseEnv() map[string]string {
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 1)
	}
	return map[string]string{
		"PUBLIC_BASE_URL":      "https://multica-slack.example.com/",
		"ENCRYPTION_KEY":       base64.StdEncoding.EncodeToString(key),
		"SLACK_BOT_TOKEN":      "xoxb-test-token",
		"SLACK_SIGNING_SECRET": "test-signing-secret",
		"SLACK_CLIENT_ID":      "12345.67890",
		"SLACK_CLIENT_SECRET":  "test-client-secret",
	}
}

func mapLookup(m map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) {
		v, ok := m[k]
		return v, ok
	}
}

func TestLoadValidDefaults(t *testing.T) {
	env := validBaseEnv()
	cfg, err := config.Load(mapLookup(env))
	if err != nil {
		t.Fatalf("Load() unexpected error: %v", err)
	}

	if cfg.Port != 3020 {
		t.Errorf("Port = %d, want 3020", cfg.Port)
	}
	if cfg.DataDir != "/data" {
		t.Errorf("DataDir = %q, want %q", cfg.DataDir, "/data")
	}
	if cfg.PublicBaseURL != "https://multica-slack.example.com" {
		t.Errorf("PublicBaseURL = %q, want trailing slash stripped", cfg.PublicBaseURL)
	}
	if cfg.MulticaAPIURL != "https://api.multica.ai" {
		t.Errorf("MulticaAPIURL = %q, want %q", cfg.MulticaAPIURL, "https://api.multica.ai")
	}
	if cfg.MulticaAppURL != "https://multica.ai" {
		t.Errorf("MulticaAppURL = %q, want %q", cfg.MulticaAppURL, "https://multica.ai")
	}
	if len(cfg.EncryptionKey) != 32 {
		t.Errorf("len(EncryptionKey) = %d, want 32", len(cfg.EncryptionKey))
	}
	if cfg.SlackBotToken != "xoxb-test-token" {
		t.Errorf("SlackBotToken = %q, want %q", cfg.SlackBotToken, "xoxb-test-token")
	}
	if cfg.SlackSigningSecret != "test-signing-secret" {
		t.Errorf("SlackSigningSecret = %q, want %q", cfg.SlackSigningSecret, "test-signing-secret")
	}
	if cfg.SlackClientID != "12345.67890" {
		t.Errorf("SlackClientID = %q, want %q", cfg.SlackClientID, "12345.67890")
	}
	if cfg.SlackClientSecret != "test-client-secret" {
		t.Errorf("SlackClientSecret = %q, want %q", cfg.SlackClientSecret, "test-client-secret")
	}

	// Verify custom overrides and trailing slash stripping on Multica URLs.
	env["PORT"] = "8080"
	env["DATA_DIR"] = "/var/lib/multica-slack"
	env["MULTICA_API_URL"] = "https://api.staging.multica.ai///"
	env["MULTICA_APP_URL"] = "https://staging.multica.ai/"
	env["SLACK_ALLOWED_TEAM_ID"] = "T12345"
	env["MULTICA_DEFAULT_WORKSPACE_SLUG"] = "acme"
	env["MULTICA_FALLBACK_PAT"] = "mul_fallback"
	env["VERSION"] = "v1.2.3"

	cfg, err = config.Load(mapLookup(env))
	if err != nil {
		t.Fatalf("Load() with overrides unexpected error: %v", err)
	}
	if cfg.Port != 8080 {
		t.Errorf("Port = %d, want 8080", cfg.Port)
	}
	if cfg.DataDir != "/var/lib/multica-slack" {
		t.Errorf("DataDir = %q, want %q", cfg.DataDir, "/var/lib/multica-slack")
	}
	if cfg.MulticaAPIURL != "https://api.staging.multica.ai" {
		t.Errorf("MulticaAPIURL = %q, want trailing slashes stripped", cfg.MulticaAPIURL)
	}
	if cfg.MulticaAppURL != "https://staging.multica.ai" {
		t.Errorf("MulticaAppURL = %q, want trailing slashes stripped", cfg.MulticaAppURL)
	}
	if cfg.SlackAllowedTeamID != "T12345" {
		t.Errorf("SlackAllowedTeamID = %q, want %q", cfg.SlackAllowedTeamID, "T12345")
	}
	if cfg.MulticaDefaultWorkspaceSlug != "acme" {
		t.Errorf("MulticaDefaultWorkspaceSlug = %q, want %q", cfg.MulticaDefaultWorkspaceSlug, "acme")
	}
	if cfg.MulticaFallbackPAT != "mul_fallback" {
		t.Errorf("MulticaFallbackPAT = %q, want %q", cfg.MulticaFallbackPAT, "mul_fallback")
	}
	if cfg.Version != "v1.2.3" {
		t.Errorf("Version = %q, want %q", cfg.Version, "v1.2.3")
	}
}

func TestLoadRejectsBadEncryptionKey(t *testing.T) {
	tests := []struct {
		name string
		val  *string
	}{
		{name: "missing", val: nil},
		{name: "empty", val: strPtr("")},
		{name: "non-base64", val: strPtr("not-valid-base64!!!")},
		{name: "16-byte-key", val: strPtr(base64.StdEncoding.EncodeToString(make([]byte, 16)))},
		{name: "64-byte-key", val: strPtr(base64.StdEncoding.EncodeToString(make([]byte, 64)))},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			env := validBaseEnv()
			if tc.val == nil {
				delete(env, "ENCRYPTION_KEY")
			} else {
				env["ENCRYPTION_KEY"] = *tc.val
			}
			_, err := config.Load(mapLookup(env))
			if err == nil {
				t.Fatalf("Load() expected error for %s ENCRYPTION_KEY, got nil", tc.name)
			}
		})
	}
}

func TestLoadRejectsMissingRequired(t *testing.T) {
	required := []string{
		"PUBLIC_BASE_URL",
		"SLACK_BOT_TOKEN",
		"SLACK_SIGNING_SECRET",
		"SLACK_CLIENT_ID",
		"SLACK_CLIENT_SECRET",
	}

	for _, key := range required {
		t.Run("missing_"+key, func(t *testing.T) {
			env := validBaseEnv()
			delete(env, key)
			_, err := config.Load(mapLookup(env))
			if err == nil {
				t.Fatalf("Load() expected error when %s is missing, got nil", key)
			}
			if !strings.Contains(err.Error(), key) {
				t.Errorf("error %q should mention missing key %s", err.Error(), key)
			}
		})

		t.Run("empty_"+key, func(t *testing.T) {
			env := validBaseEnv()
			env[key] = "   "
			_, err := config.Load(mapLookup(env))
			if err == nil {
				t.Fatalf("Load() expected error when %s is empty, got nil", key)
			}
		})
	}
}

func TestLoadRejectsInvalidPortAndURL(t *testing.T) {
	t.Run("non-numeric port", func(t *testing.T) {
		env := validBaseEnv()
		env["PORT"] = "abc"
		if _, err := config.Load(mapLookup(env)); err == nil {
			t.Fatal("expected error for non-numeric PORT")
		}
	})

	t.Run("out-of-range port", func(t *testing.T) {
		env := validBaseEnv()
		env["PORT"] = "70000"
		if _, err := config.Load(mapLookup(env)); err == nil {
			t.Fatal("expected error for out-of-range PORT")
		}
	})

	t.Run("invalid PUBLIC_BASE_URL", func(t *testing.T) {
		env := validBaseEnv()
		env["PUBLIC_BASE_URL"] = "not-a-url"
		if _, err := config.Load(mapLookup(env)); err == nil {
			t.Fatal("expected error for invalid PUBLIC_BASE_URL")
		}
	})
}

func strPtr(s string) *string {
	return &s
}
