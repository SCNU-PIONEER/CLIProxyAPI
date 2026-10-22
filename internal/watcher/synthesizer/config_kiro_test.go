package synthesizer

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/constant"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func newKiroSynthesisContext(cfg *config.Config, now time.Time) *SynthesisContext {
	return &SynthesisContext{
		Config:      cfg,
		Now:         now,
		IDGenerator: NewStableIDGenerator(),
	}
}

func kiroAuths(t *testing.T, cfg *config.Config, now time.Time) []*coreauth.Auth {
	t.Helper()
	auths, errSynthesize := NewConfigSynthesizer().Synthesize(newKiroSynthesisContext(cfg, now))
	if errSynthesize != nil {
		t.Fatalf("Synthesize() error = %v", errSynthesize)
	}
	return auths
}

func TestConfigSynthesizerKiroInlineCredentials(t *testing.T) {
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	cfg := &config.Config{
		KiroPreferredEndpoint: "cli",
		KiroKey: []config.KiroKey{{
			AccessToken:   "access-secret-value",
			RefreshToken:  "refresh-secret-value",
			ProfileArn:    "arn:aws:codewhisperer:us-east-1:123456789012:profile/EXAMPLE",
			Region:        "us-east-1",
			ProxyURL:      "http://127.0.0.1:8080",
			AgentTaskType: "dev",
		}},
	}

	auths := kiroAuths(t, cfg, now)
	if len(auths) != 1 {
		t.Fatalf("synthesized %d kiro auths, want 1", len(auths))
	}
	auth := auths[0]
	if auth.Provider != constant.Kiro {
		t.Fatalf("provider = %q, want %q", auth.Provider, constant.Kiro)
	}
	if auth.ProxyURL != "http://127.0.0.1:8080" {
		t.Fatalf("proxy = %q", auth.ProxyURL)
	}
	if auth.AuthKind() != coreauth.AuthKindOAuth {
		t.Fatalf("auth kind = %q, want oauth", auth.AuthKind())
	}
	if coreauth.IsConfigAPIKeyAuth(auth) {
		t.Fatal("kiro config credential must not be classified as a config API key")
	}
	for key, want := range map[string]any{
		"type":               "kiro",
		"access_token":       "access-secret-value",
		"refresh_token":      "refresh-secret-value",
		"profile_arn":        "arn:aws:codewhisperer:us-east-1:123456789012:profile/EXAMPLE",
		"region":             "us-east-1",
		"preferred_endpoint": "cli",
	} {
		got, ok := auth.Metadata[key]
		if !ok {
			if want == "" {
				continue
			}
			t.Fatalf("metadata[%q] missing", key)
		}
		if got != want {
			t.Fatalf("metadata[%q] = %v, want %v", key, got, want)
		}
	}
	if auth.Attributes["preferred_endpoint"] != "cli" {
		t.Fatalf("attributes preferred_endpoint = %q", auth.Attributes["preferred_endpoint"])
	}
	if auth.Attributes["agent_task_type"] != "dev" {
		t.Fatalf("attributes agent_task_type = %q", auth.Attributes["agent_task_type"])
	}
	if !strings.HasPrefix(auth.Attributes["source"], "config:kiro[") {
		t.Fatalf("source attribute = %q", auth.Attributes["source"])
	}
	if auth.Attributes["config_index"] != "0" {
		t.Fatalf("config_index = %q", auth.Attributes["config_index"])
	}
	if !auth.NextRefreshAfter.IsZero() {
		t.Fatalf("NextRefreshAfter = %v, want zero without an expiry", auth.NextRefreshAfter)
	}
	if strings.Contains(auth.ID, "access-secret-value") || strings.Contains(auth.ID, "refresh-secret-value") {
		t.Fatalf("auth ID leaks credential material: %q", auth.ID)
	}

	other := kiroAuths(t, cfg, now)
	if len(other) != 1 || other[0].ID != auth.ID {
		t.Fatalf("kiro config ID is not deterministic: %q vs %q", auth.ID, other[0].ID)
	}
}

func TestConfigSynthesizerKiroTokenFile(t *testing.T) {
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	tokenPath := filepath.Join(dir, "kiro-token.json")
	tokenJSON := `{"accessToken":"file-access","refreshToken":"file-refresh","profileArn":"arn:profile","region":"eu-west-1","startUrl":"https://example.awsapps.com/start","email":"file@example.com","authMethod":"idc"}`
	if errWrite := os.WriteFile(tokenPath, []byte(tokenJSON), 0o600); errWrite != nil {
		t.Fatalf("write token file: %v", errWrite)
	}
	cfg := &config.Config{
		KiroKey: []config.KiroKey{{
			TokenFile: tokenPath,
			Region:    "us-west-2", // inline value wins over the token file
		}},
	}

	auths := kiroAuths(t, cfg, now)
	if len(auths) != 1 {
		t.Fatalf("synthesized %d kiro auths, want 1", len(auths))
	}
	auth := auths[0]
	if got := auth.Metadata["access_token"]; got != "file-access" {
		t.Fatalf("access_token = %v", got)
	}
	if got := auth.Metadata["refresh_token"]; got != "file-refresh" {
		t.Fatalf("refresh_token = %v", got)
	}
	if got := auth.Metadata["region"]; got != "us-west-2" {
		t.Fatalf("region = %v, want inline override", got)
	}
	if got := auth.Metadata["start_url"]; got != "https://example.awsapps.com/start" {
		t.Fatalf("start_url = %v", got)
	}
	if auth.ID == "" || strings.Contains(auth.ID, "file-access") {
		t.Fatalf("auth ID = %q", auth.ID)
	}
	if !auth.NextRefreshAfter.IsZero() {
		t.Fatalf("NextRefreshAfter = %v, want zero without an expiry", auth.NextRefreshAfter)
	}
}

func TestConfigSynthesizerKiroTokenFileRefreshOnlyIsSynthesized(t *testing.T) {
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	tokenPath := filepath.Join(dir, "kiro-refresh.json")
	if errWrite := os.WriteFile(tokenPath, []byte(`{"refreshToken":"only-refresh","region":"us-east-1"}`), 0o600); errWrite != nil {
		t.Fatalf("write token file: %v", errWrite)
	}
	cfg := &config.Config{KiroKey: []config.KiroKey{{TokenFile: tokenPath}}}

	auths := kiroAuths(t, cfg, now)
	if len(auths) != 1 {
		t.Fatalf("synthesized %d kiro auths, want refresh-only entry to be kept", len(auths))
	}
	if auths[0].Metadata["refresh_token"] != "only-refresh" {
		t.Fatalf("refresh_token = %v", auths[0].Metadata["refresh_token"])
	}
	if !auths[0].NextRefreshAfter.Equal(now) {
		t.Fatalf("NextRefreshAfter = %v, want immediate refresh for refresh-only credential", auths[0].NextRefreshAfter)
	}
}

func TestConfigSynthesizerKiroSkipsUnusableEntries(t *testing.T) {
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	cfg := &config.Config{
		KiroKey: []config.KiroKey{
			{},
			{TokenFile: filepath.Join(t.TempDir(), "missing.json"), AccessToken: ""},
		},
	}
	if auths := kiroAuths(t, cfg, now); len(auths) != 0 {
		t.Fatalf("synthesized %d auths for unusable entries, want 0", len(auths))
	}

	cfg = &config.Config{KiroKey: []config.KiroKey{{TokenFile: filepath.Join(t.TempDir(), "missing.json"), AccessToken: "inline-access"}}}
	auths := kiroAuths(t, cfg, now)
	if len(auths) != 1 {
		t.Fatalf("synthesized %d auths, want inline fallback to survive a missing token file", len(auths))
	}
	if auths[0].Metadata["access_token"] != "inline-access" {
		t.Fatalf("access_token = %v", auths[0].Metadata["access_token"])
	}
}
