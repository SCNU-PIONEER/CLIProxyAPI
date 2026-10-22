package executor

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestConfigWithAuthProxyPrefersAuthProxy(t *testing.T) {
	cfg := &config.Config{SDKConfig: config.SDKConfig{ProxyURL: "http://cfg-proxy:8080"}}
	auth := &cliproxyauth.Auth{ProxyURL: "http://auth-proxy:9090"}

	got := configWithAuthProxy(cfg, auth)
	if got == nil {
		t.Fatal("nil config")
	}
	if got.ProxyURL != "http://auth-proxy:9090" {
		t.Fatalf("proxy = %q, want auth proxy", got.ProxyURL)
	}
	if cfg.ProxyURL != "http://cfg-proxy:8080" {
		t.Fatalf("original config mutated: proxy = %q", cfg.ProxyURL)
	}
}

func TestConfigWithAuthProxyKeepsConfigWithoutOverride(t *testing.T) {
	cfg := &config.Config{SDKConfig: config.SDKConfig{ProxyURL: "http://cfg-proxy:8080"}}
	if got := configWithAuthProxy(cfg, &cliproxyauth.Auth{}); got != cfg {
		t.Fatal("config should be returned unchanged when the auth has no proxy")
	}
	if got := configWithAuthProxy(cfg, nil); got != cfg {
		t.Fatal("config should be returned unchanged for nil auth")
	}
	if got := configWithAuthProxy(nil, &cliproxyauth.Auth{ProxyURL: "http://auth-proxy:9090"}); got != nil {
		t.Fatal("nil config should stay nil")
	}
}
