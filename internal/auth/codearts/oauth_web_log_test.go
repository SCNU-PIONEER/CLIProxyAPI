package codearts

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
)

// TestHandleCallbackLogsDoNotContainSessionIdentifiers ensures callback logs
// describe session material by length only; ticket ids and identifiers are
// session-bound credentials.
func TestHandleCallbackLogsDoNotContainSessionIdentifiers(t *testing.T) {
	var logBuffer bytes.Buffer
	previousOut := log.StandardLogger().Out
	log.SetOutput(&logBuffer)
	defer log.SetOutput(previousOut)

	gin.SetMode(gin.TestMode)
	handler := &OAuthWebHandler{}
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	redirect := "https://codearts.example.com/callback?ticket_id=" + url.QueryEscape("secret-marker-ticket")
	target := "/callback?identifier=secret-marker-identifier&redirect=" + url.QueryEscape(redirect)
	ctx.Request = httptest.NewRequest(http.MethodGet, target, nil)

	handler.handleCallback(ctx)

	logs := logBuffer.String()
	if strings.Contains(logs, "secret-marker") {
		t.Fatalf("callback logs leaked session identifiers: %s", logs)
	}
	if !strings.Contains(logs, "identifier_len=") {
		t.Fatalf("callback log did not use a length-only summary: %s", logs)
	}
}
