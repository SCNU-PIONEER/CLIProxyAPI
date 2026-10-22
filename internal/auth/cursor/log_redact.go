package cursor

import "fmt"

// redactResponseBodyForLog describes a response body without echoing it:
// provider responses can contain access/refresh tokens or authorization codes.
func redactResponseBodyForLog(body []byte) string {
	return fmt.Sprintf("<redacted %d bytes>", len(body))
}
