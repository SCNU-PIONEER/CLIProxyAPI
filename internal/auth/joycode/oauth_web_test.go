package joycode

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

// TestHandleCallbackRequiresAuthKey ensures a callback cannot complete a login
// without the authKey generated for that attempt (state binding).
func TestHandleCallbackRequiresAuthKey(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := &OAuthWebHandler{}
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/joycode/callback?pt_key=pt-value", nil)

	h.handleCallback(c)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("callback without authKey must be rejected, got status %d", rec.Code)
	}
}

// TestSanitizeFileNamePartRejectsPathSeparators ensures a provider-supplied
// user id cannot escape the auth directory.
func TestSanitizeFileNamePartRejectsPathSeparators(t *testing.T) {
	for _, input := range []string{"../../outside", `..\..\outside`, "a/b", `a\b`, ".."} {
		got := sanitizeFileNamePart(input)
		if strings.ContainsAny(got, `/\`) {
			t.Fatalf("sanitizeFileNamePart(%q) = %q contains a path separator", input, got)
		}
		if strings.HasPrefix(got, ".") {
			t.Fatalf("sanitizeFileNamePart(%q) = %q must not start with a dot", input, got)
		}
		if filepath.Base(got) != got {
			t.Fatalf("sanitizeFileNamePart(%q) = %q is not a base name", input, got)
		}
	}
}
