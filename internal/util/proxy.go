// Package util provides utility functions for the CLI Proxy API server.
// It includes helper functions for proxy configuration, HTTP client setup,
// log level management, and other common operations used across the application.
package util

import (
	"net/http"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/proxyutil"
	log "github.com/sirupsen/logrus"
)

// SetProxy configures the provided HTTP client with proxy settings from the configuration.
// It supports SOCKS5, HTTP, and HTTPS proxies. The function modifies the client's transport
// to route requests through the configured proxy server.
func SetProxy(cfg *config.SDKConfig, httpClient *http.Client) *http.Client {
	if cfg == nil || httpClient == nil {
		return httpClient
	}

	transport, _, errBuild := proxyutil.BuildHTTPTransport(cfg.ProxyURL)
	if errBuild != nil {
		log.Errorf("%v", errBuild)
	}
	if transport != nil {
		httpClient.Transport = transport
	}
	return httpClient
}

// NewHTTPClientWithProxy builds a client for a single credential operation.
// A non-empty proxyOverride (for example an auth-specific ProxyURL) takes
// precedence over cfg.ProxyURL so per-auth proxy settings apply to token
// refresh and credential acquisition instead of only to request execution.
func NewHTTPClientWithProxy(cfg *config.SDKConfig, proxyOverride string, timeout time.Duration) *http.Client {
	client := &http.Client{Timeout: timeout}
	proxyURL := strings.TrimSpace(proxyOverride)
	if proxyURL == "" && cfg != nil {
		proxyURL = strings.TrimSpace(cfg.ProxyURL)
	}
	if proxyURL == "" {
		return client
	}
	transport, _, errBuild := proxyutil.BuildHTTPTransport(proxyURL)
	if errBuild != nil {
		log.Errorf("%v", errBuild)
		return client
	}
	if transport != nil {
		client.Transport = transport
	}
	return client
}
