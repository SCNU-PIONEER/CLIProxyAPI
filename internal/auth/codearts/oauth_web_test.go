package codearts

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

// TestHandleCallbackDoesNotRedirectToCallerSuppliedURL ensures the OAuth
// callback cannot be used as an open redirect.
func TestHandleCallbackDoesNotRedirectToCallerSuppliedURL(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := &OAuthWebHandler{}
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/callback?identifier=id&redirect=https%3A%2F%2Fevil.example%2F", nil)

	h.handleCallback(c)

	if rec.Code == http.StatusTemporaryRedirect || rec.Code == http.StatusFound {
		t.Fatalf("callback must not redirect to a caller-supplied URL, got status %d", rec.Code)
	}
}

// TestSanitizeFileNamePartRejectsPathSeparators ensures a provider-supplied
// user name cannot escape the auth directory.
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
