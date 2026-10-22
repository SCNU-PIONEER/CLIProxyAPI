package executor

import (
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// configWithAuthProxy returns a config whose proxy URL prefers the auth-specific
// ProxyURL over the global SDK proxy. Refresh constructors in the Plus executors
// read config.ProxyURL, so this keeps per-auth proxy settings effective for
// token refresh as well as for request execution.
func configWithAuthProxy(cfg *config.Config, auth *cliproxyauth.Auth) *config.Config {
	if cfg == nil {
		return nil
	}
	proxyURL := ""
	if auth != nil {
		proxyURL = strings.TrimSpace(auth.ProxyURL)
	}
	if proxyURL == "" || strings.TrimSpace(cfg.ProxyURL) == proxyURL {
		return cfg
	}
	clone := cfg.CloneForRuntime()
	if clone == nil {
		return cfg
	}
	clone.ProxyURL = proxyURL
	return clone
}
