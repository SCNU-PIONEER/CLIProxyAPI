package auth

import (
	"fmt"
	"net/url"
	"strings"
)

// parseCallbackURLLike accepts either a full callback URL or a bare query string
// (the form a browser may leave in the address bar).
func parseCallbackURLLike(raw string) (*url.URL, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil, fmt.Errorf("empty callback URL")
	}
	if !strings.Contains(trimmed, "://") {
		trimmed = "http://callback.invalid/?" + strings.TrimPrefix(trimmed, "?")
	}
	parsed, err := url.Parse(trimmed)
	if err != nil {
		return nil, fmt.Errorf("invalid callback URL")
	}
	if parsed.RawQuery == "" {
		if index := strings.Index(parsed.Opaque, "?"); index >= 0 {
			parsed.RawQuery = parsed.Opaque[index+1:]
		}
	}
	if parsed.RawQuery == "" {
		return nil, fmt.Errorf("callback URL has no query parameters")
	}
	return parsed, nil
}
