package gitlab

import (
	"strings"
	"testing"
)

func TestRedactResponseBodyForLogHidesSecrets(t *testing.T) {
	body := []byte(`{"access_token":"secret-marker-token","code":"secret-marker-code"}`)
	got := redactResponseBodyForLog(body)
	if strings.Contains(got, "secret-marker") {
		t.Fatalf("redacted body leaked secret material: %q", got)
	}
}
