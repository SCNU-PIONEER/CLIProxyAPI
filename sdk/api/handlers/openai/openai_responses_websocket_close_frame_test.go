package openai

import (
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

// TestResponsesWebsocketCloseForUpstreamErrorSendsFrameWhileWriterBusy captures
// a real downstream socket so the close-frame path is exercised end to end. A
// concurrent data or ping writer owns writeMu when an upstream replay-required
// failure arrives; the client must still observe the 1012 close frame, because
// that code is the only signal telling it to replay the turn over HTTP. Closing
// the transport without the frame turns the session into an abnormal closure
// (1006) and the client cannot recover.
func TestResponsesWebsocketCloseForUpstreamErrorSendsFrameWhileWriterBusy(t *testing.T) {
	gin.SetMode(gin.TestMode)

	serverConns := make(chan *websocket.Conn, 1)
	router := gin.New()
	router.GET("/ws", func(c *gin.Context) {
		conn, errUpgrade := responsesWebsocketUpgrader.Upgrade(c.Writer, c.Request, nil)
		if errUpgrade != nil {
			return
		}
		serverConns <- conn
		for {
			if _, _, errRead := conn.ReadMessage(); errRead != nil {
				return
			}
		}
	})
	server := httptest.NewServer(router)
	defer server.Close()

	clientConn, _, errDial := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/ws", nil)
	if errDial != nil {
		t.Fatalf("dial downstream websocket: %v", errDial)
	}
	defer func() { _ = clientConn.Close() }()

	var serverConn *websocket.Conn
	select {
	case serverConn = <-serverConns:
	case <-time.After(5 * time.Second):
		t.Fatal("server did not upgrade the downstream websocket")
	}

	writer := newResponsesWebsocketWriter(serverConn)
	// Simulate an in-flight downstream write owning the writer mutex when the
	// upstream replay signal arrives.
	writer.writeMu.Lock()
	matched, errClose := writer.closeForUpstreamError(cliproxyexecutor.NewUpstreamWebsocketReplayRequiredError())
	writer.writeMu.Unlock()
	if !matched {
		t.Fatalf("replay-required error was not matched as a close-frame error: %v", errClose)
	}

	_ = clientConn.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, _, errRead := clientConn.ReadMessage()
	var closeErr *websocket.CloseError
	if !errors.As(errRead, &closeErr) {
		t.Fatalf("downstream read = %v, want a websocket close frame", errRead)
	}
	if closeErr.Code != websocket.CloseServiceRestart {
		t.Fatalf("downstream close code = %d, want %d", closeErr.Code, websocket.CloseServiceRestart)
	}
	if closeErr.Text != wsHTTPReplayRequiredCloseReason {
		t.Fatalf("downstream close reason = %q, want %q", closeErr.Text, wsHTTPReplayRequiredCloseReason)
	}
}
