package joycode

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

// TestHandleCallbackRejectsUnknownAuthKey ensures a callback carrying an
// authKey that belongs to no session is rejected instead of being attached to
// the most recent waiting session.
func TestHandleCallbackRejectsUnknownAuthKey(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := NewOAuthWebHandler(nil)
	sess := &jcWebSession{
		stateID:   "state-1",
		authKey:   "expected-auth-key",
		status:    jcWaiting,
		startedAt: time.Now(),
	}
	h.sessions[sess.stateID] = sess

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/callback?pt_key=pt&authKey=wrong-auth-key", nil)

	h.handleCallback(c)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("callback with unknown authKey returned %d, want 403", rec.Code)
	}
	if sess.status != jcWaiting {
		t.Fatalf("unrelated callback hijacked waiting session: status = %q, want %q", sess.status, jcWaiting)
	}
}

// TestHandleCallbackRejectsMissingAuthKey ensures the waiting-session fallback
// cannot be reached by omitting authKey entirely.
func TestHandleCallbackRejectsMissingAuthKey(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := NewOAuthWebHandler(nil)
	sess := &jcWebSession{
		stateID:   "state-1",
		authKey:   "expected-auth-key",
		status:    jcWaiting,
		startedAt: time.Now(),
	}
	h.sessions[sess.stateID] = sess

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/callback?pt_key=pt", nil)

	h.handleCallback(c)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("callback without authKey returned %d, want 403", rec.Code)
	}
	if sess.status != jcWaiting {
		t.Fatalf("callback without authKey touched waiting session: status = %q, want %q", sess.status, jcWaiting)
	}
}
