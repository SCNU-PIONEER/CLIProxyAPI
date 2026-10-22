package util

import (
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

func TestNewHTTPClientWithProxyPrefersAuthOverride(t *testing.T) {
	cfg := &config.SDKConfig{ProxyURL: "http://cfg-proxy:8080"}
	client := NewHTTPClientWithProxy(cfg, "http://auth-proxy:9090", 10*time.Second)
	if client == nil {
		t.Fatal("nil client")
	}
	transport, ok := client.Transport.(*http.Transport)
	if !ok || transport.Proxy == nil {
		t.Fatalf("transport = %T, want proxy-aware *http.Transport", client.Transport)
	}
	req := &http.Request{URL: &url.URL{Scheme: "https", Host: "example.com"}}
	proxyURL, err := transport.Proxy(req)
	if err != nil {
		t.Fatalf("proxy lookup: %v", err)
	}
	if proxyURL == nil || proxyURL.String() != "http://auth-proxy:9090" {
		t.Fatalf("proxy = %v, want auth-specific proxy", proxyURL)
	}
}

func TestNewHTTPClientWithProxyFallsBackToConfig(t *testing.T) {
	cfg := &config.SDKConfig{ProxyURL: "http://cfg-proxy:8080"}
	client := NewHTTPClientWithProxy(cfg, "", 10*time.Second)
	transport, ok := client.Transport.(*http.Transport)
	if !ok || transport.Proxy == nil {
		t.Fatalf("transport = %T, want proxy-aware *http.Transport", client.Transport)
	}
	proxyURL, err := transport.Proxy(&http.Request{URL: &url.URL{Scheme: "https", Host: "example.com"}})
	if err != nil {
		t.Fatalf("proxy lookup: %v", err)
	}
	if proxyURL == nil || proxyURL.String() != "http://cfg-proxy:8080" {
		t.Fatalf("proxy = %v, want config proxy fallback", proxyURL)
	}
}

func TestNewHTTPClientWithProxyWithoutProxy(t *testing.T) {
	client := NewHTTPClientWithProxy(nil, "", 10*time.Second)
	if client.Timeout != 10*time.Second {
		t.Fatalf("timeout = %s, want 10s", client.Timeout)
	}
	if transport, ok := client.Transport.(*http.Transport); ok && transport.Proxy != nil {
		t.Fatal("unexpected proxy configured without proxy settings")
	}
}
