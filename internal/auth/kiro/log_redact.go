package kiro

import "fmt"

// redactResponseBodyForLog describes a response body without echoing it.
// Token and userinfo endpoints return access/refresh tokens in these bodies.
func redactResponseBodyForLog(body []byte) string {
	return fmt.Sprintf("<redacted %d bytes>", len(body))
}
