package codearts

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

// TestHandleCallbackDoesNotFallBackToLatestWaitingSession ensures a callback
// that does not carry the ticket_id of a registered session is not attached to
// whichever session happens to be waiting.
func TestHandleCallbackDoesNotFallBackToLatestWaitingSession(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := NewOAuthWebHandler(nil)
	sess := &webSession{
		stateID:   "state-1",
		ticketID:  "ticket-1",
		status:    sWaitingCB,
		startedAt: time.Now(),
	}
	h.sessions[sess.stateID] = sess
	h.ticketToState[sess.ticketID] = sess.stateID

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/callback?identifier=someone", nil)

	h.handleCallback(c)

	if sess.status != sWaitingCB {
		t.Fatalf("unrelated callback hijacked waiting session: status = %q, want %q", sess.status, sWaitingCB)
	}
	if sess.identifier != "" {
		t.Fatalf("unrelated callback set identifier = %q, want empty", sess.identifier)
	}
}

// TestHandleCallbackIgnoresUnknownTicket ensures a forged ticket_id cannot
// attach the callback to a waiting session.
func TestHandleCallbackIgnoresUnknownTicket(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := NewOAuthWebHandler(nil)
	sess := &webSession{
		stateID:   "state-1",
		ticketID:  "ticket-1",
		status:    sWaitingCB,
		startedAt: time.Now(),
	}
	h.sessions[sess.stateID] = sess
	h.ticketToState[sess.ticketID] = sess.stateID

	redirect := url.QueryEscape("https://h/return?ticket_id=unknown")
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/callback?identifier=someone&redirect="+redirect, nil)

	h.handleCallback(c)

	if sess.status != sWaitingCB {
		t.Fatalf("forged ticket matched session: status = %q, want %q", sess.status, sWaitingCB)
	}
}
